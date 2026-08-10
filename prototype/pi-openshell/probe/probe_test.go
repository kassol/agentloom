package probe_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/prototype/pi-openshell/probe"
)

func TestInspectReportsMissingOpenShellWithoutInvokingUnrelatedHostPi(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "host-pi-ran")
	writeExecutable(t, filepath.Join(dir, "pi"), "#!/bin/sh\ntouch \"$HOST_PI_MARKER\"\n")
	t.Setenv("PATH", dir)
	t.Setenv("HOST_PI_MARKER", marker)

	report := probe.Inspect(context.Background(), probe.Options{OpenShellBin: "openshell"})

	if report.State != "unavailable" {
		t.Fatalf("state = %q, want unavailable", report.State)
	}
	if report.Reason != "openshell executable was not found" {
		t.Fatalf("reason = %q", report.Reason)
	}
	if report.HostFallback != "not_implemented" || report.IsolationEvidence != "unverified" {
		t.Fatalf("fallback/isolation = %q/%q", report.HostFallback, report.IsolationEvidence)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("host Pi fallback ran: stat err = %v", err)
	}
	if report.HostOS != runtime.GOOS || report.HostArch != runtime.GOARCH {
		t.Fatalf("host = %s/%s, want %s/%s", report.HostOS, report.HostArch, runtime.GOOS, runtime.GOARCH)
	}
}

func TestInspectRequiresReachableOpenShellGateway(t *testing.T) {
	dir := t.TempDir()
	openShell := filepath.Join(dir, "openshell")
	writeExecutable(t, openShell, `#!/bin/sh
case "$1" in
  --version) echo "openshell 0.9.0-prototype" ;;
  status) echo "gateway unavailable" >&2; exit 7 ;;
  *) exit 64 ;;
esac
`)

	report := probe.Inspect(context.Background(), probe.Options{OpenShellBin: openShell})

	if report.State != "unavailable" || report.Reason != "openshell gateway is not reachable" {
		t.Fatalf("report = %#v", report)
	}
	if report.OpenShellVersion != "openshell 0.9.0-prototype" {
		t.Fatalf("version = %q", report.OpenShellVersion)
	}
	if !strings.Contains(report.Detail, "gateway unavailable") {
		t.Fatalf("detail = %q", report.Detail)
	}
	if report.HostFallback != "not_implemented" || report.IsolationEvidence != "unverified" {
		t.Fatalf("fallback/isolation = %q/%q", report.HostFallback, report.IsolationEvidence)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
