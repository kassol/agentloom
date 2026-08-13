//go:build windows

package main

import "fmt"

func execWithCredentialPipe(_ string, _ []string, _ []string, _ map[string]string) error {
	return fmt.Errorf("managed Parall gateway is unsupported on Windows")
}
