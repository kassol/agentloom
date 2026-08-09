// Package credentials implements the v1 managed credential file store.
//
// Credentials are immutable: every Put writes a fresh random ID and never
// overwrites an existing file in place. The fixed Owner-only directory is
// protected by local file permissions (directory 0700, files 0600); there is
// no at-rest encryption and the threat model does not cover a malicious
// same-UID Owner. Only macOS/POSIX is supported in this stage.
package credentials

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DirectoryName is the fixed Owner-only credential directory beneath the
// Runtime data directory. Ordinary backups must exclude it.
const DirectoryName = "credentials"

const (
	idBytes    = 32
	idHexLen   = idBytes * 2
	maxSecret  = 1 << 20
	dirMode    = 0o700
	fileMode   = 0o600
)

// Ref is the canonical Hub-issued reference for one managed credential. It is
// always "managed:" followed by the random opaque ID; it is never a path.
type Ref string

// Store manages the fixed Owner-only credentials directory.
type Store struct {
	dir string
}

// New returns a v1 credential store rooted at dataDir/credentials. Non-POSIX
// platforms fail closed.
func New(dataDir string) (*Store, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, fmt.Errorf("managed credentials are unsupported on %s", runtime.GOOS)
	}
	clean := filepath.Clean(dataDir)
	if clean == "" || clean == "." || clean == string(filepath.Separator) {
		return nil, fmt.Errorf("credential store requires a data directory")
	}
	return &Store{dir: filepath.Join(clean, DirectoryName)}, nil
}

// Put durably writes one new immutable credential and returns its canonical
// reference. The secret is written to a temporary file and atomically renamed
// to a fresh random ID; an existing ID is never overwritten.
func (s *Store) Put(secret []byte) (Ref, error) {
	if len(secret) == 0 || len(secret) > maxSecret {
		return "", fmt.Errorf("credential secret must be between 1 byte and %d bytes", maxSecret)
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return "", fmt.Errorf("create credential store: %w", err)
	}
	if err := verifyOwnerOnlyPath(s.dir, true); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(s.dir, ".managed-credential-*")
	if err != nil {
		return "", fmt.Errorf("stage credential: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(fileMode); err != nil {
		return "", err
	}
	if _, err := temporary.Write(secret); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	target := filepath.Join(s.dir, id)
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("credential ID collision")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return "", err
	}
	committed = true
	if err := verifyOwnerOnlyPath(target, false); err != nil {
		return "", err
	}
	return Ref("managed:" + id), nil
}

// Resolve returns the secret bytes for one canonical reference.
func (s *Store) Resolve(ref Ref) ([]byte, error) {
	id, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, id)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("managed credential not found")
		}
		return nil, err
	}
	defer file.Close()
	if err := verifyOwnerOnlyFile(file); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= 0 || info.Size() > maxSecret {
		return nil, fmt.Errorf("managed credential is not a bounded regular file")
	}
	data := make([]byte, info.Size())
	if _, err := file.Read(data); err != nil {
		return nil, err
	}
	return data, nil
}

// Delete removes one canonical managed credential.
func (s *Store) Delete(ref Ref) error {
	id, err := parseRef(ref)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.dir, id)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("managed credential not found")
		}
		return err
	}
	return nil
}

func newID() (string, error) {
	random := make([]byte, idBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("issue managed credential ID: %w", err)
	}
	return hex.EncodeToString(random), nil
}

func parseRef(ref Ref) (string, error) {
	value := strings.TrimSpace(string(ref))
	const prefix = "managed:"
	if !strings.HasPrefix(value, prefix) {
		return "", fmt.Errorf("managed credential reference is not Hub-issued")
	}
	id := strings.TrimPrefix(value, prefix)
	if len(id) != idHexLen || !isLowerHex(id) {
		return "", fmt.Errorf("managed credential reference is not canonical")
	}
	return id, nil
}

func isLowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') {
			continue
		}
		return false
	}
	return true
}
