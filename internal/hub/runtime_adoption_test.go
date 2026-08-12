package hub

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

type adoptionTestDriver struct {
	*controlPlaneDriver
	candidate   nativeConversationCandidate
	discoverErr error
	inspectErr  error
}

type blockingAdoptionDriver struct {
	*adoptionTestDriver
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
	start   sync.Once
}

func (d *blockingAdoptionDriver) Acquire(ctx context.Context, request AgentHostRequest) (AgentHost, error) {
	d.calls.Add(1)
	d.start.Do(func() { close(d.started) })
	select {
	case <-d.release:
		return d.adoptionTestDriver.controlPlaneDriver.Acquire(ctx, request)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *adoptionTestDriver) DiscoverConversations(context.Context) ([]nativeConversationCandidate, error) {
	if d.discoverErr != nil {
		return nil, d.discoverErr
	}
	return []nativeConversationCandidate{d.candidate}, nil
}

func (d *adoptionTestDriver) InspectConversation(context.Context, string) (nativeConversationCandidate, error) {
	if d.inspectErr != nil {
		return nativeConversationCandidate{}, d.inspectErr
	}
	return d.candidate, nil
}

func adoptionFixture(t *testing.T) (*Hub, *store.Store, *adoptionTestDriver, *controlPlaneHost) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	contract := &controlPlaneContract{history: runtimecontract.History{Turns: []runtimecontract.HistoryTurn{}}}
	host := &controlPlaneHost{contract: contract, alive: true}
	const nativeRef = "/private/native/conversation"
	candidate := nativeConversationCandidate{RuntimeConversationCandidate: RuntimeConversationCandidate{
		ID: candidateToken("fake", nativeRef), Revision: "candidate:one", RuntimeKind: "fake", Name: "Existing", Cwd: t.TempDir(), UpdatedAt: "2026-08-12T00:00:00Z", Compatible: true,
	}, nativeRef: nativeRef}
	driver := &adoptionTestDriver{controlPlaneDriver: &controlPlaneDriver{acquireHost: host}, candidate: candidate}
	h.runtimeHostDrivers["fake"] = driver
	return h, st, driver, host
}

func TestConversationCatalogIsTypedAndRedactsNativeIdentity(t *testing.T) {
	h, st, _, _ := adoptionFixture(t)
	defer st.Close()
	snapshot, err := h.RuntimeConversationCapabilities("fake")
	if err != nil || len(snapshot.Capabilities) != 4 || !snapshot.Capabilities[0].Available || !snapshot.Capabilities[3].Available {
		t.Fatalf("capabilities = %#v, err=%v", snapshot, err)
	}
	candidates, err := h.DiscoverRuntimeConversations("fake")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %#v, err=%v", candidates, err)
	}
	raw, _ := json.Marshal(candidates)
	if strings.Contains(string(raw), "/private/") || strings.Contains(string(raw), "nativeRef") {
		t.Fatalf("public candidates leaked native identity: %s", raw)
	}

	h.runtimeHostDrivers["unsupported"] = &controlPlaneDriver{}
	unsupported, err := h.RuntimeConversationCapabilities("unsupported")
	if err != nil || unsupported.Capabilities[0].Available || !unsupported.Capabilities[3].Available {
		t.Fatalf("unsupported capabilities = %#v, err=%v", unsupported, err)
	}
}

func TestCodexConversationCandidateUsesOpaqueIdentityAndRejectsEphemeralThreads(t *testing.T) {
	recency := int64(200)
	candidate := codexConversationCandidate(codexConversationThread{ID: "native-thread-uuid", Preview: "A native preview", Cwd: "/workspace", CreatedAt: 100, UpdatedAt: 150, RecencyAt: &recency})
	if candidate.ID == "native-thread-uuid" || !strings.HasPrefix(candidate.ID, "cand_") || candidate.UpdatedAt != "1970-01-01T00:03:20Z" || !candidate.Compatible {
		t.Fatalf("candidate = %#v", candidate)
	}
	ephemeral := codexConversationCandidate(codexConversationThread{ID: "ephemeral", Cwd: "/workspace", UpdatedAt: 150, Ephemeral: true})
	if ephemeral.Compatible || ephemeral.Compatibility == "" {
		t.Fatalf("ephemeral = %#v", ephemeral)
	}
}

