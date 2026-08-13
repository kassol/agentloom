//go:build windows

package processlifecycle

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/windows"
)

const createNewProcessGroup = 0x00000200

func ConfigureDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && status == uint32(windows.WAIT_TIMEOUT)
}

// Windows preview support is unavailable; retain a compile-safe leader check
// for callers that report process cleanup diagnostics.
func GroupAlive(pid int) bool { return Alive(pid) }

// RequestGracefulStop signals the named event owned by the target CodexLoom
// process. This avoids emulating Unix signals on Windows.
func RequestGracefulStop(pid int) error {
	name, err := windows.UTF16PtrFromString(shutdownEventName(pid))
	if err != nil {
		return err
	}
	handle, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return fmt.Errorf("open CodexLoom shutdown event: %w", err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.SetEvent(handle); err != nil {
		return fmt.Errorf("signal CodexLoom shutdown event: %w", err)
	}
	return nil
}

func ForceKill(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	err = process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func WaitForShutdown() string {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)

	name, err := windows.UTF16PtrFromString(shutdownEventName(os.Getpid()))
	if err != nil {
		return (<-signals).String()
	}
	event, err := windows.CreateEvent(nil, 1, 0, name)
	if err != nil {
		return (<-signals).String()
	}
	defer windows.CloseHandle(event)

	triggered := make(chan struct{}, 1)
	go func() {
		if status, waitErr := windows.WaitForSingleObject(event, windows.INFINITE); waitErr == nil && status == uint32(windows.WAIT_OBJECT_0) {
			triggered <- struct{}{}
		}
	}()
	select {
	case signalValue := <-signals:
		return signalValue.String()
	case <-triggered:
		return "Windows shutdown event"
	}
}
