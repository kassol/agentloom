package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const maxRPCLineBytes = 16 << 20

type RPCOptions struct {
	Bin       string
	Cwd       string
	Args      []string
	Env       map[string]string
	OnEvent   func(json.RawMessage)
	OnFailure func(error)
}

type RPCResponse struct {
	ID      string
	Command string
	Success bool
	Data    json.RawMessage
	Error   string
}

type ExtensionUIResponse struct {
	ID        string
	Value     string
	Cancelled bool
}

type rpcResult struct {
	response RPCResponse
	err      error
}

type pendingCommand struct {
	command string
	result  chan rpcResult
}

type RPC struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	onEvent   func(json.RawMessage)
	onFailure func(error)
	events    chan json.RawMessage

	mu      sync.Mutex
	nextID  uint64
	pending map[string]pendingCommand
	failed  error
	closing bool

	writeMu     sync.Mutex
	failureOnce sync.Once
	closeOnce   sync.Once
}

func SpawnRPC(options RPCOptions) (*RPC, error) {
	bin, err := ResolveBin(options.Bin)
	if err != nil {
		return nil, err
	}
	args := append([]string{"--mode", "rpc"}, options.Args...)
	command := exec.Command(bin, args...)
	command.Dir = options.Cwd
	command.Env = append([]string(nil), os.Environ()...)
	for key, value := range options.Env {
		prefix := key + "="
		replaced := false
		for index, entry := range command.Env {
			if strings.HasPrefix(entry, prefix) {
				command.Env[index] = prefix + value
				replaced = true
				break
			}
		}
		if !replaced {
			command.Env = append(command.Env, prefix+value)
		}
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open Pi RPC stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open Pi RPC stdout: %w", err)
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	rpc := &RPC{
		cmd: command, stdin: stdin, onEvent: options.OnEvent, onFailure: options.OnFailure,
		pending: map[string]pendingCommand{},
	}
	if options.OnEvent != nil {
		rpc.events = make(chan json.RawMessage, 256)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start Pi RPC: %w", err)
	}
	if rpc.events != nil {
		go func() {
			for event := range rpc.events {
				rpc.onEvent(event)
			}
		}()
	}
	go rpc.read(stdout)
	go func() {
		err := command.Wait()
		rpc.mu.Lock()
		closing := rpc.closing
		rpc.mu.Unlock()
		if closing {
			return
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			rpc.fail(fmt.Errorf("Pi RPC process exited: %v: %s", err, detail))
			return
		}
		if err == nil {
			rpc.fail(errors.New("Pi RPC process exited unexpectedly"))
		} else {
			rpc.fail(fmt.Errorf("Pi RPC process exited: %w", err))
		}
	}()
	return rpc, nil
}

func (r *RPC) Request(ctx context.Context, command string, fields map[string]any) (RPCResponse, error) {
	request := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		request[key] = value
	}
	r.mu.Lock()
	if r.failed != nil {
		err := r.failed
		r.mu.Unlock()
		return RPCResponse{}, err
	}
	if r.closing {
		r.mu.Unlock()
		return RPCResponse{}, errors.New("Pi RPC process is closed")
	}
	r.nextID++
	id := fmt.Sprintf("loom-%d", r.nextID)
	result := make(chan rpcResult, 1)
	r.pending[id] = pendingCommand{command: command, result: result}
	r.mu.Unlock()
	request["id"] = id
	request["type"] = command
	line, err := json.Marshal(request)
	if err == nil {
		r.writeMu.Lock()
		_, err = r.stdin.Write(append(line, '\n'))
		r.writeMu.Unlock()
	}
	if err != nil {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return RPCResponse{}, fmt.Errorf("write Pi RPC command %s: %w", command, err)
	}
	select {
	case value := <-result:
		return value.response, value.err
	case <-ctx.Done():
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		err := fmt.Errorf("Pi RPC command %s: %w", command, ctx.Err())
		// A late response cannot be correlated safely, and a timed-out prompt
		// may already be mutating the workspace. Fail the process so Hub enters
		// interruption recovery instead of reporting a false terminal failure.
		r.fail(err)
		return RPCResponse{}, err
	}
}

