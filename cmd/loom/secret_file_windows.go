//go:build windows

package main

import "os"

// Windows file mode bits do not describe ACL ownership. The credential is
// accepted only as a non-symlink regular file; managed credentials should use
// Windows Credential Manager through the normal integration flow.
func verifyOwnerOnlySecretFile(os.FileInfo) error {
	return nil
}
