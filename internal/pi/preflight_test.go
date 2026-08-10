package pi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckAllowsMinimumAndNewerVersions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, version := range []string{"0.84.1", "0.85.0", "1.0.0"} {
		t.Run(version, func(t *testing.T) {
			if err := Check(fakePi(t, version)); err != nil {
				t.Fatalf("Check() error = %v", err)
			}
		})
	}
}

func TestCheckRejectsMissingUnparseableAndOldPi(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	t.Run("missing", func(t *testing.T) {
		t.Setenv("PI_BIN", "")
		t.Setenv("PATH", t.TempDir())
		assertCheckError(t, "", "pi not found in PATH", "set PI_BIN")
	})
	t.Run("unparseable", func(t *testing.T) {
		assertCheckError(t, fakePi(t, "pi development build"), "cannot parse", "0.84.1")
	})
	t.Run("below minimum", func(t *testing.T) {
		assertCheckError(t, fakePi(t, "0.84.0"), "0.84.0 is too old", "0.84.1 or newer")
	})
}

func TestCheckUsesConfiguredPiBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	if MinimumVersion != "0.84.1" {
		t.Fatalf("MinimumVersion = %q", MinimumVersion)
	}
	t.Setenv("PI_BIN", fakePi(t, MinimumVersion))
	if err := Check(""); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
}

func assertCheckError(t *testing.T, bin string, fragments ...string) {
	t.Helper()
	err := Check(bin)
	if err == nil {
		t.Fatal("Check() succeeded")
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("Check() error = %q, want %q", err, fragment)
		}
	}
}

func fakePi(t *testing.T, output string) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n[ \"$1\" = \"--version\" ] || exit 9\nprintf '%s\\n' '"+output+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return executable
}
