package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/codex"
	"github.com/yan5xu/codex-loom/internal/modelcatalog"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	loomskills "github.com/yan5xu/codex-loom/skills"
)

// codexHostRuntime is the single Codex app-server owned by CodexLoom. Threads
// are runtime state inside this host; they are not separate operating-system
// processes. Remote clients join the same app-server and therefore share its
// thread subscriptions with the Hub connection.
type codexHostRuntime struct {
	client     *codex.Client
	ready      chan struct{}
	initErr    error
	generation uint64
	bin        string
	catalogSHA string
	// A mutating Thread RPC that timed out may still complete later. Do not
	// reuse that Thread on the same app-server generation because a retry could
	// duplicate context or work. Replacing the host terminates the old effect
	// domain and starts with an empty fence map.
	indeterminateThreads map[string]threadControlFailure
	closeOnce            sync.Once
}

func (h *codexHostRuntime) close() {
	if h != nil && h.client != nil {
		h.closeOnce.Do(h.client.Close)
	}
}

type threadControlFailure struct {
	Method     string
	ObservedAt string
}

type SkillInventorySkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Scope       string `json:"scope"`
	Enabled     bool   `json:"enabled"`
}

type SkillInventoryError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type SkillInventoryEntry struct {
	Cwd    string                `json:"cwd"`
	Skills []SkillInventorySkill `json:"skills"`
	Errors []SkillInventoryError `json:"errors"`
}

type SkillInventory struct {
	Data   []SkillInventoryEntry      `json:"data"`
	Agents []AgentSkillInventoryEntry `json:"agents"`
}

func (h *Hub) ensureCodexHostLocked() (*codexHostRuntime, error) {
	return h.codexDriverLocked().ensureLocked()
}

func (h *Hub) codexDriverLocked() *codexRuntimeHostDriver {
	if h.codexHostDriver == nil {
		h.codexHostDriver = newCodexRuntimeHostDriver(h)
	}
	if h.runtimeHostDrivers == nil {
		h.runtimeHostDrivers = map[string]hubLockedRuntimeHostDriver{}
	}
	h.runtimeHostDrivers["codex"] = h.codexHostDriver
	return h.codexHostDriver
}

func (d *codexRuntimeHostDriver) ensureLocked() (*codexHostRuntime, error) {
	h := d.hub
	if host := h.codexHost; host != nil && !host.client.Closed() {
		return host, nil
	}
	if h.providerSwitching {
		return nil, errf(409, "CodexHost is restarting for an Agent Provider switch")
	}
	return d.startLocked()
}

func (h *Hub) startCodexHostLocked() (*codexHostRuntime, error) {
	return h.codexDriverLocked().startLocked()
}

func (d *codexRuntimeHostDriver) startLocked() (*codexHostRuntime, error) {
	h := d.hub
	if host := h.codexHost; host != nil && !host.client.Closed() {
		return host, nil
	}
	if err := d.Preflight(context.Background()); err != nil {
		return nil, errf(500, "Codex Runtime preflight: %s", err)
	}
	catalog, err := h.materializeModelCatalog()
	if err != nil {
		return nil, errf(500, "prepare Codex model catalog: %s", err)
	}
	client, err := codex.SpawnWithOptions(codex.SpawnOptions{
		Bin:  codexHostBin(),
		Env:  codexHostEnv(),
		Args: modelcatalog.SpawnArgs(catalog.Path),
	})
	if err != nil {
		return nil, errf(500, "spawn CodexHost: %s", err)
	}
	h.codexHostGeneration++
	host := &codexHostRuntime{
		client:               client,
		ready:                make(chan struct{}),
		generation:           h.codexHostGeneration,
		bin:                  codexHostBin(),
		catalogSHA:           catalog.SHA256,
		indeterminateThreads: map[string]threadControlFailure{},
	}
	client.OnNotification = func(method string, params json.RawMessage) {
		h.onHostNotification(host.generation, method, params)
	}
	client.OnServerRequest = func(id json.RawMessage, method string, params json.RawMessage) {
		h.onHostServerRequest(host.generation, id, method, params)
	}
	client.OnClose = func() { h.onHostClose(host.generation) }
	h.codexHost = host
	if !h.startWorkerLocked(func() { d.initHost(host) }) {
		host.close()
		h.codexHost = nil
		return nil, errf(503, "CodexLoom is shutting down")
	}
	return host, nil
}

