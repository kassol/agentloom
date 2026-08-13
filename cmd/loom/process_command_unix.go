//go:build darwin || linux

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

func processCommand(pid int) (string, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	return strings.TrimSpace(string(output)), err
}
