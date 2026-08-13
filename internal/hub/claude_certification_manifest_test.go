package hub

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/claudegen"
)

type claudeCertificationEvidence struct {
	Requirement string
	File        string
	Test        string
}

var claudeDeterministicCertificationManifest = []claudeCertificationEvidence{
	{"shared_runtime_contract", "runtime_contract_conformance_test.go", "TestRuntimeContractConformanceCodexPiClaudeAndMinimalFake"},
	{"unknown_available_capability", "runtime_contract_conformance_test.go", "TestRuntimeContractCertificationRejectsUnknownAvailableCapability"},
	{"mixed_http_sse_store_reopen", "../httpapi/mixed_runtime_core_story_test.go", "TestCanonicalMixedRuntimeHTTPAndSSEExecuteCodexPiClaudeAcrossRestart"},
	{"adoption_boundary", "runtime_adoption_test.go", "TestClaudeAdoptionCommitsPublicHistoryBoundaryAndOwnerRecovery"},
	{"approval_projection_and_redaction", "claude_runtime_test.go", "TestClaudeContractProjectsApprovalAndNeedsYouWithoutNativeIdentity"},
	{"archive_reopen", "claude_runtime_test.go", "TestClaudeArchiveRestorePreservesLedgerAndAgentIdentity"},
	{"callback_terminal_race", "claude_runtime_test.go", "TestClaudeNeedsYouRacingTerminalNeverLeavesOrphanRequest"},
	{"collaboration_redaction", "approval_test.go", "TestClaudeApprovalProjectionHidesNativeCollaborationIdentifiers"},
	{"context_and_usage_reopen", "claude_observability_test.go", "TestClaudePassiveContextAndUsageReadCanonicalLedgerWithoutRuntime"},
	{"four_platform_preview_gate", "claude_certification_manifest_test.go", "TestClaudePreviewWorkflowDefinesFourRequiredRowsAndGatesMissingResults"},
	{"goal_store_driver_restart", "goal_test.go", "TestLoomGoalConformanceAcrossCodexPiAndClaudeStoreDriverRestart"},
	{"hub_shutdown_serialization", "runtime_contract_consumers_test.go", "TestShutdownWaitsForSerializedRuntimeMutationBeforeStoreRetirement"},
	{"store_commit_race", "claude_runtime_test.go", "TestClaudeLedgerCommitFailureFencesHostAndCreatesOneNeedsYou"},
	{"canonical_ledger_redaction", "claude_runtime_test.go", "TestClaudeLedgerSanitizesBeforeWritingDisk"},
	{"model_reopen", "claude_runtime_test.go", "TestClaudeNonDefaultModelReopensAndDrivesNextTurn"},
	{"needs_you_answer_reopen", "claude_runtime_test.go", "TestClaudeNeedsYouInterruptsSourceAndAnswerResumesOnceAcrossReopen"},
	{"no_blind_replay", "claude_runtime_test.go", "TestClaudeIndeterminateRecoveryCreatesOneNeedsYouWithoutReplay"},
	{"process_tree_cleanup", "../claudebridge/bridge_matrix_test.go", "TestBridgeCloseAndDriverShutdownAreIdempotentAndReapProcessGroup"},
	{"resource_configuration", "claude_runtime_test.go", "TestClaudeResourceSnapshotIncludesOnlyMatchingVerifiedConfigurationEvidence"},
	{"blocked_callback_cleanup", "../claudebridge/bridge_matrix_test.go", "TestBridgeCloseReapsProcessWhenEventConsumerNeverReturns"},
	{"typed_failure_phases", "runtime_contract_conformance_test.go", "TestRuntimeContractConformanceCodexPiClaudeAndMinimalFake"},
}