// threadControlFailureLocked returns a conservative fence for the current
// CodexHost only. h.mu must be held. A cold host replacement terminates the
// outstanding request and intentionally clears this transient fence.
func (h *Hub) threadControlFailureLocked(threadID string) error {
	host := h.codexHost
	if host == nil || host.client.Closed() || strings.TrimSpace(threadID) == "" {
		return nil
	}
	failure, ok := host.indeterminateThreads[threadID]
	if !ok {
		return nil
	}
	return errf(500, "Codex Thread control outcome is indeterminate after %s timed out at %s; replace the current CodexHost before retrying the same work", failure.Method, failure.ObservedAt)
}

func (h *Hub) markThreadControlIndeterminate(rt *runtime, threadID, method string) {
	if rt == nil || strings.TrimSpace(threadID) == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	host := h.codexHost
	if host == nil || host.generation != rt.hostGeneration || host.client != rt.client {
		return
	}
	if host.indeterminateThreads == nil {
		host.indeterminateThreads = map[string]threadControlFailure{}
	}
	if _, exists := host.indeterminateThreads[threadID]; exists {
		return
	}
	host.indeterminateThreads[threadID] = threadControlFailure{
		Method: strings.TrimSpace(method), ObservedAt: now(),
	}
}

func (h *Hub) verifyRuntimeThreadControl(agentID string, rt *runtime) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta := h.agents[agentID]
	if meta == nil {
		return errf(404, "agent vanished")
	}
	if rt != nil && rt.runtimeContract != nil {
		if rt.effectDomainInvalidated {
			return errf(409, "Runtime binding is fenced after an indeterminate control outcome; restart the Runtime before continuing")
		}
		return nil
	}
	// Compatibility-only v1 runtimes retain their historical fence until the
	// v1 removal ticket deletes this projection.
	if rt != nil && rt.client == nil && runtimeBackend(rt) != nil {
		return nil
	}
	host := h.codexHost
	if host == nil || rt == nil || host.generation != rt.hostGeneration || host.client != rt.client {
		return errf(500, "CodexHost changed before Thread control started")
	}
	return h.threadControlFailureLocked(meta.RuntimeBinding.NativeRef)
}

func (h *Hub) materializeModelCatalog() (modelcatalog.Snapshot, error) {
	dataDir := filepath.Join(os.TempDir(), "codexloom-runtime")
	if h.st != nil {
		if err := h.st.ValidateWritableIdentity(); err != nil {
			return modelcatalog.Snapshot{}, err
		}
		dataDir = h.st.Dir()
	}
	return modelcatalog.Materialize(dataDir, os.Getenv("CODEX_LOOM_MODEL_CATALOG"))
}

func codexHostEnv() map[string]string {
	loomBin := strings.TrimSpace(os.Getenv("CODEX_LOOM_CLI_BIN"))
	if loomBin == "" {
		if executable, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(executable), "loom")
			if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				loomBin = candidate
			}
		}
	}
	if loomBin == "" {
		return nil
	}
	dir := filepath.Dir(loomBin)
	path := os.Getenv("PATH")
	for _, existing := range filepath.SplitList(path) {
		if filepath.Clean(existing) == filepath.Clean(dir) {
			return nil
		}
	}
	if path == "" {
		return map[string]string{"PATH": dir}
	}
	return map[string]string{"PATH": dir + string(os.PathListSeparator) + path}
}

func (h *Hub) ensureCodexHost() (*codexHostRuntime, error) {
	h.mu.Lock()
	host, err := h.ensureCodexHostLocked()
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := waitCodexHost(host); err != nil {
		return nil, errf(500, "CodexHost is not ready: %s", err)
	}
	return host, nil
}

