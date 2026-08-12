package claudebridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestBridgeRejectsEveryHandshakeMismatchBeforePublishing(t *testing.T) {
	t.Parallel()
	hello := matrixHello()
	tests := map[string]string{
		"protocol":         strings.Replace(hello, `"protocolVersion":1`, `"protocolVersion":2`, 1),
		"build":            strings.Replace(hello, `"bridgeBuild":"claude-bridge-v1"`, `"bridgeBuild":"other"`, 1),
		"Node":             strings.Replace(hello, `"nodeVersion":"24.19.0"`, `"nodeVersion":"24.18.0"`, 1),
		"SDK":              strings.Replace(hello, `"sdkVersion":"0.3.228"`, `"sdkVersion":"0.3.227"`, 1),
		"CLI":              strings.Replace(hello, `"claudeCodeVersion":"2.1.228"`, `"claudeCodeVersion":"2.1.227"`, 1),
		"OS":               strings.Replace(hello, `"os":"`+runtime.GOOS+`"`, `"os":"other"`, 1),
		"architecture":     strings.Replace(hello, `"arch":"`+runtime.GOARCH+`"`, `"arch":"other"`, 1),
		"capability":       strings.Replace(hello, `,"session_resume"`, `,"future"`, 1),
		"extra capability": strings.Replace(hello, `"session_resume"]`, `"session_resume","future"]`, 1),
	}
	for name, incompatible := range tests {
		name, incompatible := name, incompatible
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeFakeBridge(t, fmt.Sprintf("printf '%%s\\n' '%s'\nsleep 10\n", incompatible))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			bridge, err := matrixDriver(path, nil).Acquire(ctx, "agent-mismatch")
			if err == nil || bridge != nil {
				t.Fatalf("incompatible %s published bridge %#v: %v", name, bridge, err)
			}
		})
	}
}