func TestPiConversationInspectionReportsMalformedHistoryAsIncompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.jsonl")
	data := `{"type":"session","version":3,"id":"pi-broken","timestamp":"2026-08-12T00:00:00Z","cwd":"/tmp/work"}` + "\n{broken}\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := &piRuntimeHostDriver{}
	candidate, err := driver.InspectConversation(context.Background(), path)
	if err != nil || candidate.Compatible || candidate.Compatibility == "" {
		t.Fatalf("candidate=%#v err=%v", candidate, err)
	}
}

func TestAdoptConversationCommitsOnceAndRetriesIdempotently(t *testing.T) {
	h, st, driver, host := adoptionFixture(t)
	defer st.Close()
	params := AdoptConversationParams{CandidateID: driver.candidate.ID, ExpectedRevision: driver.candidate.Revision, Name: "adopted"}
	created, err := h.AdoptRuntimeConversation("fake", params)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.ThreadID == "" || created.RuntimeBinding.Kind != "fake" || created.RuntimeBinding.NativeRef != "" || !host.Alive() {
		t.Fatalf("created = %#v, alive=%t", created, host.Alive())
	}
	driver.discoverErr = errors.New("catalog offline after native resume")
	retried, err := h.AdoptRuntimeConversation("fake", params)
	if err != nil || retried.ID != created.ID || retried.ThreadID != created.ThreadID {
		t.Fatalf("retry = %#v, err=%v", retried, err)
	}
	if _, err := h.AdoptRuntimeConversation("fake", AdoptConversationParams{CandidateID: driver.candidate.ID, ExpectedRevision: driver.candidate.Revision, Name: "different"}); err == nil || !strings.Contains(err.Error(), "different Agent configuration") {
		t.Fatalf("different intent err = %v", err)
	}

	var persisted map[string]*Agent
	if err := st.LoadAgents(&persisted); err != nil || persisted[created.ID] == nil || persisted[created.ID].RuntimeBinding.NativeRef != driver.candidate.nativeRef {
		t.Fatalf("persisted = %#v, err=%v", persisted, err)
	}
	if events, err := st.ReadEvents(created.ID, 0, 10); err != nil || len(events) == 0 || events[0].Type != "loom/agent-adopted" {
		t.Fatalf("events = %#v, err=%v", events, err)
	}
	global, err := st.ReadEvents(globalEventLogID, 0, 10)
	globalJSON, _ := json.Marshal(global)
	if err != nil || !strings.Contains(string(globalJSON), "loom/agent-adopted") || strings.Contains(string(globalJSON), driver.candidate.nativeRef) {
		t.Fatalf("global adoption SSE rows = %s, err=%v", globalJSON, err)
	}
}