func (d *codexRuntimeHostDriver) initHost(host *codexHostRuntime) {
	h := d.hub
	defer close(host.ready)
	// The client name is a persisted Remote enrollment scope. Keep the legacy
	// wire identity so existing paired devices survive the product rename and
	// the move from a separate Remote process to the shared CodexHost.
	host.initErr = host.client.InitializeAs(codex.ClientInfo{
		Name: "codex-hub-remote", Title: "CodexLoom", Version: "0.1.0",
	})
	if host.initErr != nil {
		host.close()
		return
	}
	if h.st != nil {
		if err := h.st.ValidateWritableIdentity(); err != nil {
			host.initErr = fmt.Errorf("validate builtin Skill store: %w", err)
			host.close()
			return
		}
		skillRoot := filepath.Join(h.st.Dir(), "builtin-skills")
		missing := missingUserSkills()
		if len(missing) == 0 {
			_ = os.RemoveAll(skillRoot)
		} else {
			if _, err := loomskills.MaterializeSelected(skillRoot, missing); err != nil {
				host.initErr = fmt.Errorf("materialize CodexLoom skills: %w", err)
				host.close()
				return
			}
			if _, err := host.client.Request("skills/extraRoots/set", map[string]any{
				"extraRoots": []string{skillRoot},
			}, 20*time.Second); err != nil {
				host.initErr = fmt.Errorf("register CodexLoom skills: %w", err)
				host.close()
				return
			}
		}
	}
	if _, err := h.requestSkillInventory(host); err != nil {
		host.initErr = fmt.Errorf("load CodexLoom skill inventory: %w", err)
		host.close()
		return
	}
	h.mu.Lock()
	switchingProvider := h.providerSwitching
	h.mu.Unlock()
	if !switchingProvider {
		h.hydrateGoals(host)
	}
}

// ReloadSkills forces the shared CodexHost to rebuild its per-Agent skill
// catalogs. It is used after installing a new user skill and when the app-server
// reports that a watched skill root changed.
func (h *Hub) ReloadSkills() (SkillInventory, error) {
	host, err := h.ensureCodexHost()
	if err != nil {
		return SkillInventory{}, err
	}
	inventory, err := h.requestSkillInventory(host)
	if err != nil {
		return SkillInventory{}, errf(500, "reload Codex skills: %s", err)
	}
	return inventory, nil
}

func (h *Hub) requestSkillInventory(host *codexHostRuntime) (SkillInventory, error) {
	params := map[string]any{"forceReload": true}
	h.mu.Lock()
	seen := map[string]bool{}
	cwds := make([]string, 0, len(h.agents))
	for _, agent := range h.agents {
		cwd := strings.TrimSpace(agent.Cwd)
		if cwd != "" && !seen[cwd] {
			seen[cwd] = true
			cwds = append(cwds, cwd)
		}
	}
	h.mu.Unlock()
	if len(cwds) > 0 {
		sort.Strings(cwds)
		params["cwds"] = cwds
	}
	raw, err := host.client.Request("skills/list", params, 30*time.Second)
	if err != nil {
		return SkillInventory{}, err
	}
	var inventory SkillInventory
	if err := json.Unmarshal(raw, &inventory); err != nil {
		return SkillInventory{}, fmt.Errorf("decode skills/list: %w", err)
	}
	h.projectAgentSkillInventory(&inventory)
	return inventory, nil
}

func (h *Hub) reloadSkillsForGeneration(generation uint64) {
	h.mu.Lock()
	host := h.codexHost
	if host == nil || host.generation != generation {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	if err := waitCodexHost(host); err != nil {
		return
	}
	if _, err := h.requestSkillInventory(host); err != nil {
		log.Printf("[codex-loom] refresh skill inventory: %v", err)
	}
}

func missingUserSkills() []string {
	root, err := loomskills.UserRoot()
	if err != nil {
		definitions := loomskills.Definitions()
		names := make([]string, 0, len(definitions))
		for _, definition := range definitions {
			names = append(names, definition.Name)
		}
		return names
	}
	statuses, err := loomskills.Inspect(root, nil)
	if err != nil {
		definitions := loomskills.Definitions()
		names := make([]string, 0, len(definitions))
		for _, definition := range definitions {
			names = append(names, definition.Name)
		}
		return names
	}
	missing := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if status.State == loomskills.StateMissing {
			missing = append(missing, status.Name)
		}
	}
	return missing
}

func waitCodexHost(host *codexHostRuntime) error {
	if host == nil {
		return fmt.Errorf("CodexHost is unavailable")
	}
	<-host.ready
	return host.initErr
}

func notificationThreadID(params json.RawMessage) string {
	var envelope struct {
		ThreadID string `json:"threadId"`
		Thread   struct {
			ID string `json:"id"`
		} `json:"thread"`
		Turn struct {
			ThreadID string `json:"threadId"`
		} `json:"turn"`
		Item struct {
			ThreadID string `json:"threadId"`
		} `json:"item"`
	}
	if json.Unmarshal(params, &envelope) != nil {
		return ""
	}
	for _, candidate := range []string{
		envelope.ThreadID, envelope.Thread.ID, envelope.Turn.ThreadID, envelope.Item.ThreadID,
	} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return candidate
		}
	}
	return ""
}

