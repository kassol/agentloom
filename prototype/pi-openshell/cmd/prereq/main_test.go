package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/yan5xu/codex-loom/prototype/pi-openshell/probe"
)

func TestRunEmitsMachineReadableUnavailableReport(t *testing.T) {
	var output bytes.Buffer
	exitCode := run(context.Background(), &output, filepath.Join(t.TempDir(), "missing-openshell"))
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	var report probe.Report
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, output.String())
	}
	if report.State != "unavailable" || report.HostFallback != "not_implemented" || report.IsolationEvidence != "unverified" {
		t.Fatalf("report = %#v", report)
	}
}
