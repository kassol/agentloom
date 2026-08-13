//go:build windows

package processlifecycle

import (
	"os"
	"testing"
	"time"
)

func TestWindowsShutdownEventRequestsGracefulStop(t *testing.T) {
	done := make(chan string, 1)
	go func() {
		done <- WaitForShutdown()
	}()

	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = RequestGracefulStop(os.Getpid()); lastErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("request graceful stop: %v", lastErr)
	}
	select {
	case reason := <-done:
		if reason != "Windows shutdown event" {
			t.Fatalf("shutdown reason = %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Windows shutdown event was not observed")
	}
}