func notificationTurnID(params json.RawMessage) string {
	var envelope struct {
		TurnID string `json:"turnId"`
		Turn   struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &envelope) != nil {
		return ""
	}
	if turnID := strings.TrimSpace(envelope.TurnID); turnID != "" {
		return turnID
	}
	return strings.TrimSpace(envelope.Turn.ID)
}

func (h *Hub) runtimeForThreadLocked(threadID string) *runtime {
	if threadID == "" {
		return nil
	}
	for id, meta := range h.agents {
		if meta.RuntimeBinding.NativeRef == threadID {
			if rt := h.runtimes[id]; rt != nil {
				return rt
			}
			if h.codexHost == nil || h.codexHost.client.Closed() {
				return nil
			}
			ready := make(chan struct{})
			close(ready)
			handle, host, err := h.codexDriverLocked().acquireLocked(AgentHostRequest{AgentID: id})
			if err != nil {
				return nil
			}
			rt := &runtime{
				agentID: id, agentRuntime: handle.facade, agentHost: handle, runtimeContract: handle.contract,
				client: host.client, hostGeneration: host.generation,
				ready: ready, approvals: map[string]*approval{},
			}
			handle.SetFailureHandler(func(err error) { h.onCodexHostFailure(rt, err) })
			h.bindCodexContract(meta, rt, handle.contract)
			h.runtimes[id] = rt
			return rt
		}
	}
	return nil
}

func (h *Hub) bindOrAdoptStartedThreadLocked(params json.RawMessage) *runtime {
	var event struct {
		Thread struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Cwd  string `json:"cwd"`
		} `json:"thread"`
	}
	if json.Unmarshal(params, &event) != nil || strings.TrimSpace(event.Thread.ID) == "" {
		return nil
	}
	threadID := strings.TrimSpace(event.Thread.ID)
	if rt := h.runtimeForThreadLocked(threadID); rt != nil {
		return rt
	}

	// A locally requested thread/start can notify before its JSON-RPC response.
	// Bind that pending Agent first so it is not mistaken for a Remote-created
	// Agent. Cwd disambiguates concurrent creates in normal use.
	var pending *runtime
	pendingCount := 0
	for id, rt := range h.runtimes {
		meta := h.agents[id]
		if meta == nil || meta.RuntimeBinding.NativeRef != "" || rt.hostGeneration != h.codexHost.generation {
			continue
		}
		if event.Thread.Cwd != "" && meta.Cwd != event.Thread.Cwd {
			continue
		}
		pendingCount++
		pending = rt
	}
	if pendingCount == 1 {
		if meta := h.agents[pending.agentID]; meta != nil {
			previous := *meta
			meta.RuntimeBinding.NativeRef = threadID
			meta.UpdatedAt = now()
			if err := h.persistAgentsLocked(); err != nil {
				*meta = previous
				log.Printf("[codex-loom] persist pending Thread binding %s: %v", threadID, err)
				return nil
			}
		}
		return pending
	}
	if pendingCount > 1 {
		// The matching thread/start response will bind the right Agent. Adopting
		// an ambiguous notification here would create a duplicate Remote Agent.
		return nil
	}
	return h.adoptThreadLocked(threadID, event.Thread.Name, event.Thread.Cwd)
}

