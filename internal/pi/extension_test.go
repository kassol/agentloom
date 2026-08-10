package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeLoomExtensionCreatesOnePrivateCollaborationExtension(t *testing.T) {
	dataDir := t.TempDir()
	path, err := MaterializeLoomExtension(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dataDir, "pi", "runtime", "loom-extension.ts") {
		t.Fatalf("extension path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("extension mode = %o", info.Mode().Perm())
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, name := range []string{"loom_agents_find", "loom_message_send", "loom_message_receive", "loom_message_reply"} {
		if strings.Count(text, `name: "`+name+`"`) != 1 {
			t.Fatalf("tool %s is not registered exactly once", name)
		}
	}
	for _, forbidden := range []string{"loom_needs_you", "loom_approval", "--no-skills", "--no-extensions"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("extension contains out-of-scope capability %q", forbidden)
		}
	}
}
