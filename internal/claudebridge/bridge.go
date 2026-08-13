// Package claudebridge supervises one versioned Claude SDK bridge process per
// Loom Agent. Runtime semantics remain in Hub; this package owns only stdio and
// process lifecycle.
package claudebridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yan5xu/codex-loom/internal/claudegen"
)

const (
	maxFrameBytes  = 1 << 20
	maxStderrBytes = 8 << 10
	closeTimeout   = 2 * time.Second
	closeGrace     = 250 * time.Millisecond
)

type LaunchSpec struct {
	NodePath   string
	BridgePath string
	Args       []string // Tests only; installed generations leave this empty.
	Manifest   claudegen.Manifest
}

type DriverOptions struct {
	ResolveActive func(context.Context) (LaunchSpec, error)
	NextID        func() string
	OnEvent       func(Event)
	OnDiagnostic  func(string)
	OnFailure     func(string, error)
}

type Driver struct {
	options DriverOptions

	mu       sync.Mutex
	handles  map[string]*Bridge
	shutdown bool
	once     sync.Once
	ids      atomic.Uint64
}

type Command struct {
	Kind      string
	TurnID    string
	Operation string
	Payload   any
}

type Response struct {
	RequestID string          `json:"requestId"`
	TurnID    string          `json:"turnId,omitempty"`
	Operation string          `json:"operation,omitempty"`
	Accepted  bool            `json:"accepted"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type Event struct {
	AgentID   string          `json:"-"`
	Kind      string          `json:"event"`
	TurnID    string          `json:"turnId,omitempty"`
	Operation string          `json:"operation,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type IndeterminateError struct {
	Operation string
	Cause     error
}

func (e *IndeterminateError) Error() string {
	return fmt.Sprintf("Claude bridge operation %s has an Indeterminate Runtime Outcome: %v", e.Operation, e.Cause)
}

func (e *IndeterminateError) Unwrap() error { return e.Cause }

type pendingRequest struct {
	operation string
	turnID    string
	result    chan requestResult
}

type operationState struct {
	turnID   string
	accepted bool
	terminal bool
}

type requestResult struct {
	response Response
	err      error
}

type eventDelivery struct {
	event Event
	ack   chan struct{}
}

type Bridge struct {
	agentID      string
	generationID string
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	reader       *bufio.Reader

	onEvent      func(Event)
	onDiagnostic func(string)
	onFailure    func(string, error)
	nextID       func() string
	events       chan eventDelivery
	stop         chan struct{}

	mu           sync.Mutex
	pending      map[string]pendingRequest
	operations   map[string]operationState
	terminalSeen bool
	failed       error
	closing      bool

	writeMu    sync.Mutex
	opMu       sync.Mutex
	closeOnce  sync.Once
	failOnce   sync.Once
	stopOnce   sync.Once
	done       chan struct{}
	readerDone chan struct{}
	stderr     *boundedStderr
}

func NewDriver(options DriverOptions) *Driver {
	d := &Driver{options: options, handles: map[string]*Bridge{}}
	if d.options.NextID == nil {
		d.options.NextID = func() string { return fmt.Sprintf("loom-%d", d.ids.Add(1)) }
	}
	return d
}

func (d *Driver) Acquire(ctx context.Context, agentID string) (*Bridge, error) {
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("Claude bridge Agent ID is required")
	}
	if d.options.ResolveActive == nil {
		return nil, errors.New("Claude bridge active generation resolver is required")
	}
	d.mu.Lock()
	if d.shutdown {
		d.mu.Unlock()
		return nil, errors.New("Claude bridge Driver is shut down")
	}
	if existing := d.handles[agentID]; existing != nil && existing.Alive() {
		d.mu.Unlock()
		return existing, nil
	}
	d.mu.Unlock()

	spec, err := d.options.ResolveActive(ctx)
	if err != nil {
		return nil, err
	}
	bridge, err := start(ctx, agentID, spec, d.options.NextID, d.options.OnEvent, d.options.OnDiagnostic, d.options.OnFailure)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shutdown {
		bridge.Close()
		return nil, errors.New("Claude bridge Driver is shut down")
	}
	if existing := d.handles[agentID]; existing != nil && existing.Alive() {
		bridge.Close()
		return existing, nil
	}
	d.handles[agentID] = bridge
	return bridge, nil
}