func (h *Hub) adoptThreadLocked(threadID, threadName, cwd string) *runtime {
	if rt := h.runtimeForThreadLocked(threadID); rt != nil {
		return rt
	}
	name := strings.TrimSpace(threadName)
	if !nameRe.MatchString(name) {
		short := strings.ReplaceAll(threadID, "-", "")
		if len(short) > 8 {
			short = short[len(short)-8:]
		}
		name = "remote-" + short
	}
	base := name
	for suffix := 2; h.resolveLocked(name) != nil; suffix++ {
		name = fmt.Sprintf("%s-%d", base, suffix)
	}
	idBytes := make([]byte, 4)
	_, _ = rand.Read(idBytes)
	id := hex.EncodeToString(idBytes)
	meta := &Agent{
		ID: id, Name: name, Cwd: cwd, ThreadID: newIntegrationID("thr"),
		RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: threadID},
		Sandbox:        "danger-full-access", ApprovalPolicy: "never", Status: "idle",
		CreatedAt: now(), UpdatedAt: now(), Source: "remote",
	}
	h.agents[id] = meta
	h.seqs[id] = h.st.LastSeq(id)
	if err := h.persistAgentsLocked(); err != nil {
		delete(h.agents, id)
		delete(h.seqs, id)
		log.Printf("[codex-loom] persist adopted Thread %s: %v", threadID, err)
		return nil
	}
	ready := make(chan struct{})
	close(ready)
	handle, host, err := h.codexDriverLocked().acquireLocked(AgentHostRequest{AgentID: id})
	if err != nil {
		return nil
	}
	rt := &runtime{
		agentID: id, agentRuntime: handle.facade, agentHost: handle, runtimeContract: handle.contract,
		client: host.client, hostGeneration: host.generation,
		ready: ready, approvals: map[string]*approval{},
	}
	handle.SetFailureHandler(func(err error) { h.onCodexHostFailure(rt, err) })
	h.bindCodexContract(meta, rt, handle.contract)
	h.runtimes[id] = rt
	h.emitLocked(id, "loom/agent-created", map[string]any{
		"id": id, "name": name, "cwd": meta.Cwd, "threadId": meta.ThreadID, "runtimeKind": "codex", "source": "remote",
	})
	h.emitStatusLocked(meta, meta.Status)
	return rt
}

func (h *Hub) onHostNotification(generation uint64, method string, params json.RawMessage) {
	if method == "remoteControl/status/changed" {
		h.onRemoteNotification(generation, method, params)
		return
	}
	if method == "skills/changed" {
		h.startWorker(func() { h.reloadSkillsForGeneration(generation) })
		return
	}
	threadID := notificationThreadID(params)
	h.mu.Lock()
	if h.codexHost == nil || h.codexHost.generation != generation {
		h.mu.Unlock()
		return
	}
	rt := h.runtimeForThreadLocked(threadID)
	if rt != nil {
		if meta := h.agents[rt.agentID]; recoveryEventFenced(meta, rt) {
			h.mu.Unlock()
			return
		}
	}
	hydrateAgentID := ""
	if rt == nil && method == "thread/started" {
		rt = h.bindOrAdoptStartedThreadLocked(params)
	} else if rt == nil && method == "turn/started" && threadID != "" {
		// Remote may resume a pre-existing Codex Thread without emitting a
		// thread/started notification on this connection. Adopt it before the
		// following Item notifications arrive so WebUI/CLI stay live.
		rt = h.adoptThreadLocked(threadID, "", "")
		if rt != nil {
			hydrateAgentID = rt.agentID
		}
	}
	if rt != nil && method == "turn/started" && (rt.activeTurn == nil || rt.activeTurn.finished) {
		if nativeTurnID := notificationTurnID(params); nativeTurnID != "" {
			if meta := h.agents[rt.agentID]; meta != nil {
				turn := h.adoptRemoteTurnLocked(meta, rt, nativeTurnID)
				if contract, ok := rt.runtimeContract.(*codexRuntimeContract); ok {
					contract.bindTurn(turn.turnID, "", nativeTurnID)
				}
			}
		}
	}
	driver := h.codexHostDriver
	h.mu.Unlock()
	if hydrateAgentID != "" {
		h.startWorker(func() { h.hydrateAdoptedAgent(generation, hydrateAgentID, threadID) })
	}
	if rt != nil {
		canonical := false
		if driver != nil {
			canonical = driver.dispatchNativeEvent(rt.agentID, method, params)
		}
		h.onCodexCompatibilityNotification(rt, method, params, canonical)
	}
}

func (h *Hub) bindCodexContract(meta *Agent, rt *runtime, contract *codexRuntimeContract) {
	if contract == nil {
		return
	}
	for turnID, nativeTurnID := range meta.RuntimeTurnBindings {
		contract.bindTurn(turnID, "", nativeTurnID)
	}
	contract.SetEventHandler(func(event runtimecontract.Event) { h.onCanonicalRuntimeEvent(rt, event) })
}

