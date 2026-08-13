package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ExecutableName returns the native filename for a command.
func ExecutableName(name string) string {
	return executableNameFor(runtime.GOOS, name)
}

func executableNameFor(goos, name string) string {
	if goos == "windows" && !strings.EqualFold(filepath.Ext(name), ".exe") {
		return name + ".exe"
	}
	return name
}

// IsExecutableFile reports whether info can be launched on the current OS.
// Windows does not expose Unix execute bits; a regular .exe file is sufficient.
func IsExecutableFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0
}

// OpenBrowser opens a URL with the desktop's registered browser.
func OpenBrowser(rawURL string) error {
	name, args, err := browserCommand(runtime.GOOS, rawURL)
	if err != nil {
		return err
	}
	return exec.Command(name, args...).Start()
}

func browserCommand(goos, rawURL string) (string, []string, error) {
	switch goos {
	case "darwin":
		return "open", []string{rawURL}, nil
	case "linux":
		return "xdg-open", []string{rawURL}, nil
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}, nil
	default:
		return "", nil, fmt.Errorf("opening a browser is unsupported on %s", goos)
	}
}