func TestBridgeRejectsReadyCapabilityMismatchBeforePublishing(t *testing.T) {
	t.Parallel()
	for name, capabilities := range map[string]string{
		"missing": `"interrupt","approval","hooks","mcp"`,
		"extra":   `"interrupt","approval","hooks","mcp","session_resume","future"`,
	} {
		name, capabilities := name, capabilities
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '{"kind":"ready","requestId":"loom-1","capabilities":[%s]}\n'
sleep 10
`, matrixHello(), capabilities))
			bridge, err := matrixDriver(path, nil).Acquire(context.Background(), "agent-ready-mismatch")
			if err == nil || bridge != nil {
				t.Fatalf("ready %s capability mismatch published bridge %#v: %v", name, bridge, err)
			}
		})
	}
}

func TestBridgeCorrelatesFragmentedResponsesEventsAndUnknownInformation(t *testing.T) {
	t.Parallel()
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf '%%s\n' '{"kind":"event","class":"informational","event":"sdk_future_notice","operation":"loom-operation"}'
printf '%%s' '{"kind":"event","class":"control","event":"content","turnId":"loom-turn","operation":"loom-operation","data":{"text":"hel'
sleep 0.02
printf '%%s\n' 'lo"}}'
printf '%%s\n' '{"kind":"response","requestId":"loom-2","turnId":"loom-turn","operation":"loom-operation","accepted":true,"data":{"ok":true}}'
while IFS= read -r line; do [ "$line" = '{"kind":"close"}' ] && exit 0; done
`, matrixHello()))
	var mu sync.Mutex
	var events []Event
	var diagnostics []string
	driver := matrixDriver(path, func(options *DriverOptions) {
		options.OnEvent = func(event Event) {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		}
		options.OnDiagnostic = func(value string) {
			mu.Lock()
			diagnostics = append(diagnostics, value)
			mu.Unlock()
		}
	})
	bridge, err := driver.Acquire(context.Background(), "agent-correlation")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	response, err := bridge.Request(context.Background(), Command{Kind: "start_turn", TurnID: "loom-turn", Operation: "loom-operation", Payload: map[string]string{"text": "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if response.RequestID != "loom-2" || response.TurnID != "loom-turn" || response.Operation != "loom-operation" || !response.Accepted || string(response.Data) != `{"ok":true}` {
		t.Fatalf("response = %#v", response)
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		ready := len(events) == 1 && len(diagnostics) == 1
		mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("event and diagnostic callbacks did not drain")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Kind != "content" || events[0].TurnID != "loom-turn" || string(events[0].Data) != `{"text":"hello"}` {
		t.Fatalf("events = %#v", events)
	}
	if len(diagnostics) != 1 || diagnostics[0] != "ignored an unknown informational Claude bridge event" {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestBridgeRejectsEventWithForeignOperationOrTurnCorrelation(t *testing.T) {
	t.Parallel()
	for name, event := range map[string]string{
		"operation": `{"kind":"event","class":"control","event":"content","turnId":"loom-turn","operation":"foreign-operation"}`,
		"Turn":      `{"kind":"event","class":"control","event":"content","turnId":"foreign-turn","operation":"loom-operation"}`,
	} {
		name, event := name, event
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf '%%s\n' '%s'
`, matrixHello(), event))
			bridge, err := matrixDriver(path, nil).Acquire(context.Background(), "agent-correlation-failure")
			if err != nil {
				t.Fatal(err)
			}
			defer bridge.Close()
			_, err = bridge.Request(context.Background(), Command{Kind: "start_turn", TurnID: "loom-turn", Operation: "loom-operation"})
			var indeterminate *IndeterminateError
			if !errors.As(err, &indeterminate) {
				t.Fatalf("foreign %s correlation error = %#v", name, err)
			}
		})
	}
}

func TestBridgeRejectsKnownEventWithInformationalClass(t *testing.T) {
	t.Parallel()
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf '%%s\n' '{"kind":"event","class":"informational","event":"turn_completed","turnId":"loom-turn","operation":"loom-operation"}'
`, matrixHello()))
	bridge, err := matrixDriver(path, nil).Acquire(context.Background(), "agent-event-class")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	_, err = bridge.Request(context.Background(), Command{Kind: "start_turn", TurnID: "loom-turn", Operation: "loom-operation"})
	var indeterminate *IndeterminateError
	if !errors.As(err, &indeterminate) {
		t.Fatalf("known informational control event error = %#v", err)
	}
}

func TestBridgeBadResponseCorrelationSettlesRequestIndeterminate(t *testing.T) {
	t.Parallel()
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf '%%s\n' '{"kind":"response","requestId":"loom-2","turnId":"wrong-turn","operation":"loom-operation","accepted":true}'
sleep 10
`, matrixHello()))
	bridge, err := matrixDriver(path, nil).Acquire(context.Background(), "agent-response-correlation")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	result := make(chan error, 1)
	go func() {
		_, err := bridge.Request(context.Background(), Command{Kind: "start_turn", TurnID: "loom-turn", Operation: "loom-operation"})
		result <- err
	}()
	select {
	case err := <-result:
		var indeterminate *IndeterminateError
		if !errors.As(err, &indeterminate) {
			t.Fatalf("bad response correlation error = %#v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bad response correlation orphaned its pending request")
	}
}

func TestBridgeProtocolFailuresAndDeadlinesAreIndeterminate(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"malformed":       "printf '%s\\n' 'not-json'",
		"unterminated":    "printf '%s' '{\"kind\":\"response\"}'",
		"unknown control": "printf '%s\\n' '{\"kind\":\"event\",\"class\":\"control\",\"event\":\"future_control\",\"operation\":\"loom-operation\"}'",
		"oversized":       "head -c 1048577 /dev/zero | tr '\\000' x; printf '\\n'",
	}
	for name, failure := range tests {
		name, failure := name, failure
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
%s
`, matrixHello(), failure))
			bridge, err := matrixDriver(path, nil).Acquire(context.Background(), "agent-failure")
			if err != nil {
				t.Fatal(err)
			}
			defer bridge.Close()
			_, err = bridge.Request(context.Background(), Command{Kind: "start_turn", Operation: "loom-operation"})
			var indeterminate *IndeterminateError
			if !errors.As(err, &indeterminate) || indeterminate.Operation != "loom-operation" {
				t.Fatalf("error = %#v, want operation-scoped Indeterminate Runtime Outcome", err)
			}
		})
	}

	t.Run("deadline", func(t *testing.T) {
		t.Parallel()
		path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
sleep 10
`, matrixHello()))
		bridge, err := matrixDriver(path, nil).Acquire(context.Background(), "agent-timeout")
		if err != nil {
			t.Fatal(err)
		}
		defer bridge.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err = bridge.Request(ctx, Command{Kind: "start_turn", Operation: "loom-timeout"})
		var indeterminate *IndeterminateError
		if !errors.As(err, &indeterminate) || indeterminate.Operation != "loom-timeout" || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline error = %#v", err)
		}
	})
}

func TestBridgeFailureAfterAcceptedResponseReportsOperationIndeterminate(t *testing.T) {
	t.Parallel()
	failures := make(chan error, 1)
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf '%%s\n' '{"kind":"response","requestId":"loom-2","operation":"loom-accepted","accepted":true}'
printf '%%s\n' 'malformed-terminal'
`, matrixHello()))
	driver := matrixDriver(path, func(options *DriverOptions) {
		options.OnFailure = func(_ string, err error) { failures <- err }
	})
	bridge, err := driver.Acquire(context.Background(), "agent-accepted")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	response, err := bridge.Request(context.Background(), Command{Kind: "start_turn", Operation: "loom-accepted"})
	if err != nil || !response.Accepted {
		t.Fatalf("accepted response=%#v err=%v", response, err)
	}
	select {
	case err := <-failures:
		var indeterminate *IndeterminateError
		if !errors.As(err, &indeterminate) || indeterminate.Operation != "loom-accepted" {
			t.Fatalf("failure = %#v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted operation failure was not reported")
	}
}