func (h *Hub) onCodexCompatibilityNotification(rt *runtime, method string, params json.RawMessage, canonical bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta := h.agents[rt.agentID]
	if meta == nil {
		return
	}
	nativeTurnID := notificationTurnID(params)
	if rt.activeTurn != nil && !rt.activeTurn.finished && nativeTurnID != "" {
		if contract, ok := rt.runtimeContract.(*codexRuntimeContract); ok {
			if loomTurnID := contract.turnIDForNative(nativeTurnID); loomTurnID != "" && loomTurnID != rt.activeTurn.turnID {
				return
			}
		}
	}
	// These native-only notifications remain compatibility side effects until
	// their dedicated Runtime Capabilities migrate to typed events.
	if method == "error" && rt.activeTurn != nil && !rt.activeTurn.finished && rt.activeTurn.forcedFailure == "" {
		if failure, interrupt := customProviderModelRouteFailure(meta.ProviderID, meta.Model, params); failure != "" {
			rt.activeTurn.forcedFailure = failure
			if interrupt {
				h.scheduleModelRouteInterruptLocked(meta.ID, rt.activeTurn, failure)
			}
		}
	}
	if method == "thread/goal/updated" || method == "thread/goal/cleared" {
		h.onGoalNotificationLocked(meta.ID, method, params)
	}
	turnID := ""
	if rt.activeTurn != nil {
		turnID = rt.activeTurn.turnID
	}
	h.emitLocked(meta.ID, method, runtimePublicPayload(params, meta.ThreadID, turnID, true))
	h.appendRuntimeDiagnosticLocked(meta.ID, method, params)
}

func recoveryEventFenced(meta *Agent, rt *runtime) bool {
	if rt != nil && rt.effectDomainInvalidated {
		return true
	}
	if meta == nil || meta.LastTurn == nil {
		return false
	}
	marker, ok := meta.TurnRecoveryMarkers[meta.LastTurn.TurnID]
	if !ok || marker.State == TurnRecoveryCompleted {
		return false
	}
	return rt == nil || rt.activeTurn == nil || rt.activeTurn.finished || rt.activeTurn.turnID == marker.PredecessorTurnID
}

