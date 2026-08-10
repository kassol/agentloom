package pi

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed loom_extension.ts
var loomExtensionSource []byte

func MaterializeLoomExtension(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "pi", "runtime")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create Pi Runtime directory: %w", err)
	}
	path := filepath.Join(dir, "loom-extension.ts")
	tmp, err := os.CreateTemp(dir, ".loom-extension-*")
	if err != nil {
		return "", fmt.Errorf("create Loom Pi Extension: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("protect Loom Pi Extension: %w", err)
	}
	if _, err := tmp.Write(loomExtensionSource); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write Loom Pi Extension: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync Loom Pi Extension: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close Loom Pi Extension: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("install Loom Pi Extension: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("protect installed Loom Pi Extension: %w", err)
	}
	return path, nil
}