func TestClaudeDeterministicCertificationManifestIsCompleteAndInstalled(t *testing.T) {
	required := []string{
		"adoption_boundary", "approval_projection_and_redaction", "archive_reopen",
		"blocked_callback_cleanup", "callback_terminal_race", "canonical_ledger_redaction",
		"collaboration_redaction", "context_and_usage_reopen", "four_platform_preview_gate", "goal_store_driver_restart",
		"hub_shutdown_serialization", "mixed_http_sse_store_reopen", "model_reopen", "needs_you_answer_reopen",
		"no_blind_replay", "process_tree_cleanup", "resource_configuration", "shared_runtime_contract", "store_commit_race", "typed_failure_phases",
		"unknown_available_capability",
	}
	actual := make([]string, 0, len(claudeDeterministicCertificationManifest))
	for _, evidence := range claudeDeterministicCertificationManifest {
		actual = append(actual, evidence.Requirement)
		path := filepath.Clean(evidence.File)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("%s evidence file %s: %v", evidence.Requirement, path, err)
		}
		found := false
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && function.Name.Name == evidence.Test {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s evidence test %s is not installed in %s", evidence.Requirement, evidence.Test, path)
		}
	}
	slices.Sort(actual)
	if !slices.Equal(actual, required) {
		t.Fatalf("Claude deterministic certification requirements = %q, want %q", actual, required)
	}
}

func TestClaudePreviewWorkflowDefinesFourRequiredRowsAndGatesMissingResults(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "claude-preview-smoke.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, required := range []string{
		"row: darwin-arm64", "row: darwin-x64", "row: linux-arm64", "row: linux-x64",
		"runner: macos-14-xlarge", "runner: macos-14", "accept_install_terms:",
		"scripts/run-claude-preview-smoke.sh", "scripts/verify-claude-preview-results.sh",
		"Keep missing or failed evidence gated", "four-row result contract",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Claude preview workflow is missing %q", required)
		}
	}
	verifier, err := os.ReadFile(filepath.Join("..", "..", "scripts", "verify-claude-preview-results.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(verifier), `"productionReady":false`) ||
		!strings.Contains(string(verifier), `"status":"passed"`) ||
		!strings.Contains(string(verifier), "does not match") {
		t.Fatal("four-row verifier could treat missing evidence or productionReady=true as passing")
	}
	smoke, err := os.ReadFile(filepath.Join("..", "..", "scripts", "run-claude-preview-smoke.sh"))
	if err != nil {
		t.Fatal(err)
	}
	generation := claudegen.CurrentManifest().ID
	if !strings.Contains(string(smoke), generation) || !strings.Contains(string(verifier), generation) {
		t.Fatalf("preview scripts do not require current generation %q", generation)
	}
	releaseGates, err := os.ReadFile(filepath.Join("..", "..", "scripts", "verify-release-gates.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string][]byte{
		"run-claude-preview-smoke.sh":      smoke,
		"verify-claude-preview-results.sh": verifier,
		"verify-release-gates.sh":          releaseGates,
	} {
		if strings.Contains(string(content), "rg ") {
			t.Fatalf("%s depends on non-portable ripgrep availability", path)
		}
	}
}

func TestClaudePreviewResultVerifierRequiresCurrentCommitAndAllRows(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	commitBytes, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(string(commitBytes))
	results := t.TempDir()
	generation := claudegen.CurrentManifest().ID
	for _, row := range []struct {
		name, os, arch string
	}{
		{"darwin-arm64", "darwin", "arm64"},
		{"darwin-x64", "darwin", "x64"},
		{"linux-arm64", "linux", "arm64"},
		{"linux-x64", "linux", "x64"},
	} {
		payload := fmt.Sprintf(`{"schemaVersion":1,"row":%q,"os":%q,"arch":%q,"generation":%q,"commit":%q,"status":"passed","reason":"verified","productionReady":false}`+"\n",
			row.name, row.os, row.arch, generation, commit)
		if err := os.WriteFile(filepath.Join(results, row.name+".json"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	verify := func() ([]byte, error) {
		command := exec.Command("sh", filepath.Join(root, "scripts", "verify-claude-preview-results.sh"), results)
		command.Dir = root
		return command.CombinedOutput()
	}
	if output, err := verify(); err != nil {
		t.Fatalf("four current preview rows did not verify: %v\n%s", err, output)
	}
	stale := filepath.Join(results, "linux-x64.json")
	payload := fmt.Sprintf(`{"schemaVersion":1,"row":"linux-x64","os":"linux","arch":"x64","generation":%q,"commit":"0000000000000000000000000000000000000000","status":"passed","reason":"verified","productionReady":false}`+"\n", generation)
	if err := os.WriteFile(stale, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(); err == nil {
		t.Fatal("four-row verifier accepted a stale commit")
	}
}