func (d *Driver) Shutdown(context.Context) error {
	if d == nil {
		return nil
	}
	d.once.Do(func() {
		d.mu.Lock()
		d.shutdown = true
		handles := make([]*Bridge, 0, len(d.handles))
		for _, bridge := range d.handles {
			handles = append(handles, bridge)
		}
		d.mu.Unlock()
		for _, bridge := range handles {
			bridge.Close()
		}
	})
	return nil
}

func start(ctx context.Context, agentID string, spec LaunchSpec, nextID func() string, onEvent func(Event), onDiagnostic func(string), onFailure func(string, error)) (*Bridge, error) {
	if spec.NodePath == "" || spec.BridgePath == "" {
		return nil, errors.New("Claude bridge launch paths are required")
	}
	args := append([]string{spec.BridgePath}, spec.Args...)
	cmd := exec.Command(spec.NodePath, args...)
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open Claude bridge stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open Claude bridge stdout: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open Claude bridge stderr: %w", err)
	}
	bridge := &Bridge{
		agentID: agentID, generationID: spec.Manifest.ID, cmd: cmd, stdin: stdin, reader: bufio.NewReaderSize(stdout, 64<<10),
		onEvent: onEvent, onDiagnostic: onDiagnostic, onFailure: onFailure, nextID: nextID,
		pending: map[string]pendingRequest{}, operations: map[string]operationState{}, done: make(chan struct{}), readerDone: make(chan struct{}), stop: make(chan struct{}), stderr: &boundedStderr{},
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start Claude bridge: %w", err)
	}
	go io.Copy(bridge.stderr, stderrPipe) //nolint:errcheck -- bounded diagnostic capture
	if err := bridge.handshake(ctx, spec.Manifest); err != nil {
		_ = stdin.Close()
		terminateProcessGroup(cmd)
		_ = cmd.Wait()
		return nil, err
	}
	if onEvent != nil {
		bridge.events = make(chan eventDelivery)
		go bridge.dispatchEvents()
	}
	go bridge.read()
	go func() {
		<-bridge.readerDone
		_ = cmd.Wait()
		close(bridge.done)
	}()
	return bridge, nil
}

// GenerationID identifies the exact installed Claude generation backing this
// bridge. Capability evidence must not outlive this scope.
func (b *Bridge) GenerationID() string {
	if b == nil {
		return ""
	}
	return b.generationID
}

// ProcessID returns the supervised process-group leader for cleanup
// certification. It exposes no executable path or command arguments.
func (b *Bridge) ProcessID() int {
	if b == nil || b.cmd == nil || b.cmd.Process == nil {
		return 0
	}
	return b.cmd.Process.Pid
}

func (b *Bridge) handshake(ctx context.Context, manifest claudegen.Manifest) error {
	raw, err := b.readFrame(ctx)
	if err != nil {
		return fmt.Errorf("read Claude bridge hello: %w", err)
	}
	var hello struct {
		Kind              string   `json:"kind"`
		ProtocolVersion   int      `json:"protocolVersion"`
		BridgeBuild       string   `json:"bridgeBuild"`
		NodeVersion       string   `json:"nodeVersion"`
		SDKVersion        string   `json:"sdkVersion"`
		ClaudeCodeVersion string   `json:"claudeCodeVersion"`
		OS                string   `json:"os"`
		Arch              string   `json:"arch"`
		Capabilities      []string `json:"capabilities"`
	}
	if json.Unmarshal(raw, &hello) != nil || hello.Kind != "hello" {
		return errors.New("Claude bridge emitted malformed hello")
	}
	checks := []struct{ got, want, name string }{
		{fmt.Sprint(hello.ProtocolVersion), fmt.Sprint(manifest.BridgeProtocol), "protocol"},
		{hello.BridgeBuild, manifest.BridgeBuild, "build"},
		{hello.NodeVersion, manifest.NodeVersion, "Node"},
		{hello.SDKVersion, manifest.SDKVersion, "SDK"},
		{hello.ClaudeCodeVersion, manifest.ClaudeCodeVersion, "Claude Code CLI"},
		{hello.OS, runtime.GOOS, "platform OS"},
		{hello.Arch, runtime.GOARCH, "platform architecture"},
	}
	for _, check := range checks {
		if check.got != check.want {
			return fmt.Errorf("Claude bridge %s mismatch: got %q, require %q", check.name, check.got, check.want)
		}
	}
	if !sameCapabilities(manifest.RequiredCapabilities, hello.Capabilities) {
		return errors.New("Claude bridge hello capabilities do not match the exact generation")
	}
	requestID := b.nextID()
	if err := b.writeFrame(map[string]any{"kind": "initialize", "requestId": requestID, "agentId": b.agentID}); err != nil {
		return fmt.Errorf("initialize Claude bridge: %w", err)
	}
	raw, err = b.readFrame(ctx)
	if err != nil {
		return fmt.Errorf("read Claude bridge ready: %w", err)
	}
	var ready struct {
		Kind         string   `json:"kind"`
		RequestID    string   `json:"requestId"`
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal(raw, &ready) != nil || ready.Kind != "ready" || ready.RequestID != requestID {
		return errors.New("Claude bridge emitted malformed or uncorrelated ready")
	}
	if !sameCapabilities(manifest.RequiredCapabilities, ready.Capabilities) {
		return errors.New("Claude bridge ready capabilities do not match the exact generation")
	}
	return nil
}

func sameCapabilities(required, actual []string) bool {
	required, actual = append([]string(nil), required...), append([]string(nil), actual...)
	sort.Strings(required)
	sort.Strings(actual)
	return strings.Join(required, "\x00") == strings.Join(actual, "\x00")
}

func (b *Bridge) Alive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.closing && b.failed == nil && b.cmd != nil && b.cmd.Process != nil
}

