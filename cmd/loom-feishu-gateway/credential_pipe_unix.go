//go:build darwin || linux

package main

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func reexecWithCredentialPipe(payload []byte) error {
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reader.Close()
	if _, err := writer.Write(payload); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := unix.Dup2(int(reader.Fd()), 3); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	environment := append(os.Environ(), "CODEX_LOOM_CREDENTIAL_FD_CHILD=1")
	return syscall.Exec(executable, os.Args, environment)
}
