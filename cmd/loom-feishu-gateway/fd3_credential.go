package main

import (
	"io"
	"os"
)

// readInheritedCredentialFD reads the Lark managed credential from inherited
// descriptor 3 when the parent spawned this gateway with an anonymous
// credential FD. The secret never appears in argv or the normal environment.
func readInheritedCredentialFD() ([]byte, bool) {
	file := os.NewFile(3, "managed-credential")
	if file == nil {
		return nil, false
	}
	defer file.Close()
	const maxCredentialBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxCredentialBytes {
		return nil, false
	}
	return data, true
}