func TestBridgeTerminalBeforeAcceptedResponseDoesNotResurrectOperation(t *testing.T) {
	t.Parallel()
	failures := make(chan error, 1)
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf '%%s\n' '{"kind":"event","class":"control","event":"turn_completed","turnId":"loom-turn","operation":"loom-terminal-first"}'
printf '%%s\n' '{"kind":"response","requestId":"loom-2","turnId":"loom-turn","operation":"loom-terminal-first","accepted":true}'
while IFS= read -r line; do [ "$line" = '{"kind":"close"}' ] && exit 0; done
`, matrixHello()))
	bridge, err := matrixDriver(path, func(options *DriverOptions) {
		options.OnFailure = func(_ string, err error) { failures <- err }
	}).Acquire(context.Background(), "agent-terminal-first")
	if err != nil {
		t.Fatal(err)
	}
	response, err := bridge.Request(context.Background(), Command{Kind: "start_turn", TurnID: "loom-turn", Operation: "loom-terminal-first"})
	if err != nil || !response.Accepted {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	bridge.Close()
	select {
	case err := <-failures:
		t.Fatalf("terminal operation was resurrected as indeterminate: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestBridgeCloseReapsProcessWhenEventConsumerNeverReturns(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	blocked := make(chan struct{})
	pidPath := filepath.Join(t.TempDir(), "bridge.pid")
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' "$$" > "$1"
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf '%%s\n' '{"kind":"event","class":"control","event":"content","turnId":"loom-turn","operation":"loom-blocked-callback"}'
sleep 60
`, matrixHello()))
	driver := NewDriver(DriverOptions{
		ResolveActive: func(context.Context) (LaunchSpec, error) {
			return LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Args: []string{pidPath}, Manifest: testManifest()}, nil
		},
		NextID: matrixIDs(),
		OnEvent: func(Event) {
			close(entered)
			<-blocked
		},
	})
	bridge, err := driver.Acquire(context.Background(), "agent-blocked-callback")
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan error, 1)
	go func() {
		_, err := bridge.Request(context.Background(), Command{Kind: "start_turn", TurnID: "loom-turn", Operation: "loom-blocked-callback"})
		requestDone <- err
	}()
	<-entered
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	bridge.Close()
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Close blocked %s behind event consumer", elapsed)
	}
	if matrixProcessAlive(pid) {
		t.Fatalf("bridge process %d was not reaped", pid)
	}
	close(blocked)
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("request did not settle after Close")
	}
}

