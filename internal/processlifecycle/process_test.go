package processlifecycle

import "testing"

func TestShutdownEventNameIsStableAndPIDScoped(t *testing.T) {
	if got, want := shutdownEventName(4870), `Local\CodexLoomShutdown-4870`; got != want {
		t.Fatalf("shutdownEventName() = %q, want %q", got, want)
	}
	if Alive(0) {
		t.Fatal("pid zero reported alive")
	}
}
