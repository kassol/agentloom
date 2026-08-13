package platform

import (
	"reflect"
	"testing"
)

func TestExecutableNameForPlatform(t *testing.T) {
	tests := []struct {
		goos string
		name string
		want string
	}{
		{goos: "windows", name: "codex-loom", want: "codex-loom.exe"},
		{goos: "windows", name: "codex-loom.EXE", want: "codex-loom.EXE"},
		{goos: "darwin", name: "codex-loom", want: "codex-loom"},
		{goos: "linux", name: "codex-loom", want: "codex-loom"},
	}
	for _, test := range tests {
		if got := executableNameFor(test.goos, test.name); got != test.want {
			t.Errorf("executableNameFor(%q, %q) = %q, want %q", test.goos, test.name, got, test.want)
		}
	}
}

func TestBrowserCommandForPlatform(t *testing.T) {
	const target = "http://localhost:4870"
	tests := []struct {
		goos string
		name string
		args []string
	}{
		{goos: "darwin", name: "open", args: []string{target}},
		{goos: "linux", name: "xdg-open", args: []string{target}},
		{goos: "windows", name: "rundll32", args: []string{"url.dll,FileProtocolHandler", target}},
	}
	for _, test := range tests {
		name, args, err := browserCommand(test.goos, target)
		if err != nil {
			t.Fatalf("browserCommand(%q): %v", test.goos, err)
		}
		if name != test.name || !reflect.DeepEqual(args, test.args) {
			t.Errorf("browserCommand(%q) = %q %#v, want %q %#v", test.goos, name, args, test.name, test.args)
		}
	}
	if _, _, err := browserCommand("plan9", target); err == nil {
		t.Fatal("unsupported platform accepted")
	}
}