func (h *Hub) hydrateAdoptedAgent(generation uint64, agentID, threadID string) {
	h.mu.Lock()
	if h.codexHost == nil || h.codexHost.generation != generation {
		h.mu.Unlock()
		return
	}
	client := h.codexHost.client
	h.mu.Unlock()

	raw, err := client.Request("thread/read", map[string]any{
		"threadId": threadID, "includeTurns": false,
	}, 15*time.Second)
	if err != nil {
		log.Printf("[codex-loom] hydrate Remote Thread %s: %v", threadID, err)
		return
	}
	var result struct {
		Thread struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Cwd  string `json:"cwd"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Thread.ID != threadID {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.codexHost == nil || h.codexHost.generation != generation {
		return
	}
	agent := h.agents[agentID]
	if agent == nil || agent.RuntimeBinding.NativeRef != threadID || agent.Source != "remote" {
		return
	}
	previous := *agent
	changed := false
	if cwd := strings.TrimSpace(result.Thread.Cwd); cwd != "" && cwd != agent.Cwd {
		agent.Cwd = cwd
		changed = true
	}
	if name := strings.TrimSpace(result.Thread.Name); nameRe.MatchString(name) && strings.HasPrefix(agent.Name, "remote-") {
		if existing := h.resolveLocked(name); existing == nil || existing.ID == agent.ID {
			agent.Name = name
			changed = true
		}
	}
	if changed {
		agent.UpdatedAt = now()
		if err := h.persistAgentsLocked(); err != nil {
			*agent = previous
			log.Printf("[codex-loom] persist hydrated Agent %s: %v", agentID, err)
			return
		}
		h.emitStatusLocked(agent, agent.Status)
	}
}

func (h *Hub) onHostServerRequest(generation uint64, id json.RawMessage, method string, params json.RawMessage) {
	threadID := notificationThreadID(params)
	h.mu.Lock()
	if h.codexHost == nil || h.codexHost.generation != generation {
		h.mu.Unlock()
		return
	}
	rt := h.runtimeForThreadLocked(threadID)
	if rt == nil {
		// Older approval payloads may omit threadId. An app-server only has one
		// active turn per thread, so route to the sole active Loom thread.
		for _, candidate := range h.runtimes {
			if candidate.activeTurn == nil || candidate.activeTurn.finished {
				continue
			}
			if rt != nil {
				rt = nil
				break
			}
			rt = candidate
		}
	}
	client := h.codexHost.client
	h.mu.Unlock()
	if rt != nil {
		h.onServerRequest(rt, id, method, params)
		return
	}
	_ = client.RespondError(id, -32601, "CodexLoom cannot route "+method+" without a threadId")
}

func (h *Hub) onHostClose(generation uint64) {
	h.mu.Lock()
	driver := h.codexHostDriver
	valid := h.codexHost != nil && h.codexHost.generation == generation
	h.mu.Unlock()
	if valid && driver != nil {
		driver.fanoutHostFailure(generation, fmt.Errorf("CodexHost exited"))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.codexHost == nil || h.codexHost.generation != generation {
		return
	}
	h.codexHost = nil
	h.remoteRuntime = nil
	for id, rt := range h.runtimes {
		if rt.hostGeneration != generation {
			continue
		}
		delete(h.runtimes, id)
	}
	if h.remoteConfig.Enabled {
		h.remoteStatus.State = "error"
		h.remoteStatus.LastError = "CodexHost exited"
		h.remoteStatus.UpdatedAt = now()
		h.remoteEnabledGeneration = 0
		h.emitRemoteLocked()
	}
}

func (h *Hub) onCodexHostFailure(rt *runtime, err error) {
	if rt == nil || err == nil {
		return
	}
	h.mu.Lock()
	meta := h.agents[rt.agentID]
	if meta == nil || rt.activeTurn == nil || rt.activeTurn.finished {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	h.onRuntimeFailure(rt, err)
}

// invalidateRuntimeEffectDomain terminates the transport generation that may
// still deliver a late mutating result. Pi owns one process per Agent; Codex
// fans the shared generation failure out once before closing the host.
func (h *Hub) invalidateRuntimeEffectDomain(rt *runtime, cause error) {
	if rt == nil {
		return
	}
	if rt.client == nil {
		h.mu.Lock()
		rt.effectDomainInvalidated = true
		h.mu.Unlock()
		if backend := runtimeBackend(rt); backend != nil {
			backend.Close()
		}
		return
	}
	type recoveryJob struct {
		agentID, predecessorTurnID string
	}
	h.mu.Lock()
	rt.effectDomainInvalidated = true
	host := h.codexHost
	driver := h.codexHostDriver
	valid := host != nil && host.generation == rt.hostGeneration && host.client == rt.client
	var affected []*runtime
	if valid {
		for _, candidate := range h.runtimes {
			if candidate == nil || candidate.hostGeneration != host.generation || candidate.client != host.client {
				continue
			}
			candidate.effectDomainInvalidated = true
			if candidate != rt && candidate.activeTurn != nil && !candidate.activeTurn.finished {
				affected = append(affected, candidate)
			}
		}
		// Fence the generation before Close can deliver its asynchronous exit
		// callback. Keep per-Agent projections long enough for the recovery
		// inspectors; lazy runtime acquisition replaces their closed backend.
		h.codexHost = nil
		h.remoteRuntime = nil
		h.remoteEnabledGeneration = 0
	}
	h.mu.Unlock()
	if !valid {
		return
	}
	sharedCause := fmt.Errorf("CodexHost effect domain invalidated after indeterminate command: %w", cause)
	jobs := make([]recoveryJob, 0, len(affected))
	for _, candidate := range affected {
		agentID, predecessorTurnID, checkpointed := h.checkpointRuntimeFailure(candidate, sharedCause)
		if checkpointed {
			jobs = append(jobs, recoveryJob{agentID: agentID, predecessorTurnID: predecessorTurnID})
		}
	}
	if driver != nil {
		driver.suppressHostFailureFanout(host.generation)
	}
	if err := closeCodexHost(host); err != nil {
		log.Printf("[codex-loom] close invalidated CodexHost generation %d: %v", host.generation, err)
	}
	h.mu.Lock()
	if h.remoteConfig.Enabled {
		h.remoteStatus.State = "error"
		h.remoteStatus.LastError = "CodexHost effect domain was invalidated"
		h.remoteStatus.UpdatedAt = now()
		h.emitRemoteLocked()
	}
	h.mu.Unlock()
	for _, job := range jobs {
		h.mu.Lock()
		h.scheduleTurnRecoveryLocked(job.agentID, job.predecessorTurnID)
		h.mu.Unlock()
	}
}
