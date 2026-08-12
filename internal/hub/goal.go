package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

const (
	GoalStatusActive        = "active"
	GoalStatusPaused        = "paused"
	GoalStatusBlocked       = "blocked"
	GoalStatusUsageLimited  = "usageLimited"
	GoalStatusBudgetLimited = "budgetLimited"
	GoalStatusComplete      = "complete"
)

// ThreadGoal is Loom's durable Goal aggregate. Native Runtime Goal state is an
// optional synchronization projection and never owns this record.
type ThreadGoal struct {
	ID              string `json:"id"`
	Version         int64  `json:"version"`
	ThreadID        string `json:"threadId"`
	Objective       string `json:"objective"`
	Status          string `json:"status"`
	TokenBudget     *int64 `json:"tokenBudget"`
	TokensUsed      int64  `json:"tokensUsed"`
	TimeUsedSeconds int64  `json:"timeUsedSeconds"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
	ClearedAt       int64  `json:"clearedAt,omitempty"`
	NativeSyncState string `json:"nativeSyncState,omitempty"`
	NativeSyncedAt  int64  `json:"nativeSyncedAt,omitempty"`
	NativeSyncError string `json:"nativeSyncError,omitempty"`
	// NativeSyncBindingRevision scopes evidence to one Runtime binding. It is
	// a non-secret digest; evidence from a replaced native conversation is not
	// proof about the replacement.
	NativeSyncBindingRevision string `json:"nativeSyncBindingRevision,omitempty"`
	// NativeMigrationBlocked is set only while an imported active Codex Goal
	// has not been durably neutralized. Ordinary native shadow failures are
	// evidence-only and never control Loom continuation.
	NativeMigrationBlocked         bool   `json:"nativeMigrationBlocked,omitempty"`
	NativeMigrationBindingRevision string `json:"nativeMigrationBindingRevision,omitempty"`
}

func (h *Hub) persistGoalsLocked() error { return h.st.SaveGoals(h.goals) }

func (h *Hub) saveGoalRecordLocked(agentID string, next *ThreadGoal) error {
	records := make(map[string]*ThreadGoal, len(h.goals)+1)
	for id, goal := range h.goals {
		records[id] = goal
	}
	if next == nil {
		delete(records, agentID)
	} else {
		records[agentID] = next
	}
	if err := h.st.SaveGoals(records); err != nil {
		return err
	}
	h.goals = records
	return nil
}

type GoalUpdateParams struct {
	Objective        *string `json:"objective"`
	Status           *string `json:"status"`
	TokenBudget      *int64  `json:"tokenBudget"`
	ClearTokenBudget bool    `json:"clearTokenBudget"`
	ExpectedVersion  *int64  `json:"expectedVersion"`
}

type threadGoalGetResponse struct {
	Goal *ThreadGoal `json:"goal"`
}

type threadGoalSetResponse struct {
	Goal ThreadGoal `json:"goal"`
}

func goalUpdateRuntimeParams(update GoalUpdateParams) map[string]any {
	params := map[string]any{}
	if update.Objective != nil {
		params["objective"] = strings.TrimSpace(*update.Objective)
	}
	if update.Status != nil {
		params["status"] = strings.TrimSpace(*update.Status)
	}
	if update.TokenBudget != nil {
		params["tokenBudget"] = *update.TokenBudget
	} else if update.ClearTokenBudget {
		params["tokenBudget"] = nil
	}
	return params
}

const (
	goalNativeSyncPending       = "pending"
	goalNativeSyncSynced        = "synced"
	goalNativeSyncFailed        = "failed"
	goalNativeSyncNotApplicable = "notApplicable"
)

func validGoalStatus(status string) bool {
	switch status {
	case GoalStatusActive, GoalStatusPaused, GoalStatusBlocked, GoalStatusUsageLimited, GoalStatusBudgetLimited, GoalStatusComplete:
		return true
	default:
		return false
	}
}

func goalBindingRevision(binding runtimecontract.Binding) string {
	digest := sha256Hex([]byte(fmt.Sprint(binding.SchemaVersion) + "\x00" + binding.RuntimeKind + "\x00" + binding.NativeRef))
	return "binding:" + digest[:16]
}

func goalMigrationBlockedForBinding(goal *ThreadGoal, binding runtimecontract.Binding) bool {
	if goal == nil || !goal.NativeMigrationBlocked || binding.RuntimeKind != "codex" {
		return false
	}
	revision := goalBindingRevision(binding)
	return goal.NativeMigrationBindingRevision == "" || goal.NativeMigrationBindingRevision == revision
}

func goalMigrationBlockedForAgent(goal *ThreadGoal, agent *Agent) bool {
	return agent != nil && goalMigrationBlockedForBinding(goal, runtimeContractBinding(agent))
}

// activeGoalReservesThreadLocked reports whether automatic Goal continuation
// currently owns the next Turn. A paused, blocked, or limited Goal remains
// durable and visible, but it must not starve the Agent's Inbox.
func (h *Hub) activeGoalReservesThreadLocked(agentID string) bool {
	goal := h.goals[agentID]
	return goal != nil && goal.ClearedAt == 0 && goal.Status == GoalStatusActive
}

func (h *Hub) goalContinuationReadyLocked(agentID string) bool {
	if !h.activeGoalReservesThreadLocked(agentID) {
		return false
	}
	agent := h.agents[agentID]
	if agent == nil || agent.Status != "idle" || goalMigrationBlockedForAgent(h.goals[agentID], agent) {
		return false
	}
	for key := range h.turnRecoveryInFlight {
		if strings.HasPrefix(key, agentID+"\x00") {
			return false
		}
	}
	for _, marker := range agent.TurnRecoveryMarkers {
		if marker.State != TurnRecoveryCompleted {
			return false
		}
	}
	return true
}

// ActiveGoalAgentIDs returns the stable identities whose Loom Goals currently
// own automatic continuation. It is used by graceful restart to
// stop at a Turn boundary instead of waiting forever for an active Goal.
func (h *Hub) ActiveGoalAgentIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0)
	for agentID, goal := range h.goals {
		if h.agents[agentID] != nil && goal != nil && goal.ClearedAt == 0 && goal.Status == GoalStatusActive {
			ids = append(ids, agentID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (h *Hub) goalStartupAgentIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0)
	missingCodexGoal := make([]string, 0)
	codexHostWillStart := false
	for agentID, agent := range h.agents {
		goal := h.goals[agentID]
		if goal != nil && goal.ClearedAt == 0 && goal.Status == GoalStatusActive {
			ids = append(ids, agentID)
			codexHostWillStart = codexHostWillStart || agent != nil && agent.RuntimeBinding.Kind == "codex"
		} else if goal == nil && agent != nil && agent.RuntimeBinding.Kind == "codex" {
			missingCodexGoal = append(missingCodexGoal, agentID)
		}
	}
	sort.Strings(ids)
	sort.Strings(missingCodexGoal)
	// One shared Codex Host hydration pass imports every legacy native Goal.
	// Starting every Goal-less Codex Agent only creates redundant resume races.
	if !codexHostWillStart && len(missingCodexGoal) != 0 {
		ids = append(ids, missingCodexGoal[0])
		sort.Strings(ids)
	}
	return ids
}

// PauseGoalsForRestart pauses only Goals that are still active when each
// update reaches Codex. The active Turn is not interrupted; changing the Goal
// status only prevents Codex from immediately starting its next continuation.
func (h *Hub) PauseGoalsForRestart(agentIDs []string) ([]string, error) {
	paused := make([]string, 0, len(agentIDs))
	for _, agentID := range uniqueSortedStrings(agentIDs) {
		goal, err := h.GetGoal(agentID)
		if err != nil {
			return paused, fmt.Errorf("read Goal for %s: %w", agentID, err)
		}
		if goal == nil || goal.Status != GoalStatusActive {
			continue
		}
		status := GoalStatusPaused
		updated, err := h.UpdateGoal(agentID, GoalUpdateParams{Status: &status})
		if err != nil {
			return paused, fmt.Errorf("pause Goal for %s: %w", agentID, err)
		}
		if updated == nil || updated.Status != GoalStatusPaused {
			return paused, fmt.Errorf("pause Goal for %s returned status %q", agentID, goalStatus(updated))
		}
		paused = append(paused, agentID)
	}
	return paused, nil
}

// ResumeGoalsAfterRestart resumes only Goals that remain paused. A Goal that
// completed, was cleared, or was otherwise changed while its final Turn
// drained is left untouched. Repeating this operation is therefore safe.
func (h *Hub) ResumeGoalsAfterRestart(agentIDs []string) error {
	var errs []error
	for _, agentID := range uniqueSortedStrings(agentIDs) {
		goal, err := h.GetGoal(agentID)
		if err != nil {
			var hubErr *HubError
			if errors.As(err, &hubErr) && hubErr.Status == 404 {
				continue
			}
			errs = append(errs, fmt.Errorf("read Goal for %s: %w", agentID, err))
			continue
		}
		if goal == nil || goal.Status != GoalStatusPaused {
			continue
		}
		status := GoalStatusActive
		updated, err := h.UpdateGoal(agentID, GoalUpdateParams{Status: &status})
		if err != nil {
			errs = append(errs, fmt.Errorf("resume Goal for %s: %w", agentID, err))
			continue
		}
		if updated == nil || updated.Status != GoalStatusActive {
			errs = append(errs, fmt.Errorf("resume Goal for %s returned status %q", agentID, goalStatus(updated)))
		}
	}
	return errors.Join(errs...)
}

func goalStatus(goal *ThreadGoal) string {
	if goal == nil {
		return ""
	}
	return goal.Status
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

type goalHydrationTarget struct {
	agentID            string
	threadID           string
	loomThreadID       string
	sandbox            string
	provider           string
	model              string
	effort             string
	disabledSkillPaths []string
}

func (h *Hub) hydrateGoals(host *codexHostRuntime) {
	h.mu.Lock()
	targets := make([]goalHydrationTarget, 0, len(h.agents))
	for _, agent := range h.agents {
		if agent.RuntimeBinding.Kind != "codex" || strings.TrimSpace(agent.RuntimeBinding.NativeRef) == "" {
			continue
		}
		providerID, model := effectiveProviderBinding(agent)
		targets = append(targets, goalHydrationTarget{
			agentID: agent.ID, threadID: agent.RuntimeBinding.NativeRef, loomThreadID: agent.ThreadID, sandbox: agent.Sandbox,
			provider: providerID, model: model, effort: agent.Effort,
			disabledSkillPaths: h.disabledSkillPathsLocked(agent.ID),
		})
	}
	h.mu.Unlock()

	for _, target := range targets {
		contract := host.agentContract(target.agentID)
		configureRuntimeBinding(contract, target.sandbox, target.provider, target.model, target.effort, target.disabledSkillPaths)
		capability, ok := contract.(runtimeGoalCapability)
		if !ok {
			log.Printf("[codex-loom] hydrate Goal for %s: Runtime Goal capability is unavailable", target.threadID)
			continue
		}
		h.hydrateGoal(target, capability)
	}
}

func (h *Hub) hydrateGoal(target goalHydrationTarget, capability runtimeGoalCapability) {
	binding := runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "codex", NativeRef: target.threadID}
	bindingRevision := goalBindingRevision(binding)
	h.mu.Lock()
	current := cloneGoalRecord(h.goals[target.agentID])
	h.mu.Unlock()
	if current == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		nativeGoal, err := capability.RuntimeGoal(ctx, binding)
		cancel()
		if err != nil {
			log.Printf("[codex-loom] hydrate Goal for %s: %v", target.threadID, err)
			return
		}
		current = nativeGoal
		nowMillis := time.Now().UnixMilli()
		if current == nil {
			current = &ThreadGoal{ID: newIntegrationID("goal"), Version: 1, ThreadID: target.loomThreadID, ClearedAt: nowMillis}
		} else {
			current.ID, current.Version = newIntegrationID("goal"), 1
			current.ThreadID = target.loomThreadID
			current.NativeMigrationBlocked = current.ClearedAt == 0 && current.Status == GoalStatusActive
			if current.NativeMigrationBlocked {
				current.NativeMigrationBindingRevision = bindingRevision
			}
			if current.CreatedAt == 0 {
				current.CreatedAt = nowMillis
			}
			if current.UpdatedAt == 0 {
				current.UpdatedAt = nowMillis
			}
		}
		current.NativeSyncState = goalNativeSyncPending
		current.NativeSyncBindingRevision = bindingRevision
		h.mu.Lock()
		if h.goals[target.agentID] == nil {
			h.goals[target.agentID] = cloneGoalRecord(current)
			if err := h.persistGoalsLocked(); err != nil {
				delete(h.goals, target.agentID)
				h.mu.Unlock()
				log.Printf("[codex-loom] persist imported Goal for %s: %v", target.threadID, err)
				return
			}
			if agent := h.agents[target.agentID]; agent != nil {
				h.emitStatusLocked(agent, agent.Status)
			}
		} else {
			current = cloneGoalRecord(h.goals[target.agentID])
		}
		h.mu.Unlock()
	}

	// Persisted Loom truth exists before the legacy native active loop is
	// neutralized. Loom dispatch is withheld unless neutralization succeeds.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	err := syncNativeGoalProjection(ctx, capability, binding, current)
	cancel()
	h.mu.Lock()
	persisted := h.goals[target.agentID]
	if persisted == nil || persisted.ID != current.ID || persisted.Version != current.Version {
		h.mu.Unlock()
		return
	}
	next := cloneGoalRecord(persisted)
	if err == nil {
		next.NativeSyncedAt = time.Now().UnixMilli()
		next.NativeSyncState, next.NativeSyncError = goalNativeSyncSynced, ""
		next.NativeMigrationBlocked = false
		next.NativeMigrationBindingRevision = ""
	} else {
		next.NativeSyncedAt = 0
		next.NativeSyncState, next.NativeSyncError = goalNativeSyncFailed, boundedDisplayTask(redactRuntimeDiagnosticString(err.Error()), 320)
		if next.NativeMigrationBlocked && next.NativeMigrationBindingRevision == "" {
			next.NativeMigrationBindingRevision = bindingRevision
		}
	}
	next.NativeSyncBindingRevision = bindingRevision
	if persistErr := h.saveGoalRecordLocked(target.agentID, next); persistErr != nil {
		log.Printf("[codex-loom] persist native Goal sync evidence: %v", persistErr)
		h.mu.Unlock()
		return
	}
	if agent := h.agents[target.agentID]; agent != nil {
		h.emitStatusLocked(agent, agent.Status)
	}
	h.mu.Unlock()
}

func syncNativeGoalProjection(ctx context.Context, capability runtimeGoalCapability, binding runtimecontract.Binding, goal *ThreadGoal) error {
	if goal == nil {
		return nil
	}
	if goal.ClearedAt != 0 {
		_, err := capability.ClearRuntimeGoal(ctx, binding)
		return err
	}
	objective, paused := goal.Objective, GoalStatusPaused
	projected, err := capability.UpdateRuntimeGoal(ctx, binding, GoalUpdateParams{
		Objective: &objective, Status: &paused, TokenBudget: goal.TokenBudget, ClearTokenBudget: goal.TokenBudget == nil,
	})
	if err == nil && (projected == nil || projected.Status != GoalStatusPaused) {
		return errors.New("native Goal pause was not confirmed")
	}
	if err == nil && (projected.Objective != goal.Objective || !sameOptionalInt64(projected.TokenBudget, goal.TokenBudget)) {
		return errors.New("native Goal shadow did not match the Loom Goal")
	}
	return err
}

func sameOptionalInt64(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func (h *Hub) GetGoal(key string) (*ThreadGoal, error) {
	goal, _, err := h.GetGoalState(key)
	return goal, err
}

func (h *Hub) GetGoalState(key string) (*ThreadGoal, int64, error) {
	h.mu.Lock()
	agent := h.resolveLocked(key)
	if agent == nil {
		h.mu.Unlock()
		return nil, 0, errf(404, "agent not found: %s", key)
	}
	record := h.goals[agent.ID]
	goal, revision := cloneGoalForAgent(record, agent), int64(0)
	if record != nil {
		revision = record.Version
	}
	h.mu.Unlock()
	return goal, revision, nil
}

func (h *Hub) UpdateGoal(key string, update GoalUpdateParams) (*ThreadGoal, error) {
	params := map[string]any{}
	if update.Objective != nil {
		objective := strings.TrimSpace(*update.Objective)
		if objective == "" {
			return nil, errf(400, "goal objective is required")
		}
		if len(objective) > 4000 {
			return nil, errf(400, "goal objective must be at most 4000 characters")
		}
		params["objective"] = objective
	}
	if update.Status != nil {
		status := strings.TrimSpace(*update.Status)
		if !validGoalStatus(status) {
			return nil, errf(400, "invalid goal status: %s", status)
		}
		params["status"] = status
	}
	if update.TokenBudget != nil {
		if *update.TokenBudget <= 0 {
			return nil, errf(400, "goal token budget must be positive")
		}
		params["tokenBudget"] = *update.TokenBudget
	} else if update.ClearTokenBudget {
		params["tokenBudget"] = nil
	}
	if len(params) == 0 {
		return nil, errf(400, "goal objective, status, or token budget is required")
	}

	h.mu.Lock()
	agent := h.resolveLocked(key)
	if agent == nil {
		h.mu.Unlock()
		return nil, errf(404, "agent not found: %s", key)
	}
	if err := h.runtimeMutationAllowedLocked(agent.ID); err != nil {
		h.mu.Unlock()
		return nil, err
	}
	agentID := agent.ID
	current := h.goals[agentID]
	currentVersion := int64(0)
	if current != nil {
		currentVersion = current.Version
	}
	if update.ExpectedVersion != nil && *update.ExpectedVersion != currentVersion {
		h.mu.Unlock()
		return nil, errf(409, "Goal version changed: expected %d, current %d", *update.ExpectedVersion, currentVersion)
	}
	nowMillis := time.Now().UnixMilli()
	binding := runtimeContractBinding(agent)
	bindingRevision := goalBindingRevision(binding)
	next := ThreadGoal{ID: newIntegrationID("goal"), Version: currentVersion + 1, ThreadID: agent.ThreadID, Status: GoalStatusActive, CreatedAt: nowMillis, UpdatedAt: nowMillis}
	if current != nil && current.ClearedAt == 0 {
		next = *current
		next.Version++
		next.UpdatedAt = nowMillis
	}
	if current != nil && goalMigrationBlockedForBinding(current, binding) {
		next.NativeMigrationBlocked = true
		next.NativeMigrationBindingRevision = current.NativeMigrationBindingRevision
		if next.NativeMigrationBindingRevision == "" {
			next.NativeMigrationBindingRevision = bindingRevision
		}
	} else {
		next.NativeMigrationBlocked = false
		next.NativeMigrationBindingRevision = ""
	}
	if objective, ok := params["objective"].(string); ok {
		next.Objective = objective
	}
	if next.Objective == "" {
		h.mu.Unlock()
		return nil, errf(400, "goal objective is required")
	}
	if status, ok := params["status"].(string); ok {
		next.Status = status
	}
	if budget, exists := params["tokenBudget"]; exists {
		if budget == nil {
			next.TokenBudget = nil
		} else {
			value := budget.(int64)
			next.TokenBudget = &value
		}
	}
	next.ClearedAt = 0
	next.NativeSyncError = ""
	next.NativeSyncedAt = 0
	if agent.RuntimeBinding.Kind == "codex" {
		next.NativeSyncState = goalNativeSyncPending
		next.NativeSyncBindingRevision = bindingRevision
	} else {
		next.NativeSyncState = goalNativeSyncNotApplicable
		next.NativeSyncBindingRevision = ""
	}
	wasReserved := h.activeGoalReservesThreadLocked(agentID)
	previous := cloneGoalRecord(current)
	h.goals[agentID] = &next
	if err := h.persistGoalsLocked(); err != nil {
		if previous == nil {
			delete(h.goals, agentID)
		} else {
			h.goals[agentID] = previous
		}
		h.mu.Unlock()
		return nil, errf(500, "save Goal: %s", err)
	}
	h.emitStatusLocked(agent, agent.Status)
	if wasReserved && next.Status != GoalStatusActive {
		h.startPendingWorkersLocked(agentID)
	}
	h.mu.Unlock()

	h.syncNativeGoal(agentID, next.ID, next.Version, false)
	h.mu.Lock()
	continueReady := h.goalContinuationReadyLocked(agentID) && h.goals[agentID].ID == next.ID && h.goals[agentID].Version == next.Version
	h.mu.Unlock()
	if continueReady {
		h.startWorker(func() { h.continueGoal(agentID) })
	}
	h.mu.Lock()
	goal := cloneGoalForAgent(h.goals[agentID], h.agents[agentID])
	h.mu.Unlock()
	return goal, nil
}

func (h *Hub) ClearGoal(key string) (bool, error) {
	return h.ClearGoalVersion(key, nil)
}

func (h *Hub) ClearGoalVersion(key string, expectedVersion *int64) (bool, error) {
	h.mu.Lock()
	agent := h.resolveLocked(key)
	if agent == nil {
		h.mu.Unlock()
		return false, errf(404, "agent not found: %s", key)
	}
	if err := h.runtimeMutationAllowedLocked(agent.ID); err != nil {
		h.mu.Unlock()
		return false, err
	}
	agentID := agent.ID
	current := h.goals[agentID]
	currentVersion := int64(0)
	if current != nil {
		currentVersion = current.Version
	}
	if expectedVersion != nil && *expectedVersion != currentVersion {
		h.mu.Unlock()
		return false, errf(409, "Goal version changed: expected %d, current %d", *expectedVersion, currentVersion)
	}
	if current == nil || current.ClearedAt != 0 {
		h.mu.Unlock()
		return false, nil
	}
	next := *current
	next.Version++
	next.UpdatedAt = time.Now().UnixMilli()
	next.ClearedAt = next.UpdatedAt
	next.NativeSyncError = ""
	next.NativeSyncedAt = 0
	binding := runtimeContractBinding(agent)
	if !goalMigrationBlockedForBinding(current, binding) {
		next.NativeMigrationBlocked = false
		next.NativeMigrationBindingRevision = ""
	} else if next.NativeMigrationBindingRevision == "" {
		next.NativeMigrationBindingRevision = goalBindingRevision(binding)
	}
	if agent.RuntimeBinding.Kind == "codex" {
		next.NativeSyncState = goalNativeSyncPending
		next.NativeSyncBindingRevision = goalBindingRevision(binding)
	} else {
		next.NativeSyncState = goalNativeSyncNotApplicable
		next.NativeSyncBindingRevision = ""
	}
	h.goals[agentID] = &next
	if err := h.persistGoalsLocked(); err != nil {
		h.goals[agentID] = current
		h.mu.Unlock()
		return false, errf(500, "clear Goal: %s", err)
	}
	h.emitStatusLocked(agent, agent.Status)
	h.startPendingWorkersLocked(agentID)
	h.mu.Unlock()
	h.syncNativeGoal(agentID, next.ID, next.Version, true)
	return true, nil
}

func (h *Hub) continueGoal(agentID string) {
	if request, delivered := h.deliverAnsweredHumanRequest(agentID); delivered || request.ID != "" {
		return
	}
	if message, delivered := h.deliverNextQueuedForTarget(agentID, defaultInactivity); delivered || message != nil {
		return
	}
	h.mu.Lock()
	agent := h.agents[agentID]
	goal := cloneGoal(h.goals[agentID])
	if agent == nil || goal == nil || !h.goalContinuationReadyLocked(agentID) {
		h.mu.Unlock()
		return
	}
	if rt := h.runtimes[agentID]; rt != nil && rt.activeTurn != nil && !rt.activeTurn.finished {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	prompt := `<loom_goal_continuation goal_id="` + xmlEscape(goal.ID) + `" goal_version="` + fmt.Sprint(goal.Version) + `">Continue working toward the current Loom Goal. Complete one useful Turn and report progress. The Owner controls Goal status through Loom.</loom_goal_continuation>`
	source := internalBusinessContext("loom_goal", "goal_continuation", goal.ID, "", goal.Objective)
	source.DisplayText = "Continue Goal: " + goal.Objective
	source.GoalID, source.GoalVersion, source.GoalActive = goal.ID, goal.Version, true
	if _, err := h.sendTaskWithContext(agentID, prompt, nil, defaultInactivity, "", "", "", "", source.DisplayText, source); err != nil {
		var hubErr *HubError
		if !errors.As(err, &hubErr) || hubErr.Status != 409 {
			log.Printf("[codex-loom] continue Goal for %s: %v", agentID, err)
		}
	}
}

func (h *Hub) resumeGoalAfterOpen(agentID string) {
	h.mu.Lock()
	agent := h.agents[agentID]
	if agent == nil {
		h.mu.Unlock()
		return
	}
	kind := agent.RuntimeBinding.Kind
	if kind != "codex" {
		h.mu.Unlock()
		h.continueGoal(agentID)
		return
	}
	rt, err := h.getRuntimeLocked(agent)
	h.mu.Unlock()
	if err != nil || waitReady(rt) != nil {
		return
	}
	h.mu.Lock()
	goal := h.goals[agentID]
	goalID, goalVersion := "", int64(0)
	if goal != nil {
		goalID, goalVersion = goal.ID, goal.Version
	}
	h.mu.Unlock()
	if goalID != "" {
		h.syncNativeGoal(agentID, goalID, goalVersion, false)
	}
	h.mu.Lock()
	goal = h.goals[agentID]
	ready := h.goalContinuationReadyLocked(agentID)
	h.mu.Unlock()
	if ready {
		h.continueGoal(agentID)
	}
}

func (h *Hub) onGoalNotificationLocked(_ string, _ string, _ json.RawMessage) {
	// Native notifications have no Loom Goal ID/revision correlation. Treat the
	// RPC result as sync evidence and deliberately ignore these stale-prone
	// shapes; they never mutate, clear, or certify Loom-owned state.
}

func (h *Hub) syncNativeGoal(agentID, goalID string, version int64, clear bool) {
	h.mu.Lock()
	agent := h.agents[agentID]
	goal := h.goals[agentID]
	if agent == nil || goal == nil || goal.ID != goalID || goal.Version != version || agent.RuntimeBinding.Kind != "codex" {
		h.mu.Unlock()
		return
	}
	binding := agent.RuntimeBinding
	bindingMayInitialize := binding.NativeRef == ""
	rt, err := h.getRuntimeLocked(agent)
	h.mu.Unlock()
	if err == nil {
		err = waitReady(rt)
	}
	if err == nil {
		rt.startMu.Lock()
		defer rt.startMu.Unlock()
		h.mu.Lock()
		if fenceErr := h.runtimeMutationAllowedLocked(agentID); fenceErr != nil {
			h.mu.Unlock()
			return
		}
		currentAgent, currentGoal := h.agents[agentID], h.goals[agentID]
		if currentAgent == nil || (!bindingMayInitialize && currentAgent.RuntimeBinding != binding) || currentGoal == nil || currentGoal.ID != goalID || currentGoal.Version != version {
			h.mu.Unlock()
			return
		}
		binding = currentAgent.RuntimeBinding
		bindingRevision := goalBindingRevision(runtimeContractBinding(currentAgent))
		if rt.binding != runtimeContractBinding(currentAgent) {
			h.mu.Unlock()
			return
		}
		if currentGoal.NativeSyncState == goalNativeSyncSynced && currentGoal.NativeSyncBindingRevision == bindingRevision {
			h.mu.Unlock()
			return
		}
		goal = cloneGoalRecord(currentGoal)
		h.mu.Unlock()
		capability, ok := rt.runtimeContract.(runtimeGoalCapability)
		if !ok {
			err = errors.New("Runtime Goal synchronization is unavailable")
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err = syncNativeGoalProjection(ctx, capability, rt.binding, goal)
			cancel()
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	goal = h.goals[agentID]
	agent = h.agents[agentID]
	if agent == nil || agent.RuntimeBinding != binding || goal == nil || goal.ID != goalID || goal.Version != version || (goal.ClearedAt != 0) != clear {
		return
	}
	next := cloneGoalRecord(goal)
	bindingRevision := goalBindingRevision(runtimeContractBinding(agent))
	if err == nil {
		next.NativeSyncedAt = time.Now().UnixMilli()
		next.NativeSyncState = goalNativeSyncSynced
		next.NativeSyncError = ""
		next.NativeMigrationBlocked = false
		next.NativeMigrationBindingRevision = ""
	} else {
		next.NativeSyncedAt = 0
		next.NativeSyncState = goalNativeSyncFailed
		next.NativeSyncError = boundedDisplayTask(redactRuntimeDiagnosticString(err.Error()), 320)
	}
	next.NativeSyncBindingRevision = bindingRevision
	if persistErr := h.saveGoalRecordLocked(agentID, next); persistErr != nil {
		log.Printf("[codex-loom] persist native Goal sync evidence: %v", persistErr)
		return
	}
	if agent = h.agents[agentID]; agent != nil {
		h.emitStatusLocked(agent, agent.Status)
	}
}

func (h *Hub) startPendingWorkersLocked(agentID string) {
	if h.isDrainingLocked() {
		return
	}
	h.startWorkerLocked(func() { h.deliverNextQueuedForTarget(agentID, defaultInactivity) })
	h.startWorkerLocked(func() { h.deliverNextInboxForAgent(agentID) })
	h.startWorkerLocked(func() { h.deliverAnsweredHumanRequest(agentID) })
}

func cloneGoal(goal *ThreadGoal) *ThreadGoal {
	if goal == nil || goal.ClearedAt != 0 {
		return nil
	}
	return cloneGoalRecord(goal)
}

func cloneGoalForAgent(goal *ThreadGoal, agent *Agent) *ThreadGoal {
	copy := cloneGoal(goal)
	if copy != nil && !goalMigrationBlockedForAgent(copy, agent) {
		copy.NativeMigrationBlocked = false
		copy.NativeMigrationBindingRevision = ""
	}
	return copy
}

func cloneGoalRecord(goal *ThreadGoal) *ThreadGoal {
	if goal == nil {
		return nil
	}
	copy := *goal
	if goal.TokenBudget != nil {
		budget := *goal.TokenBudget
		copy.TokenBudget = &budget
	}
	return &copy
}
