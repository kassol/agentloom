//go:build windows

package main

import "fmt"

func processCommand(pid int) (string, error) {
	return "", fmt.Errorf("process command inspection for pid %d is unsupported on windows", pid)
}
