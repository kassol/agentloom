package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type runtimeGatewayFixture struct {
	Version      int            `json:"version"`
	Controls     map[string]any `json:"controls"`
	Observations map[string]any `json:"observations"`
}

func TestRuntimeGatewayStateRaisesFloorWithStateInOneFile(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := st.ClaimWritableOwnership()
	if err != nil {
		t.Fatal(err)
	}
	want := runtimeGatewayFixture{Version: 1, Controls: map[string]any{}, Observations: map[string]any{}}
	if err := st.SaveRuntimeGatewayState(want); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, foundationFileName))
	if err != nil {
		t.Fatal(err)
	}
	var envelope runtimeFoundationEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != runtimeFoundationSchemaVersion || envelope.MinimumWriter != runtimeWriterFloorGatewayState {
		t.Fatalf("foundation compatibility = schema %d floor %d", envelope.SchemaVersion, envelope.MinimumWriter)
	}
	var got runtimeGatewayFixture
	exists, err := st.LoadRuntimeGatewayState(&got)
	if err != nil || !exists || got.Version != 1 || got.Controls == nil || got.Observations == nil {
		t.Fatalf("round trip = %#v, exists=%v, err=%v", got, exists, err)
	}
	owner.Release()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("current writer rejected its own floor: %v", err)
	}
	_ = reopened.Close()
}

func TestRuntimeGatewayStateRequiresLiveHubOwnership(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	value := runtimeGatewayFixture{Version: 1, Controls: map[string]any{}, Observations: map[string]any{}}
	if err := st.SaveRuntimeGatewayState(value); err == nil {
		t.Fatal("foundation write did not require live Hub ownership")
	}
	if _, err := os.Stat(filepath.Join(dir, foundationFileName)); !os.IsNotExist(err) {
		t.Fatalf("rejected foundation write created a file: %v", err)
	}
}

func TestRuntimeGatewayStateRejectsOversizeDocumentBeforeCommit(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := st.ClaimWritableOwnership()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { owner.Release(); _ = st.Close() }()
	value := runtimeGatewayFixture{Version: 1, Controls: map[string]any{
		"conn": map[string]any{
			"connectionId": "conn", "epoch": 1, "lifecycle": "provisioning", "recovery": "none",
			"reason":    strings.Repeat("x", runtimeFoundationMaxBytes),
			"binding":   map[string]any{"connection": map[string]any{"id": "conn", "provider": "test", "enabled": true, "createdAt": "t"}, "addresses": []any{}},
			"updatedAt": "t",
		},
	}, Observations: map[string]any{}}
	if err := st.SaveRuntimeGatewayState(value); err == nil {
		t.Fatal("oversize foundation was committed")
	}
	if _, err := os.Stat(filepath.Join(dir, foundationFileName)); !os.IsNotExist(err) {
		t.Fatalf("oversize rejection created foundation: %v", err)
	}
}

func TestRuntimeGatewayFoundationRejectsUnknownOrIncompleteStateBeforeMutation(t *testing.T) {
	cases := map[string]string{
		"unknown":              `{"schemaVersion":1,"minimumWriter":2,"state":{"version":2,"gatewayState":{"version":1,"controls":{},"observations":{},"extra":true}}}`,
		"missing-observations": `{"schemaVersion":1,"minimumWriter":2,"state":{"version":2,"gatewayState":{"version":1,"controls":{}}}}`,
		"bad-control":          `{"schemaVersion":1,"minimumWriter":2,"state":{"version":2,"gatewayState":{"version":1,"controls":{"conn":{"connectionId":"conn","epoch":0,"lifecycle":"adopted","recovery":"none","binding":{"connection":{"id":"conn","provider":"x","enabled":true,"createdAt":"t"},"addresses":[]},"updatedAt":"t"}},"observations":{}}}}`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, foundationFileName)
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			before := snapshotDirectory(t, dir)
			if st, err := Open(dir); err == nil {
				_ = st.Close()
				t.Fatal("invalid R0b foundation was accepted")
			}
			after := snapshotDirectory(t, dir)
			if len(before) != len(after) || before[foundationFileName] != after[foundationFileName] {
				t.Fatalf("failed open mutated foundation: before=%v after=%v", before, after)
			}
		})
	}
}
