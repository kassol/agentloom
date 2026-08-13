//go:build darwin || linux

package main

import (
	"encoding/json"
	"os"
	"syscall"
)

func execWithCredentialPipe(executable string, arguments, environment []string, values map[string]string) error {
	payload, err := json.Marshal(values)
	if err != nil {
		return err
	}
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
	if err := syscall.Dup2(int(reader.Fd()), 3); err != nil {
		return err
	}
	return syscall.Exec(executable, arguments, environment)
}