func TestAdoptConversationConcurrentRetriesReturnOneAgent(t *testing.T) {
	h, st, driver, _ := adoptionFixture(t)
	defer st.Close()
	params := AdoptConversationParams{CandidateID: driver.candidate.ID, ExpectedRevision: driver.candidate.Revision, Name: "concurrent"}

	const callers = 8
	results := make([]AgentView, callers)
	errs := make([]error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for index := range callers {
		go func() {
			defer wait.Done()
			results[index], errs[index] = h.AdoptRuntimeConversation("fake", params)
		}()
	}
	wait.Wait()
	for index := range callers {
		if errs[index] != nil || results[index].ID != results[0].ID || results[index].ThreadID != results[0].ThreadID {
			t.Fatalf("result[%d]=%#v err=%v, first=%#v", index, results[index], errs[index], results[0])
		}
	}
	if agents := h.ListAgents(); len(agents) != 1 {
		t.Fatalf("agents=%#v", agents)
	}
}

func TestShutdownWaitsForBlockedAdoptionAndRejectsItsCommit(t *testing.T) {
	h, st, base, host := adoptionFixture(t)
	defer st.Close()
	driver := &blockingAdoptionDriver{adoptionTestDriver: base, started: make(chan struct{}), release: make(chan struct{})}
	h.runtimeHostDrivers["fake"] = driver
	var adoptionSaves atomic.Int32
	h.saveAgentsForTest = func(value any) error {
		if len(value.(map[string]*Agent)) != 0 {
			adoptionSaves.Add(1)
		}
		return nil
	}
	params := AdoptConversationParams{CandidateID: base.candidate.ID, ExpectedRevision: base.candidate.Revision, Name: "shutdown-race"}
	adoptDone := make(chan error, 1)
	go func() {
		_, err := h.AdoptRuntimeConversation("fake", params)
		adoptDone <- err
	}()
	<-driver.started

	shutdownDone := make(chan struct{})
	go func() {
		h.Shutdown()
		close(shutdownDone)
	}()
	waitForHubStopping(t, h)
	select {
	case <-shutdownDone:
		close(driver.release)
		t.Fatal("Shutdown retired Runtime ownership while adoption Acquire was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	close(driver.release)
	if err := <-adoptDone; err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("adoption racing Shutdown = %v", err)
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after adoption transaction exited")
	}
	if adoptionSaves.Load() != 0 || len(h.ListAgents()) != 0 || host.Alive() {
		t.Fatalf("shutdown race adoption saves=%d agents=%#v alive=%t", adoptionSaves.Load(), h.ListAgents(), host.Alive())
	}
	if _, err := h.AdoptRuntimeConversation("fake", params); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("adoption after Shutdown = %v", err)
	}
	if driver.calls.Load() != 1 || adoptionSaves.Load() != 0 {
		t.Fatalf("work after stopping: Acquire=%d adoption persist=%d", driver.calls.Load(), adoptionSaves.Load())
	}
}

func TestAdoptConversationFailureLeavesNoAgentEventOrLiveHandle(t *testing.T) {
	h, st, driver, host := adoptionFixture(t)
	defer st.Close()
	driver.acquireHost.(*controlPlaneHost).contract.resumeOutcome = runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: &runtimecontract.Failure{Code: runtimecontract.FailureCodeBindingNotFound, Phase: runtimecontract.FailurePhaseBindingResume, Message: "private /native/path"}}
	_, err := h.AdoptRuntimeConversation("fake", AdoptConversationParams{CandidateID: driver.candidate.ID, ExpectedRevision: driver.candidate.Revision, Name: "failed"})
	if err == nil || strings.Contains(err.Error(), "/native/") {
		t.Fatalf("safe adoption error = %v", err)
	}
	if len(h.ListAgents()) != 0 || host.Alive() {
		t.Fatalf("failure left agents=%#v alive=%t", h.ListAgents(), host.Alive())
	}

	// A persistence failure has the same rollback boundary after native resume
	// and history verification, and remains safe to retry with a fresh handle.
	host.alive = true
	driver.acquireHost.(*controlPlaneHost).contract.resumeOutcome = runtimecontract.Outcome{}
	h.saveAgentsForTest = func(any) error { return errors.New("disk full") }
	_, err = h.AdoptRuntimeConversation("fake", AdoptConversationParams{CandidateID: driver.candidate.ID, ExpectedRevision: driver.candidate.Revision, Name: "failed-save"})
	if err == nil || len(h.ListAgents()) != 0 || host.Alive() {
		t.Fatalf("persistence failure err=%v agents=%#v alive=%t", err, h.ListAgents(), host.Alive())
	}
	h.saveAgentsForTest = nil
	host.alive = true // acquisition returns a new equivalent handle in production
	retried, err := h.AdoptRuntimeConversation("fake", AdoptConversationParams{CandidateID: driver.candidate.ID, ExpectedRevision: driver.candidate.Revision, Name: "failed-save"})
	if err != nil || retried.ID == "" || len(h.ListAgents()) != 1 || !host.Alive() {
		t.Fatalf("retry after persistence failure=%#v err=%v agents=%#v alive=%t", retried, err, h.ListAgents(), host.Alive())
	}
}