// RespondExtensionUI writes Pi's correlated dialog response without creating
// an RPC command waiter. Pi resolves the matching Extension UI promise and
// intentionally sends no acknowledgement for this one-way protocol message.
func (r *RPC) RespondExtensionUI(response ExtensionUIResponse) error {
	if strings.TrimSpace(response.ID) == "" {
		return errors.New("Pi Extension UI response id is required")
	}
	if response.Cancelled && response.Value != "" {
		return errors.New("Pi Extension UI response cannot contain both value and cancelled")
	}
	if !response.Cancelled && response.Value == "" {
		return errors.New("Pi Extension UI response value is required")
	}
	r.mu.Lock()
	if r.failed != nil {
		err := r.failed
		r.mu.Unlock()
		return err
	}
	if r.closing {
		r.mu.Unlock()
		return errors.New("Pi RPC process is closed")
	}
	r.mu.Unlock()
	message := map[string]any{"type": "extension_ui_response", "id": response.ID}
	if response.Cancelled {
		message["cancelled"] = true
	} else {
		message["value"] = response.Value
	}
	line, err := json.Marshal(message)
	if err == nil {
		r.writeMu.Lock()
		_, err = r.stdin.Write(append(line, '\n'))
		r.writeMu.Unlock()
	}
	if err != nil {
		err = fmt.Errorf("write Pi Extension UI response %s: %w", response.ID, err)
		r.fail(err)
		return err
	}
	return nil
}

func (r *RPC) Close() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closing = true
		pending := r.pending
		r.pending = map[string]pendingCommand{}
		r.mu.Unlock()
		for _, command := range pending {
			command.result <- rpcResult{err: errors.New("Pi RPC process is closed")}
		}
		_ = r.stdin.Close()
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
	})
}

func (r *RPC) Alive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closing && r.failed == nil && r.cmd != nil && r.cmd.Process != nil
}

func (r *RPC) read(stdout io.Reader) {
	if r.events != nil {
		defer close(r.events)
	}
	reader := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := reader.ReadString('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxRPCLineBytes {
			r.fail(fmt.Errorf("Pi RPC protocol line exceeds %d bytes", maxRPCLineBytes))
			return
		}
		if err != nil {
			if len(line) > 0 {
				r.fail(errors.New("Pi RPC protocol output ended without LF delimiter"))
			}
			return
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" || strings.HasSuffix(line, "\r") {
			r.fail(errors.New("Pi RPC protocol requires one JSON object per LF-delimited line"))
			return
		}
		r.handleLine(json.RawMessage(line))
		r.mu.Lock()
		failed := r.failed != nil
		r.mu.Unlock()
		if failed {
			return
		}
	}
}

func (r *RPC) handleLine(raw json.RawMessage) {
	var envelope struct {
		ID      string          `json:"id"`
		Type    string          `json:"type"`
		Command string          `json:"command"`
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   string          `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Type == "" {
		r.fail(errors.New("Pi RPC protocol emitted a malformed JSON object"))
		return
	}
	if envelope.Type != "response" {
		if r.events != nil {
			r.events <- append(json.RawMessage(nil), raw...)
		}
		return
	}
	r.mu.Lock()
	pending, ok := r.pending[envelope.ID]
	if ok {
		delete(r.pending, envelope.ID)
	}
	r.mu.Unlock()
	if !ok {
		r.fail(fmt.Errorf("Pi RPC protocol emitted unexpected response id %q", envelope.ID))
		return
	}
	if envelope.Command != pending.command {
		err := fmt.Errorf("Pi RPC protocol response %s used command %q, expected %q", envelope.ID, envelope.Command, pending.command)
		pending.result <- rpcResult{err: err}
		r.fail(err)
		return
	}
	response := RPCResponse{ID: envelope.ID, Command: envelope.Command, Success: envelope.Success, Data: envelope.Data, Error: envelope.Error}
	if !response.Success {
		message := strings.TrimSpace(response.Error)
		if message == "" {
			message = "unspecified error"
		}
		pending.result <- rpcResult{response: response, err: fmt.Errorf("Pi RPC command %s failed: %s", pending.command, message)}
		return
	}
	pending.result <- rpcResult{response: response}
}

func (r *RPC) fail(err error) {
	r.failureOnce.Do(func() {
		r.mu.Lock()
		r.failed = err
		pending := r.pending
		r.pending = map[string]pendingCommand{}
		r.mu.Unlock()
		for _, command := range pending {
			command.result <- rpcResult{err: err}
		}
		if r.onFailure != nil {
			r.onFailure(err)
		}
		_ = r.stdin.Close()
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
	})
}
