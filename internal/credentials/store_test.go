package credentials

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVPutResolveRoundTripAndOwnerOnlyPermissions(t *testing.T) {
	dataDir := t.TempDir()
	store, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("v1-secret-勿-泄露")
	ref, err := store.Put(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(ref), "managed:") || len(string(ref)) != len("managed:")+idHexLen {
		t.Fatalf("ref is not canonical: %q", ref)
	}
	got, err := store.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatal("resolved secret does not match")
	}
	dirInfo, err := os.Stat(filepath.Join(dataDir, DirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode().Perm() != dirMode {
		t.Fatalf("credential directory permissions = %v", dirInfo.Mode().Perm())
	}
	id, _ := parseRef(ref)
	fileInfo, err := os.Stat(filepath.Join(dataDir, DirectoryName, id))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != fileMode {
		t.Fatalf("credential file permissions = %v", fileInfo.Mode().Perm())
	}
}

func TestVPutIsImmutableAndNeverOverwrites(t *testing.T) {
	dataDir := t.TempDir()
	store, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Put([]byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put([]byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two Puts returned the same reference")
	}
	firstValue, err := store.Resolve(first)
	if err != nil || string(firstValue) != "first" {
		t.Fatalf("first credential changed: %q err=%v", firstValue, err)
	}
	// A temp file must not survive a successful Put.
	entries, err := os.ReadDir(filepath.Join(dataDir, DirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("credential directory contains %d entries, want 2", len(entries))
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".managed-credential-") {
			t.Fatal("stale credential temp file survived")
		}
	}
}

func TestVDeleteRemovesOnlyTarget(t *testing.T) {
	dataDir := t.TempDir()
	store, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Put([]byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put([]byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(first); err == nil {
		t.Fatal("deleted credential still resolves")
	}
	if value, err := store.Resolve(second); err != nil || string(value) != "second" {
		t.Fatalf("unrelated credential damaged: %q err=%v", value, err)
	}
	if err := store.Delete(Ref("managed:" + strings.Repeat("0", idHexLen))); err == nil {
		t.Fatal("deleting a missing credential succeeded")
	}
}

func TestVRefRejectsPathsAndMalformedIDs(t *testing.T) {
	dataDir := t.TempDir()
	store, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{
		"../../etc/passwd", "managed:../..", "managed:", "managed:xyz",
		"managed:" + strings.Repeat("Z", idHexLen), "managed:" + strings.Repeat("a", idHexLen-1),
		"/abs/path", "C:\\windows\\system32",
	} {
		if _, err := store.Resolve(Ref(candidate)); err == nil {
			t.Fatalf("malformed reference resolved: %q", candidate)
		}
		if err := store.Delete(Ref(candidate)); err == nil {
			t.Fatalf("malformed reference deleted: %q", candidate)
		}
	}
}

func TestVSecretNeverAppearsInErrors(t *testing.T) {
	dataDir := t.TempDir()
	store, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("sensitive-value-should-not-leak")
	ref, err := store.Put(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(Ref("managed:" + strings.Repeat("b", idHexLen))); err != nil && strings.Contains(err.Error(), string(secret)) {
		t.Fatal("secret leaked into an error")
	}
	if err := store.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(ref); err != nil && strings.Contains(err.Error(), string(secret)) {
		t.Fatal("secret leaked into a not-found error")
	}
}