func TestBridgeCloseReapsProcessWhenDiagnosticConsumerNeverReturns(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	blocked := make(chan struct{})
	pidPath := filepath.Join(t.TempDir(), "bridge.pid")
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' "$$" > "$1"
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf '%%s\n' '{"kind":"event","class":"informational","event":"future","operation":"loom-diagnostic"}'
sleep 60
`, matrixHello()))
	driver := NewDriver(DriverOptions{
		ResolveActive: func(context.Context) (LaunchSpec, error) {
			return LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Args: []string{pidPath}, Manifest: testManifest()}, nil
		},
		NextID: matrixIDs(),
		OnDiagnostic: func(string) {
			close(entered)
			<-blocked
		},
	})
	bridge, err := driver.Acquire(context.Background(), "agent-blocked-diagnostic")
	if err != nil {
		t.Fatal(err)
	}
	go bridge.Request(context.Background(), Command{Kind: "start_turn", Operation: "loom-diagnostic"}) //nolint:errcheck -- settled by Close
	<-entered
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	bridge.Close()
	if matrixProcessAlive(pid) {
		t.Fatalf("bridge process %d survived blocked diagnostic observer", pid)
	}
	close(blocked)
}

func TestBridgeCloseReportsAcceptedOperationWithoutTerminalAsIndeterminate(t *testing.T) {
	t.Parallel()
	failures := make(chan error, 1)
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf '%%s\n' '{"kind":"response","requestId":"loom-2","operation":"loom-close","accepted":true}'
while IFS= read -r line; do [ "$line" = '{"kind":"close"}' ] && exit 0; done
`, matrixHello()))
	bridge, err := matrixDriver(path, func(options *DriverOptions) {
		options.OnFailure = func(_ string, err error) { failures <- err }
	}).Acquire(context.Background(), "agent-close")
	if err != nil {
		t.Fatal(err)
	}
	response, err := bridge.Request(context.Background(), Command{Kind: "start_turn", Operation: "loom-close"})
	if err != nil || !response.Accepted {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	bridge.Close()
	select {
	case err := <-failures:
		var indeterminate *IndeterminateError
		if !errors.As(err, &indeterminate) || indeterminate.Operation != "loom-close" {
			t.Fatalf("close failure = %#v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close lost accepted operation without terminal evidence")
	}
}

func TestBridgeCloseFencesInFlightRequestBeforeClosingProcess(t *testing.T) {
	t.Parallel()
	received := filepath.Join(t.TempDir(), "command-received")
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf received > "$1"
sleep 0.1
printf '%%s\n' '{"kind":"response","requestId":"loom-2","operation":"loom-in-flight","accepted":false}'
while IFS= read -r line; do [ "$line" = '{"kind":"close"}' ] && exit 0; done
`, matrixHello()))
	driver := NewDriver(DriverOptions{
		ResolveActive: func(context.Context) (LaunchSpec, error) {
			return LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Args: []string{received}, Manifest: testManifest()}, nil
		},
		NextID: matrixIDs(),
	})
	bridge, err := driver.Acquire(context.Background(), "agent-in-flight")
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan error, 1)
	go func() {
		_, err := bridge.Request(context.Background(), Command{Kind: "inspect", Operation: "loom-in-flight"})
		requestDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(received); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake bridge did not receive request")
		}
		time.Sleep(time.Millisecond)
	}
	closeDone := make(chan struct{})
	go func() { bridge.Close(); close(closeDone) }()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not settle the in-flight request")
	}
	if err := <-requestDone; err != nil {
		var indeterminate *IndeterminateError
		if !errors.As(err, &indeterminate) || indeterminate.Operation != "loom-in-flight" {
			t.Fatalf("in-flight close error = %#v", err)
		}
	} else {
		t.Fatal("Close reported a possibly accepted in-flight operation as completed")
	}
}

func TestBridgeCloseFencesUnresponsiveInFlightRequest(t *testing.T) {
	t.Parallel()
	received := filepath.Join(t.TempDir(), "command-received")
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf received > "$1"
sleep 60
`, matrixHello()))
	driver := NewDriver(DriverOptions{
		ResolveActive: func(context.Context) (LaunchSpec, error) {
			return LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Args: []string{received}, Manifest: testManifest()}, nil
		},
		NextID: matrixIDs(),
	})
	bridge, err := driver.Acquire(context.Background(), "agent-unresponsive")
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan error, 1)
	go func() {
		_, err := bridge.Request(context.Background(), Command{Kind: "start_turn", Operation: "loom-unresponsive"})
		requestDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(received); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake bridge did not receive request")
		}
		time.Sleep(time.Millisecond)
	}
	closeDone := make(chan struct{})
	go func() { bridge.Close(); close(closeDone) }()
	select {
	case err := <-requestDone:
		var indeterminate *IndeterminateError
		if !errors.As(err, &indeterminate) || indeterminate.Operation != "loom-unresponsive" {
			t.Fatalf("unresponsive request error = %#v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not fence the unresponsive in-flight request")
	}
	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not kill the unresponsive process group within its deadline")
	}
}

func TestBridgeSlowEventConsumerBackpressuresWithoutReordering(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "writer-finished")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
payload=$(head -c 65536 /dev/zero | tr '\000' x)
i=0
while [ "$i" -lt 128 ]; do
  printf '{"kind":"event","class":"control","event":"content","operation":"loom-slow","data":{"text":"%%s"}}\n' "$payload"
  i=$((i + 1))
