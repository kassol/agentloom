//go:build darwin || linux

package main

import "syscall"

func runNode(node string, arguments, environment []string) error {
	return syscall.Exec(node, arguments, environment)
}
