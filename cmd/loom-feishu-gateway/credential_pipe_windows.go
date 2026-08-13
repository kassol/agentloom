//go:build windows

package main

import "fmt"

func reexecWithCredentialPipe(_ []byte) error {
	return fmt.Errorf("managed Feishu gateway is unsupported on Windows")
}