done
printf done > '%s'
printf '%%s\n' '{"kind":"response","requestId":"loom-2","operation":"loom-slow","accepted":true}'
while IFS= read -r line; do [ "$line" = '{"kind":"close"}' ] && exit 0; done
`, matrixHello(), marker))
	driver := matrixDriver(path, func(options *DriverOptions) {
		options.OnEvent = func(Event) { once.Do(func() { close(entered) }); <-release }
	})
	bridge, err := driver.Acquire(context.Background(), "agent-slow")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	result := make(chan error, 1)
	go func() {
		_, err := bridge.Request(context.Background(), Command{Kind: "start_turn", Operation: "loom-slow"})
		result <- err
	}()
	<-entered
	time.Sleep(30 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("bridge writer did not backpressure behind the slow reader: %v", err)
	}
	select {
	case err := <-result:
		t.Fatalf("response overtook blocked event callback: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("bridge writer did not resume after reader progress: %v", err)
	}
}

func TestBridgeBoundsAndRedactsStderr(t *testing.T) {
	t.Parallel()
	failures := make(chan error, 1)
	secret := strings.Repeat("x", maxStderrBytes+512)
	path := writeFakeBridge(t, fmt.Sprintf(`
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
printf '%s Authorization: Bearer-secret token=private' >&2
exit 17
`, matrixHello(), secret))
	driver := matrixDriver(path, func(options *DriverOptions) {
		options.OnFailure = func(_ string, err error) { failures <- err }
	})
	bridge, err := driver.Acquire(context.Background(), "agent-stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	select {
	case <-failures:
	case <-time.After(time.Second):
		t.Fatal("process exit not reported")
	}
	diagnostic := bridge.Diagnostics()
	if len(diagnostic) > maxStderrBytes || strings.Contains(diagnostic, "Bearer-secret") || strings.Contains(diagnostic, "private") {
		t.Fatalf("stderr was not bounded and redacted: len=%d tail=%q", len(diagnostic), diagnostic)
	}
}

func TestBridgeCloseAndDriverShutdownAreIdempotentAndReapProcessGroup(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process group contract is supported on macOS and Linux")
	}
	for _, action := range []string{"Bridge Close", "Driver Shutdown"} {
		t.Run(action, func(t *testing.T) {
			pidPath := filepath.Join(t.TempDir(), "child.pid")
			path := writeFakeBridge(t, fmt.Sprintf(`
sleep 60 &
printf '%%s\n' "$!" > "$1"
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
while IFS= read -r line; do [ "$line" = '{"kind":"close"}' ] && exit 0; done
`, matrixHello()))
			driver := NewDriver(DriverOptions{
				ResolveActive: func(context.Context) (LaunchSpec, error) {
					return LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Args: []string{pidPath}, Manifest: testManifest()}, nil
				},
				NextID: matrixIDs(),
			})
			bridge, err := driver.Acquire(context.Background(), "agent-tree")
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(pidPath)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			if action == "Bridge Close" {
				bridge.Close()
				bridge.Close()
			} else {
				if err := driver.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := driver.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(time.Second)
			for matrixProcessAlive(pid) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if matrixProcessAlive(pid) {
				t.Fatalf("child process %d survived %s", pid, action)
			}
		})
	}
}

func TestBridgeGracefulExitStillReapsDetachedStdioDescendant(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process group contract is supported on macOS and Linux")
	}
	pidPath := filepath.Join(t.TempDir(), "detached-child.pid")
	path := writeFakeBridge(t, fmt.Sprintf(`
sleep 60 </dev/null >/dev/null 2>&1 &
printf '%%s\n' "$!" > "$1"
printf '%%s\n' '%s'
IFS= read -r init
printf '%%s\n' '{"kind":"ready","requestId":"loom-1","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
while IFS= read -r line; do [ "$line" = '{"kind":"close"}' ] && exit 0; done
`, matrixHello()))
	driver := NewDriver(DriverOptions{
		ResolveActive: func(context.Context) (LaunchSpec, error) {
			return LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Args: []string{pidPath}, Manifest: testManifest()}, nil
		},
		NextID: matrixIDs(),
	})
	bridge, err := driver.Acquire(context.Background(), "agent-detached-tree")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	bridge.Close()
	deadline := time.Now().Add(time.Second)
	for matrixProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if matrixProcessAlive(pid) {
		t.Fatalf("detached-stdio descendant %d survived graceful bridge exit", pid)
	}
}

func matrixHello() string {
	return fmt.Sprintf(`{"kind":"hello","protocolVersion":1,"bridgeBuild":"claude-bridge-v1","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","os":%q,"arch":%q,"capabilities":["interrupt","approval","hooks","mcp","session_resume"]}`, runtime.GOOS, runtime.GOARCH)
}

func matrixDriver(path string, configure func(*DriverOptions)) *Driver {
	options := DriverOptions{
		ResolveActive: func(context.Context) (LaunchSpec, error) {
			return LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Manifest: testManifest()}, nil
		},
		NextID: matrixIDs(),
	}
	if configure != nil {
		configure(&options)
	}
	return NewDriver(options)
}

func matrixIDs() func() string {
	var mu sync.Mutex
	next := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		next++
		return fmt.Sprintf("loom-%d", next)
	}
}

func matrixProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