func (b *Bridge) Request(ctx context.Context, command Command) (Response, error) {
	if strings.TrimSpace(command.Kind) == "" || strings.TrimSpace(command.Operation) == "" {
		return Response{}, errors.New("Claude bridge command kind and Loom operation ID are required")
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	requestID := b.nextID()
	result := make(chan requestResult, 1)
	b.mu.Lock()
	if b.failed != nil || b.closing {
		err := b.failed
		if err == nil {
			err = errors.New("Claude bridge is closed")
		}
		b.mu.Unlock()
		return Response{}, err
	}
	if _, exists := b.operations[command.Operation]; exists {
		b.mu.Unlock()
		return Response{}, errors.New("Claude bridge Loom operation ID is already active")
	}
	b.pending[requestID] = pendingRequest{operation: command.Operation, turnID: command.TurnID, result: result}
	b.operations[command.Operation] = operationState{turnID: command.TurnID}
	b.mu.Unlock()
	frame := map[string]any{"kind": "command", "command": command.Kind, "requestId": requestID, "operation": command.Operation}
	if command.TurnID != "" {
		frame["turnId"] = command.TurnID
	}
	if command.Payload != nil {
		frame["payload"] = command.Payload
	}
	if err := b.writeFrame(frame); err != nil {
		indeterminate := &IndeterminateError{Operation: command.Operation, Cause: fmt.Errorf("write Claude bridge command %s: %w", command.Kind, err)}
		b.fail(indeterminate)
		return Response{}, indeterminate
	}
	select {
	case result := <-result:
		return result.response, result.err
	case <-ctx.Done():
		err := &IndeterminateError{Operation: command.Operation, Cause: ctx.Err()}
		b.fail(err)
		return Response{}, err
	}
}

func (b *Bridge) read() {
	defer close(b.readerDone)
	if b.events != nil {
		defer close(b.events)
	}
	for {
		raw, err := b.readFrame(context.Background())
		if err != nil {
			b.fail(err)
			return
		}
		if err := b.handleFrame(raw); err != nil {
			b.fail(err)
			return
		}
	}
}

func (b *Bridge) dispatchEvents() {
	for {
		select {
		case <-b.stop:
			return
		case delivery, ok := <-b.events:
			if !ok {
				return
			}
			select {
			case <-b.stop:
				return
			default:
			}
			b.onEvent(delivery.event)
			close(delivery.ack)
		}
	}
}

func (b *Bridge) handleFrame(raw json.RawMessage) error {
	var envelope struct {
		Kind      string          `json:"kind"`
		Class     string          `json:"class"`
		Event     string          `json:"event"`
		RequestID string          `json:"requestId"`
		TurnID    string          `json:"turnId"`
		Operation string          `json:"operation"`
		Accepted  bool            `json:"accepted"`
		Data      json.RawMessage `json:"data"`
		Error     string          `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Kind == "" {
		return errors.New("Claude bridge protocol emitted malformed JSON")
	}
	switch envelope.Kind {
	case "response":
		b.mu.Lock()
		pending, ok := b.pending[envelope.RequestID]
		if !ok || envelope.Operation != pending.operation || envelope.TurnID != pending.turnID {
			b.mu.Unlock()
			return errors.New("Claude bridge protocol emitted an uncorrelated response")
		}
		delete(b.pending, envelope.RequestID)
		if envelope.Accepted {
			state := b.operations[envelope.Operation]
			if state.terminal {
				delete(b.operations, envelope.Operation)
			} else {
				b.operations[envelope.Operation] = operationState{turnID: pending.turnID, accepted: true}
			}
		} else {
			delete(b.operations, envelope.Operation)
		}
		b.mu.Unlock()
		pending.result <- requestResult{response: Response{RequestID: envelope.RequestID, TurnID: envelope.TurnID, Operation: envelope.Operation, Accepted: envelope.Accepted, Data: cloneRaw(envelope.Data), Error: envelope.Error}}
		return nil
	case "event":
		if envelope.Class == "informational" && !knownEvent(envelope.Event) {
			b.diagnostic("ignored an unknown informational Claude bridge event")
			return nil
		}
		if envelope.Class != "control" {
			return errors.New("Claude bridge known event used a non-control class")
		}
		if !knownEvent(envelope.Event) {
			return fmt.Errorf("Claude bridge protocol emitted unknown control event %q", envelope.Event)
		}
		if envelope.Operation == "" {
			return errors.New("Claude bridge event is missing Loom operation correlation")
		}
		b.mu.Lock()
		operation, correlated := b.operations[envelope.Operation]
		b.mu.Unlock()
		if !correlated || envelope.TurnID != operation.turnID {
			return errors.New("Claude bridge event is not correlated to the active Loom operation and Turn")
		}
		if terminalEvent(envelope.Event) {
			b.mu.Lock()
			b.terminalSeen = true
			for operationID, state := range b.operations {
				if state.turnID != envelope.TurnID {
					continue
				}
				if state.accepted {
					delete(b.operations, operationID)
					continue
				}
				state.terminal = true
				b.operations[operationID] = state
			}
			b.mu.Unlock()
		}
		if b.events != nil {
			delivery := eventDelivery{event: Event{AgentID: b.agentID, Kind: envelope.Event, TurnID: envelope.TurnID, Operation: envelope.Operation, Data: cloneRaw(envelope.Data)}, ack: make(chan struct{})}
			select {
			case b.events <- delivery:
			case <-b.stop:
				return errors.New("Claude bridge event delivery stopped")
			}
			select {
			case <-delivery.ack:
			case <-b.stop:
				return errors.New("Claude bridge event delivery stopped")
			}
		}
		return nil
	default:
		return fmt.Errorf("Claude bridge protocol emitted unknown control frame %q", envelope.Kind)
	}
}

func terminalEvent(kind string) bool {
	return kind == "binding_resumed" || kind == "turn_completed" || kind == "turn_failed" || kind == "turn_interrupted"
}

func knownEvent(kind string) bool {
	switch kind {
	case "binding_resumed", "turn_started", "content", "tool", "usage", "approval", "needs_you", "interrupt_receipt", "turn_completed", "turn_failed", "turn_interrupted":
		return true
	default:
		return false
	}
}

func (b *Bridge) readFrame(ctx context.Context) (json.RawMessage, error) {
	type value struct {
		raw json.RawMessage
		err error
	}
	result := make(chan value, 1)
	go func() {
		var line bytes.Buffer
		for {
			fragment, err := b.reader.ReadSlice('\n')
			if line.Len()+len(fragment) > maxFrameBytes {
				result <- value{err: fmt.Errorf("Claude bridge protocol frame exceeds %d bytes", maxFrameBytes)}
				return
			}
			line.Write(fragment)
			if err == nil {
				break
			}
			if !errors.Is(err, bufio.ErrBufferFull) {
				if errors.Is(err, io.EOF) && line.Len() != 0 {
					result <- value{err: errors.New("Claude bridge protocol output ended without LF delimiter")}
				} else {
					result <- value{err: err}
				}
				return
			}
		}
		raw := line.Bytes()
		if len(raw) <= 1 || raw[len(raw)-1] != '\n' || raw[len(raw)-2] == '\r' {
			result <- value{err: errors.New("Claude bridge protocol requires one JSON object per LF-delimited line")}
			return
		}
		result <- value{raw: append(json.RawMessage(nil), raw[:len(raw)-1]...)}
	}()
	select {
	case got := <-result:
		return got.raw, got.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *Bridge) writeFrame(frame any) error {
	line, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(line) > maxFrameBytes {
		return fmt.Errorf("Claude bridge protocol frame exceeds %d bytes", maxFrameBytes)
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_, err = b.stdin.Write(append(line, '\n'))
	return err
}

func (b *Bridge) fail(cause error) {
	b.failOnce.Do(func() {
		b.stopDelivery()
		b.mu.Lock()
		b.failed = cause
		pending := b.pending
		b.pending = map[string]pendingRequest{}
		operations := b.operations
		b.operations = map[string]operationState{}
		terminalSeen := b.terminalSeen
		closing := b.closing
		b.mu.Unlock()
		for _, request := range pending {
			request.result <- requestResult{err: &IndeterminateError{Operation: request.operation, Cause: cause}}
		}
		_ = b.stdin.Close()
		terminateProcessGroup(b.cmd)
		if !closing && b.onFailure != nil {
			accepted := 0
			for operation, state := range operations {
				if state.accepted && !state.terminal {
					accepted++
					b.reportFailure(&IndeterminateError{Operation: operation, Cause: cause})
				}
			}
			// An in-flight request owns its operation-scoped outcome. Reporting
			// the same process failure through OnFailure would race that caller
			// and can resurrect a Turn whose terminal was already delivered.
			if accepted == 0 && len(pending) == 0 && !terminalSeen {
				b.reportFailure(cause)
			}
		}
	})
}

func (b *Bridge) diagnostic(message string) {
	if b.onDiagnostic != nil {
		const limit = 1024
		if len(message) > limit {
			message = message[:limit]
		}
		go b.onDiagnostic(message)
	}
}

func (b *Bridge) reportFailure(err error) {
	go b.onFailure(b.agentID, err)
}

func (b *Bridge) stopDelivery() { b.stopOnce.Do(func() { close(b.stop) }) }

func (b *Bridge) Diagnostics() string { return b.stderr.String() }

func (b *Bridge) Close() {
	if b == nil {
		return
	}
	b.closeOnce.Do(func() {
		b.stopDelivery()
		b.mu.Lock()
		b.closing = true
		pending := b.pending
		b.pending = map[string]pendingRequest{}
		operations := b.operations
		b.operations = map[string]operationState{}
		b.mu.Unlock()
		for _, request := range pending {
			request.result <- requestResult{err: &IndeterminateError{Operation: request.operation, Cause: errors.New("Claude bridge closed before response evidence")}}
		}
		if b.onFailure != nil {
			for operation, state := range operations {
				if state.accepted && !state.terminal {
					b.reportFailure(&IndeterminateError{Operation: operation, Cause: errors.New("Claude bridge closed before terminal evidence")})
				}
			}
		}
		wrote := make(chan struct{})
		go func() {
			_ = b.writeFrame(map[string]any{"kind": "close"})
			close(wrote)
		}()
		select {
		case <-b.done:
			terminateProcessGroup(b.cmd)
			return
		case <-wrote:
		case <-time.After(closeGrace):
		}
		_ = b.stdin.Close()
		select {
		case <-b.done:
			terminateProcessGroup(b.cmd)
			return
		case <-time.After(closeGrace):
		}
		gracefulProcessGroup(b.cmd)
		select {
		case <-b.done:
			terminateProcessGroup(b.cmd)
			return
		case <-time.After(closeTimeout - 2*closeGrace):
		}
		terminateProcessGroup(b.cmd)
		select {
		case <-b.done:
		case <-time.After(closeGrace):
		}
	})
}

func cloneRaw(raw json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), raw...) }

type boundedStderr struct {
	mu    sync.Mutex
	bytes int
}

func (w *boundedStderr) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.bytes += len(p)
	if w.bytes > maxStderrBytes {
		w.bytes = maxStderrBytes
	}
	w.mu.Unlock()
	return len(p), nil
}

func (w *boundedStderr) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.bytes == 0 {
		return ""
	}
	return fmt.Sprintf("[redacted Claude bridge stderr; up to %d bytes captured]", w.bytes)
}
