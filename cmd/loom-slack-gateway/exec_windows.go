//go:build windows

package main

import (
	"os"
	"os/exec"
)

func runNode(node string, arguments, environment []string) error {
	cmd := exec.Command(node, arguments[1:]...)
	cmd.Env = environment
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