func TestCodexConversationAdoptionSurvivesStoreAndDriverRestart(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.144.1'; exit 0; fi
while IFS= read -r line; do
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  [ -z "$id" ] && continue
  case "$line" in
    *'"method":"initialize"'*) printf '{"id":%s,"result":{"userAgent":"adoption-test"}}\n' "$id" ;;
    *'"method":"remoteControl/status/read"'*) printf '{"id":%s,"result":{"status":"disabled","serverName":"local","installationId":"test","environmentId":null}}\n' "$id" ;;
    *'"method":"thread/list"'*) printf '{"id":%s,"result":{"data":[{"id":"thr-adopt","sessionId":"thr-adopt","preview":"existing work","ephemeral":false,"modelProvider":"openai","createdAt":100,"updatedAt":200,"recencyAt":200,"cwd":"/tmp/adopt-work","cliVersion":"0.144.1","source":{"kind":"cli"},"name":"existing-work","turns":[]}],"nextCursor":null,"backwardsCursor":null}}\n' "$id" ;;
    *'"method":"thread/read"'*) printf '{"id":%s,"result":{"thread":{"id":"thr-adopt","sessionId":"thr-adopt","preview":"existing work","ephemeral":false,"modelProvider":"openai","createdAt":100,"updatedAt":200,"recencyAt":200,"cwd":"/tmp/adopt-work","cliVersion":"0.144.1","source":{"kind":"cli"},"name":"existing-work","turns":[]}}}\n' "$id" ;;
    *'"method":"thread/resume"'*) printf '{"id":%s,"result":{"thread":{"id":"thr-adopt"}}}\n' "$id" ;;
    *) printf '{"id":%s,"result":{}}\n' "$id" ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_REMOTE_BIN", bin)
	rolloutDir := t.TempDir()
	writeTestRollout(t, rolloutDir, "thr-adopt", time.Now().UTC().Format(time.RFC3339Nano))
	t.Setenv("CODEX_SESSIONS_DIR", rolloutDir)
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	candidates, err := h.DiscoverRuntimeConversations("codex")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("Codex candidates=%#v err=%v", candidates, err)
	}
	created, err := h.AdoptRuntimeConversation("codex", AdoptConversationParams{CandidateID: candidates[0].ID, ExpectedRevision: candidates[0].Revision, Name: "codex-adopted"})
	if err != nil {
		t.Fatal(err)
	}
	if history, err := h.CanonicalHistory(created.ID, 10, 0); err != nil || history.Total != 1 {
		t.Fatalf("history=%#v err=%v", history, err)
	}
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(reopenedStore)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Shutdown()
	restarted, err := reopened.GetAgent(created.ID)
	if err != nil || restarted.ID != created.ID || restarted.ThreadID != created.ThreadID {
		t.Fatalf("restarted=%#v err=%v", restarted, err)
	}
	if history, err := reopened.CanonicalHistory(created.ID, 10, 0); err != nil || history.Total != 1 {
		t.Fatalf("restarted history=%#v err=%v", history, err)
	}
}

func TestPiConversationAdoptionUsesExistingSessionWithoutCopyingIt(t *testing.T) {
	configureFakePiHubRPC(t, "conformance")
	home := t.TempDir()
	t.Setenv("HOME", home)
	sessionDir := filepath.Join(home, ".pi", "agent", "sessions", "work")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(sessionDir, "existing.jsonl")
	if err := os.WriteFile(sessionPath, []byte(`{"type":"session","version":3,"id":"pi-existing","timestamp":"2026-08-12T00:00:00Z","cwd":"/tmp/work"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	defer h.Shutdown()
	candidates, err := h.DiscoverRuntimeConversations("pi")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("Pi candidates=%#v err=%v", candidates, err)
	}
	created, err := h.AdoptRuntimeConversation("pi", AdoptConversationParams{CandidateID: candidates[0].ID, ExpectedRevision: candidates[0].Revision, Name: "pi-adopted"})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := h.GetRuntimeDiagnostics(created.ID)
	if err != nil || diagnostics.NativeRef != sessionPath {
		t.Fatalf("diagnostics=%#v err=%v", diagnostics, err)
	}
	resumed, err := os.ReadFile(os.Getenv("FAKE_PI_RESUME_FILE"))
	if err != nil || !strings.Contains(string(resumed), sessionPath) {
		t.Fatalf("Pi resume args=%q err=%v", resumed, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(st.Dir(), "pi", created.ID, "session-*.jsonl")); len(matches) != 0 {
		t.Fatalf("adoption copied Pi session: %#v", matches)
	}
}
