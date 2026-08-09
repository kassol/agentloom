package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/credentials"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestGatewayProcessProofFromEnvRequiresCompleteIdentity(t *testing.T) {
	t.Setenv("CODEX_LOOM_GATEWAY_ATTEMPT_ID", "")
	t.Setenv("CODEX_LOOM_GATEWAY_GENERATION", "")
	t.Setenv("CODEX_LOOM_GATEWAY_BUILD", "")
	t.Setenv("CODEX_LOOM_GATEWAY_EXECUTABLE_DIGEST", "")
	if proof := gatewayProcessProofFromEnv(); proof != nil {
		t.Fatalf("empty proof environment produced a proof: %#v", proof)
	}
	t.Setenv("CODEX_LOOM_GATEWAY_ATTEMPT_ID", "gattempt_l2a")
	t.Setenv("CODEX_LOOM_GATEWAY_GENERATION", "ggen_l2a")
	t.Setenv("CODEX_LOOM_GATEWAY_BUILD", "build-l2a")
	t.Setenv("CODEX_LOOM_GATEWAY_EXECUTABLE_DIGEST", "digest-l2a")
	proof := gatewayProcessProofFromEnv()
	if proof == nil || proof.AttemptID != "gattempt_l2a" || proof.Generation != "ggen_l2a" ||
		proof.Build != "build-l2a" || proof.ExecutableDigest != "digest-l2a" {
		t.Fatalf("exact proof identity was not read: %#v", proof)
	}
}

func TestResolveManagedSecretReadOnly(t *testing.T) {
	dir := t.TempDir()
	ownerStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ownerStore.ClaimWritableOwnership(); err != nil {
		t.Fatal(err)
	}
	if err := ownerStore.SaveCredentialFloor(); err != nil {
		t.Fatal(err)
	}
	credentialStore, err := credentials.New(ownerStore)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("gateway-managed-secret")
	ref, err := credentialStore.Put(want)
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolveManagedSecret(dir, ref)
	if err != nil {
		t.Fatalf("read-only managed resolution failed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("resolved secret does not match the managed credential")
	}
	if _, err := resolveManagedSecret(dir, credentials.Ref("managed:"+strings.Repeat("0", 64))); err == nil {
		t.Fatal("missing managed reference resolved without error")
	}
	if _, err := resolveManagedSecret("", ref); err == nil {
		t.Fatal("empty data directory resolved a managed reference")
	}
}
