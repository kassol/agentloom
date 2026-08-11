// Package hub governs durable Agents. An Agent is a stable governance entity
// bound to one long-lived Codex Thread; callers and interfaces come and go
// while the Agent identity, Profile, relationships and Thread remain.
//
// Process model: one shared CodexHost owns a long-lived `codex app-server`.
// All Agent Threads and Remote clients use that process; thread/resume restores
// bindings after a CodexLoom restart.
//
// Event flow: every JSON-RPC notification from codex is wrapped into
// {seq, ts, type, data}, appended to the Agent event log, and fanned out to
// subscribers. HTTP projects legacy hub/* lifecycle names into canonical
// loom/* names; Codex notifications retain their method name.
//
// Locking rule: NEVER call client.Request while holding h.mu — responses are
// delivered by the reader goroutine, which also takes h.mu for notifications.
package hub

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/codex"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

const (
	defaultInactivity         = 30 * time.Minute
	absoluteTurnCap           = 4 * time.Hour
	externalRunningStaleAfter = 2 * time.Minute
	schedulerIdentity         = "scheduler"
	schedulerDefaultTZ        = "Asia/Shanghai"
	subscriberBuffer          = 1024
	edgeCreatedAt             = "1970-01-01T00:00:00Z"
	piShutdownInterruptError  = "Pi RPC process exited: signal: interrupt"
)

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

type HubError struct {
	Status  int
	Message string
}

func (e *HubError) Error() string { return e.Message }

func errf(status int, format string, args ...any) *HubError {
	return &HubError{Status: status, Message: fmt.Sprintf(format, args...)}
}

type TurnSummary struct {
	TurnID      string `json:"turnId"`
	Task        string `json:"task"`
	Status      string `json:"status"`
	CompletedAt string `json:"completedAt"`
}

type RuntimeBinding struct {
	SchemaVersion int    `json:"schemaVersion,omitempty"`
	Kind          string `json:"kind"`
	NativeRef     string `json:"nativeRef,omitempty"`
}

const RuntimeBindingSchemaVersion = runtimecontract.BindingSchemaVersion

// Agent is CodexLoom's stable governance entity. ThreadID is owned by Loom;
// RuntimeBinding is the durable association to a Runtime-native conversation.
type Agent struct {
	ID                    string                        `json:"id"`
	Name                  string                        `json:"name"`
	Cwd                   string                        `json:"cwd"`
	ThreadID              string                        `json:"threadId"`
	RuntimeBinding        RuntimeBinding                `json:"runtimeBinding"`
	RuntimeTurnBindings   map[string]string             `json:"runtimeTurnBindings,omitempty"`
	TurnRecoveryMarkers   map[string]TurnRecoveryMarker `json:"turnRecoveryMarkers,omitempty"`
	Sandbox               string                        `json:"sandbox"`
	ApprovalPolicy        string                        `json:"approvalPolicy"`
	ProviderID            string                        `json:"providerId,omitempty"`
	Model                 string                        `json:"model,omitempty"`
	Effort                string                        `json:"effort,omitempty"`
	Status                string                        `json:"status"`
	CurrentTask           string                        `json:"currentTask"`
	CurrentTurnID         string                        `json:"currentTurnId"`
	LastError             string                        `json:"lastError"`
	LastTurn              *TurnSummary                  `json:"lastTurn"`
	CreatedAt             string                        `json:"createdAt"`
	UpdatedAt             string                        `json:"updatedAt"`
	PendingProviderSwitch *ProviderSwitchBinding        `json:"pendingProviderSwitch,omitempty"`
	ProviderHistory       []ProviderBindingChange       `json:"providerHistory,omitempty"`
	// Source is "edge" for Agents mirrored read-only from pinix-edge's
	// registry (they are re-imported each startup and never persisted here);
	// empty for Agents CodexLoom owns. Starting a Turn promotes an edge mirror
	// to a native Agent (Source cleared, then persisted).
	Source string `json:"source,omitempty"`
}

// Session is the pre-CodexLoom compatibility name.
type Session = Agent

// AgentView is what the canonical API returns: governance metadata plus live
// Codex Thread runtime state.
type AgentView struct {
	Agent
	ProcessAlive        bool                `json:"processAlive"`
	RuntimeCapabilities RuntimeCapabilities `json:"runtimeCapabilities"`
	PendingApprovals    []ApprovalView      `json:"pendingApprovals"`
	Goal                *ThreadGoal         `json:"goal,omitempty"`
	LastSeq             int64               `json:"lastSeq"`
	Recovery            *RecoveryView       `json:"recovery,omitempty"`
	nativeRuntimeRef    string
	nativeTurnBindings  map[string]string
	turnRecoveryMarkers map[string]TurnRecoveryMarker
}

type RecoveryView struct {
	PredecessorTurnID string `json:"predecessorTurnId"`
	RuntimeKind       string `json:"runtimeKind"`
	State             string `json:"state"`
	Cause             string `json:"cause"`
	FailurePhase      string `json:"failurePhase,omitempty"`
	FailureCode       string `json:"failureCode,omitempty"`
	Summary           string `json:"summary"`
}

// RuntimeDiagnostics is the explicit Developer-only projection of native
// Runtime identity. Ordinary Agent and event APIs keep these values redacted.
type RuntimeDiagnostics struct {
	AgentID      string            `json:"agentId"`
	ThreadID     string            `json:"threadId"`
	RuntimeKind  string            `json:"runtimeKind"`
	NativeRef    string            `json:"nativeRef"`
	TurnBindings map[string]string `json:"turnBindings"`
}

// SessionView is the pre-CodexLoom compatibility name.
type SessionView = AgentView

type ApprovalView struct {
	ApprovalID      string          `json:"approvalId"`
	AgentID         string          `json:"agentId"`
	TurnID          string          `json:"turnId,omitempty"`
	RuntimeKind     string          `json:"runtimeKind"`
	Method          string          `json:"method"`
	Params          json.RawMessage `json:"params"`
	Status          string          `json:"status"`
	Decision        string          `json:"decision,omitempty"`
	RequestedAt     string          `json:"requestedAt"`
	ResolvedAt      string          `json:"resolvedAt,omitempty"`
	ResolutionError string          `json:"resolutionError,omitempty"`
	TS              string          `json:"ts,omitempty"` // compatibility alias for requestedAt
}

type ActiveAgent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CurrentTask string `json:"currentTask"`
}

// RunningSession is the pre-CodexLoom compatibility name.
type RunningSession = ActiveAgent

type AgentMessage struct {
	ID                 string                        `json:"id"`
	FromAgentID        string                        `json:"fromAgentId"`
	ToAgentID          string                        `json:"toAgentId"`
	From               string                        `json:"from"`
	To                 string                        `json:"to"`
	Subject            string                        `json:"subject"`
	Body               string                        `json:"body"`
	Response           string                        `json:"response"`
	ReplyTo            string                        `json:"replyTo,omitempty"`
	SourceTurnID       string                        `json:"sourceTurnId,omitempty"`
	ScheduleID         string                        `json:"scheduleId,omitempty"`
	ScheduledAt        string                        `json:"scheduledAt,omitempty"`
	TriggerID          string                        `json:"triggerId,omitempty"`
	TopicID            string                        `json:"topicId,omitempty"`
	TriggerEvent       *TriggerEvent                 `json:"triggerEvent,omitempty"`
	Status             string                        `json:"status"`               // open, answered, closed
	Resolution         string                        `json:"resolution,omitempty"` // reply, no_reply, cancelled, completed_elsewhere, superseded
	ResolutionReason   string                        `json:"resolutionReason,omitempty"`
	ResolvedBy         string                        `json:"resolvedBy,omitempty"`
	ResolvedAt         string                        `json:"resolvedAt,omitempty"`
	DeliveryStatus     string                        `json:"deliveryStatus"`         // queued, delivering, delivered, failed, cancelled
	DeliveryMode       string                        `json:"deliveryMode,omitempty"` // turn_start, turn_steer
	CreatedAt          string                        `json:"createdAt"`
	UpdatedAt          string                        `json:"updatedAt"`
	DeliveredAt        string                        `json:"deliveredAt,omitempty"`
	LastDeliveryError  string                        `json:"lastDeliveryError,omitempty"`
	DeliveredAgentID   string                        `json:"deliveredAgentId,omitempty"`
	DeliveredSessionID string                        `json:"deliveredSessionId,omitempty"` // compatibility
	DeliveredTurnID    string                        `json:"deliveredTurnId,omitempty"`
	HandlingStatus     string                        `json:"handlingStatus,omitempty"` // pending, running, completed, interrupted, failed
	ActiveHandlingID   string                        `json:"activeHandlingAttemptId,omitempty"`
	LastHandlingError  string                        `json:"lastHandlingError,omitempty"`
	HandlingAttempts   []AgentMessageHandlingAttempt `json:"handlingAttempts,omitempty"`
}

// AgentMessageHandlingAttempt records one Turn that handled an already
// delivered internal message. Delivery and handling are separate lifecycles:
// interrupting an attempt never makes the original delivery pending again.
type AgentMessageHandlingAttempt struct {
	ID          string `json:"id"`
	TurnID      string `json:"turnId,omitempty"`
	Status      string `json:"status"` // running, completed, interrupted, failed
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt,omitempty"`
	Error       string `json:"error,omitempty"`
}

type CommParams struct {
	From       string        `json:"from"`
	To         string        `json:"to"`
	Subject    string        `json:"subject"`
	Body       string        `json:"body"`
	Response   string        `json:"response"`
	ReplyTo    string        `json:"replyTo"`
	TopicID    string        `json:"topicId,omitempty"`
	System     bool          `json:"-"`
	Timeout    time.Duration `json:"-"`
	TimeoutSec int           `json:"timeoutSec"`
}

type CommResult struct {
	Message *AgentMessage `json:"message"`
	TurnID  string        `json:"turnId,omitempty"`
}

type Schedule struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	To            string `json:"to"`
	Subject       string `json:"subject"`
	Body          string `json:"body"`
	Response      string `json:"response"`
	At            string `json:"at,omitempty"`
	Cron          string `json:"cron,omitempty"`
	Timezone      string `json:"timezone"`
	Enabled       bool   `json:"enabled"`
	LastRunAt     string `json:"lastRunAt,omitempty"`
	NextRunAt     string `json:"nextRunAt,omitempty"`
	LastMessageID string `json:"lastMessageId,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

type ScheduleParams struct {
	Name     string `json:"name"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	Response string `json:"response"`
	At       string `json:"at"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

type commRecord struct {
	Message AgentMessage `json:"message"`
}

type approvalRecord struct {
	Approval ApprovalView `json:"approval"`
}

type approval struct {
	rpcID   json.RawMessage // retained for compact-state compatibility
	respond func(decision string) error
	done    chan struct{}
}

type turnState struct {
	turnID            string
	nativeTurnID      string
	nativeTurnReady   chan struct{}
	startedConfirmed  bool
	task              string
	source            string
	inboxItemID       string
	attemptID         string
	agentMessageID    string
	topicID           string
	handlingAttemptID string
	contextAttemptID  string
	contextEpochID    string
	finalAnswer       string
	forcedFailure     string
	startedAt         time.Time
	lastActivity      time.Time
	finished          bool
	stopWatchdog      chan struct{}
}

type runtime struct {
	agentID           string
	agentRuntime      AgentRuntime
	agentHost         AgentHost
	runtimeContract   runtimecontract.Contract
	client            *codex.Client
	hostGeneration    uint64
	skillConfigHash   string
	skillConfigLoaded bool
	binding           runtimecontract.Binding
	ready             chan struct{}
	initErr           error
	startMu           sync.Mutex

	activeTurn *turnState           // guarded by Hub.mu
	approvals  map[string]*approval // guarded by Hub.mu
	// effectDomainInvalidated fences late events from a transport generation
	// whose mutating command has an indeterminate outcome.
	effectDomainInvalidated bool // guarded by Hub.mu
}

type subscriber struct {
	ch     chan store.Event
	once   sync.Once
	global bool
}

func (s *subscriber) close() {
	s.once.Do(func() { close(s.ch) })
}

type Hub struct {
	st              *store.Store
	passive         bool
	runtimeAPIURL   string
	writerOwnership *store.WritableOwnership

	mu                      sync.Mutex
	contextCoverageMu       sync.Mutex
	modelProviderMu         sync.Mutex
	agents                  map[string]*Agent
	agentSkillConfigs       map[string]*AgentSkillConfig
	comms                   map[string]*AgentMessage
	commOrder               []string
	schedules               map[string]*Schedule
	triggers                map[string]*Trigger
	topics                  map[string]*Topic
	profiles                map[string]*AgentProfile
	teamLinks               map[string]*TeamRelationship
	loomAgentPrompt         *LoomAgentPrompt
	collaborationGroups     map[string]*CollaborationGroup
	organizationLinks       map[string]*OrganizationRelationship
	connections             map[string]*PlatformConnection
	addresses               map[string]*AgentAddress
	addressOperations       map[string]*AddressLifecycleOperation
	memberships             map[string]*ConversationMembership
	conversationCandidates  map[string]*ConversationCandidate
	messages                map[string]*InboxMessage
	messageOrder            []string
	externalMessages        map[string]string
	inbox                   map[string]*InboxItem
	inboxOrder              []string
	attempts                map[string]*HandlingAttempt
	outbox                  map[string]*OutboxItem
	outboxOrder             []string
	providerOperations      map[string]*ProviderOperation
	providerOperationOrder  []string
	approvals               map[string]*ApprovalView
	approvalOrder           []string
	humanRequests           map[string]*HumanRequest
	humanRequestOrder       []string
	goals                   map[string]*ThreadGoal
	seqs                    map[string]int64
	globalSeq               int64
	runtimes                map[string]*runtime
	turnRecoveryInFlight    map[string]bool
	subs                    map[string]map[*subscriber]struct{}
	globalSubs              map[*subscriber]struct{}
	remoteConfig            RemoteConfig
	remoteStatus            RemoteStatus
	remotePairing           *RemotePairing
	remoteRuntime           *remoteRuntime
	remoteGeneration        uint64
	remoteStartMu           sync.Mutex
	remoteEnabledGeneration uint64
	codexHost               *codexHostRuntime
	codexHostDriver         *codexRuntimeHostDriver
	piHostDriver            *piRuntimeHostDriver
	runtimeHostDrivers      map[string]hubLockedRuntimeHostDriver
	codexHostGeneration     uint64
	stop                    chan struct{}
	stopOnce                sync.Once
	shutdownOnce            sync.Once
	stopping                bool
	draining                bool
	providerSwitching       bool
	triggerObservations     map[string]struct{}
	background              sync.WaitGroup
	workers                 sync.WaitGroup
	steerTurn               func(threadID, expectedTurnID, input string, timeout time.Duration) (string, error)
	dispatchHumanAnswer     func(key, text string) (SendResult, error)
	observeTrigger          triggerObserver
	contextHistoryProbe     contextHistoryProbeFunc
	threadResumeTimeout     time.Duration
	developerContextTimeout time.Duration
	agentRuntimeFactory     func(kind string) (AgentRuntime, error)

	integrationNormalizationPending  bool
	gatewayState                     gatewayState
	gatewayFoundationPoisoned        bool
	gatewayFoundationPoisonReason    string
	gatewayOpenGeneration            string
	gatewayCoordinatorInitMu         sync.Mutex
	gatewayCoordinator               *gatewayConnectionCoordinator
	saveGatewayStateForTest          func(gatewayState) error
	loadGatewayStateForTest          func(*gatewayState) (bool, error)
	gatewayServiceAdapterForTest     func(gatewayLaunchPlan) (gatewayServiceAdapter, error)
	gatewayProofWaitForTest          time.Duration
	larkUpdateConnectionForTest      func(string, string) (PlatformConnection, error)
	larkMigrationRecordWriteForTest  func() error
	larkMigrationRecordRemoveForTest func(string) error
}

// New is retained for in-process callers that cannot recover from an invalid
// store. The service entry point uses Open so it can report the startup error.
func New(st *store.Store) *Hub {
	h, err := Open(st)
	if err != nil {
		panic(err)
	}
	return h
}

// OpenOptions controls process-level behavior that is intentionally separate
// from the durable Hub model. Passive mode is used by read-only development
// canaries: it loads projections without importing external registries,
// reconciling live runtime state, or starting workers.
type OpenOptions struct {
	Passive       bool
	RuntimeAPIURL string
}

// Open loads all durable projections before starting background work. Required
// state is fail-closed: malformed data is never replaced with an empty map.
func Open(st *store.Store) (*Hub, error) {
	return OpenWithOptions(st, OpenOptions{})
}

func OpenWithOptions(st *store.Store, options OpenOptions) (*Hub, error) {
	if st == nil {
		return nil, fmt.Errorf("store is required")
	}
	var ownership *store.WritableOwnership
	if options.Passive {
		if !st.ReadOnly() {
			return nil, fmt.Errorf("passive Hub requires an independently opened read-only Store")
		}
	} else {
		if st.ReadOnly() {
			return nil, fmt.Errorf("writable Hub requires a writable Store")
		}
		var err error
		ownership, err = st.ClaimWritableOwnership()
		if err != nil {
			return nil, err
		}
	}
	opened := false
	defer func() {
		if !opened && ownership != nil {
			ownership.Release()
		}
	}()
	h := &Hub{
		st:                     st,
		passive:                options.Passive,
		runtimeAPIURL:          options.RuntimeAPIURL,
		writerOwnership:        ownership,
		agents:                 map[string]*Agent{},
		agentSkillConfigs:      map[string]*AgentSkillConfig{},
		comms:                  map[string]*AgentMessage{},
		schedules:              map[string]*Schedule{},
		triggers:               map[string]*Trigger{},
		topics:                 map[string]*Topic{},
		profiles:               map[string]*AgentProfile{},
		teamLinks:              map[string]*TeamRelationship{},
		collaborationGroups:    map[string]*CollaborationGroup{},
		organizationLinks:      map[string]*OrganizationRelationship{},
		connections:            map[string]*PlatformConnection{},
		addresses:              map[string]*AgentAddress{},
		gatewayState:           emptyGatewayState(),
		gatewayOpenGeneration:  newIntegrationID("gopen"),
		gatewayCoordinator:     newGatewayConnectionCoordinator(),
		addressOperations:      map[string]*AddressLifecycleOperation{},
		memberships:            map[string]*ConversationMembership{},
		conversationCandidates: map[string]*ConversationCandidate{},
		messages:               map[string]*InboxMessage{},
		externalMessages:       map[string]string{},
		inbox:                  map[string]*InboxItem{},
		attempts:               map[string]*HandlingAttempt{},
		outbox:                 map[string]*OutboxItem{},
		providerOperations:     map[string]*ProviderOperation{},
		approvals:              map[string]*ApprovalView{},
		humanRequests:          map[string]*HumanRequest{},
		goals:                  map[string]*ThreadGoal{},
		seqs:                   map[string]int64{},
		runtimes:               map[string]*runtime{},
		runtimeHostDrivers:     map[string]hubLockedRuntimeHostDriver{},
		turnRecoveryInFlight:   map[string]bool{},
		subs:                   map[string]map[*subscriber]struct{}{},
		globalSubs:             map[*subscriber]struct{}{},
		triggerObservations:    map[string]struct{}{},
		stop:                   make(chan struct{}),
	}
	// Validate the complete private Gateway foundation against integrations
	// before any startup recovery or compatibility normalization can write the
	// durable tree. This keeps corrupt/newer state fail-closed at Hub open.
	if err := h.loadIntegrations(); err != nil {
		return nil, fmt.Errorf("load integrations: %w", err)
	}
	if err := h.loadGatewayState(); err != nil {
		return nil, fmt.Errorf("load Gateway foundation: %w", err)
	}
	if h.integrationNormalizationPending && !options.Passive {
		if len(h.gatewayState.Controls) != 0 {
			return nil, fmt.Errorf("integration normalization requires explicit Gateway reconciliation")
		}
		if err := h.persistIntegrationsLocked(); err != nil {
			return nil, fmt.Errorf("persist normalized integrations: %w", err)
		}
	}
	h.globalSeq = st.LastSeq(globalEventLogID)
	if err := st.LoadAgents(&h.agents); err != nil {
		return nil, fmt.Errorf("load agents: %w", err)
	}
	if h.agents == nil {
		h.agents = map[string]*Agent{}
	}
	agentRegistryRecovered := false
	for id, meta := range h.agents {
		if meta == nil {
			return nil, fmt.Errorf("load agents: Agent %s uses the unsupported legacy Codex-only format; recreate the Agent", id)
		}
		switch meta.RuntimeBinding.SchemaVersion {
		case 0, 1:
			if strings.TrimSpace(meta.ThreadID) == "" || strings.TrimSpace(meta.RuntimeBinding.Kind) == "" || strings.TrimSpace(meta.RuntimeBinding.NativeRef) == "" {
				return nil, fmt.Errorf("load agents: Agent %s uses the unsupported legacy Codex-only format; recreate the Agent", id)
			}
			meta.RuntimeBinding.SchemaVersion = RuntimeBindingSchemaVersion
			agentRegistryRecovered = true
		case RuntimeBindingSchemaVersion:
			if strings.TrimSpace(meta.ThreadID) == "" || strings.TrimSpace(meta.RuntimeBinding.Kind) == "" || strings.TrimSpace(meta.RuntimeBinding.NativeRef) == "" {
				return nil, fmt.Errorf("load agents: Agent %s has an invalid Runtime Binding schema version %d", id, meta.RuntimeBinding.SchemaVersion)
			}
		default:
			return nil, fmt.Errorf("load agents: Agent %s uses unsupported Runtime Binding schema version %d", id, meta.RuntimeBinding.SchemaVersion)
		}
		if meta.RuntimeBinding.Kind == "pi" && meta.Status == "idle" && meta.LastError == piShutdownInterruptError && meta.LastTurn != nil && meta.LastTurn.Status == "completed" {
			meta.LastError = ""
			meta.UpdatedAt = now()
			agentRegistryRecovered = true
		}
		if meta.PendingProviderSwitch == nil {
			continue
		}
		meta.PendingProviderSwitch = nil
		meta.LastError = "an interrupted Provider switch was rolled back to the previous binding"
		meta.UpdatedAt = now()
		agentRegistryRecovered = true
	}
	if agentRegistryRecovered && !options.Passive {
		if err := h.persistAgentsLocked(); err != nil {
			return nil, fmt.Errorf("persist normalized Agent registry: %w", err)
		}
	}
	if err := h.st.LoadAgentSkillConfigs(&h.agentSkillConfigs); err != nil {
		return nil, fmt.Errorf("load Agent Skill config: %w", err)
	}
	if h.agentSkillConfigs == nil {
		h.agentSkillConfigs = map[string]*AgentSkillConfig{}
	}
	if err := h.st.LoadProfiles(&h.profiles); err != nil {
		return nil, fmt.Errorf("load profiles: %w", err)
	}
	if h.profiles == nil {
		h.profiles = map[string]*AgentProfile{}
	}
	if err := h.loadLoomAgentPrompt(); err != nil {
		return nil, fmt.Errorf("load Loom Agent Prompt: %w", err)
	}
	if err := h.st.LoadTeamLinks(&h.teamLinks); err != nil {
		return nil, fmt.Errorf("load team links: %w", err)
	}
	if h.teamLinks == nil {
		h.teamLinks = map[string]*TeamRelationship{}
	}
	if err := h.st.LoadCollaborationGroups(&h.collaborationGroups); err != nil {
		return nil, fmt.Errorf("load collaboration groups: %w", err)
	}
	if h.collaborationGroups == nil {
		h.collaborationGroups = map[string]*CollaborationGroup{}
	}
	if err := h.st.LoadOrganizationLinks(&h.organizationLinks); err != nil {
		return nil, fmt.Errorf("load organization links: %w", err)
	}
	if h.organizationLinks == nil {
		h.organizationLinks = map[string]*OrganizationRelationship{}
	}
	if err := h.validateLoadedCollaborationGroupsLocked(); err != nil {
		return nil, fmt.Errorf("validate collaboration groups: %w", err)
	}
	if err := h.loadInboxState(); err != nil {
		return nil, fmt.Errorf("load inbox state: %w", err)
	}
	if err := h.loadProviderOperations(); err != nil {
		return nil, fmt.Errorf("load provider operations: %w", err)
	}
	if err := h.loadApprovals(); err != nil {
		return nil, fmt.Errorf("load Approvals: %w", err)
	}
	if err := h.recoverPendingApprovals(!options.Passive); err != nil {
		return nil, fmt.Errorf("recover pending Approvals: %w", err)
	}
	if err := h.loadComms(); err != nil {
		return nil, fmt.Errorf("load communications: %w", err)
	}
	if err := h.loadHumanRequests(); err != nil {
		return nil, fmt.Errorf("load human requests: %w", err)
	}
	if err := h.st.LoadSchedules(&h.schedules); err != nil {
		return nil, fmt.Errorf("load schedules: %w", err)
	}
	if h.schedules == nil {
		h.schedules = map[string]*Schedule{}
	}
	if err := h.st.LoadTriggers(&h.triggers); err != nil {
		return nil, fmt.Errorf("load triggers: %w", err)
	}
	if h.triggers == nil {
		h.triggers = map[string]*Trigger{}
	}
	if err := h.st.LoadTopics(&h.topics); err != nil {
		return nil, fmt.Errorf("load Topics: %w", err)
	}
	if h.topics == nil {
		h.topics = map[string]*Topic{}
	}
	if h.normalizeTopicsLocked() && !options.Passive {
		if err := h.persistTopicsLocked(); err != nil {
			return nil, fmt.Errorf("persist normalized Topics: %w", err)
		}
	}
	if err := h.loadRemoteLocked(); err != nil {
		return nil, fmt.Errorf("load Remote config: %w", err)
	}
	if !options.Passive {
		// Mirror pinix-edge's registry: edge-created agents become visible here
		// (read-only) and their rollout history is immediately viewable.
		h.importEdgeLocked()
	}
	if err := h.migrateCommAgentIDsLocked(); err != nil {
		return nil, fmt.Errorf("migrate communication agent ids: %w", err)
	}
	if err := h.reconcileTriggersLocked(); err != nil {
		return nil, fmt.Errorf("reconcile triggers: %w", err)
	}
	for _, meta := range h.agents {
		h.seqs[meta.ID] = st.LastSeq(meta.ID)
	}
	if options.Passive {
		opened = true
		return h, nil
	}

	// Reconcile: tasks running when the Hub last died are interrupted. Keep the
	// interrupted projection visible until the Owner continues or dismisses it;
	// otherwise a restart silently turns unfinished work into an idle Agent.
	recoveryJobs := [][2]string{}
	restartInterrupted := []*Agent{}
	restartMessageTurns := []*turnState{}
	h.mu.Lock()
	for _, meta := range h.agents {
		if meta.Source == "edge" {
			continue // edge mirrors carry no CodexLoom-driven turn state
		}
		if meta.Status == "running" {
			interrupted, missingTerminal := reconcileInterruptedTurn(meta)
			interruptedTurnID := interrupted.TurnID
			if !missingTerminal {
				if interruptedTurnID != "" {
					for _, messageID := range h.commOrder {
						msg := h.comms[messageID]
						if msg == nil || msg.ToAgentID != meta.ID || msg.DeliveryMode != "turn_start" || msg.DeliveredTurnID != interruptedTurnID {
							continue
						}
						h.finishAgentMessageTurnLocked(&turnState{turnID: interruptedTurnID, agentMessageID: msg.ID}, interrupted.Status, "")
					}
				}
				meta.Status = "idle"
				meta.LastError = ""
				meta.LastTurn = interrupted
				meta.CurrentTask = ""
				meta.CurrentTurnID = ""
				meta.UpdatedAt = now()
				continue
			}
			if interruptedTurnID != "" {
				for _, messageID := range h.commOrder {
					msg := h.comms[messageID]
					if msg == nil || msg.ToAgentID != meta.ID || msg.DeliveryMode != "turn_start" || msg.DeliveredTurnID != interruptedTurnID {
						continue
					}
					restartMessageTurns = append(restartMessageTurns, &turnState{turnID: interruptedTurnID, agentMessageID: msg.ID})
				}
			}
			meta.Status = "interrupted"
			meta.LastError = "interrupted: CodexLoom restarted while task was running"
			meta.LastTurn = interrupted
			meta.CurrentTask = ""
			meta.CurrentTurnID = ""
			meta.UpdatedAt = now()
			if meta.TurnRecoveryMarkers == nil {
				meta.TurnRecoveryMarkers = map[string]TurnRecoveryMarker{}
			}
			stamp := now()
			meta.TurnRecoveryMarkers[interrupted.TurnID] = TurnRecoveryMarker{
				PredecessorTurnID: interrupted.TurnID, NativeTurnID: meta.RuntimeTurnBindings[interrupted.TurnID],
				RuntimeKind: meta.RuntimeBinding.Kind, Cause: "hub_restart", State: TurnRecoveryObserved,
				Summary: "CodexLoom restarted while the Turn outcome was not confirmed", CreatedAt: stamp, UpdatedAt: stamp,
			}
			restartInterrupted = append(restartInterrupted, meta)
		}
	}
	for _, meta := range h.agents {
		if meta == nil || meta.Source == "edge" {
			continue
		}
		planned := map[string]bool{}
		for predecessorTurnID, marker := range meta.TurnRecoveryMarkers {
			if marker.State == TurnRecoveryPlanned {
				planned[predecessorTurnID] = true
				recoveryJobs = append(recoveryJobs, [2]string{meta.ID, predecessorTurnID})
			}
		}
		if meta.LastTurn == nil || meta.LastTurn.Status != "interrupted" || planned[meta.LastTurn.TurnID] {
			continue
		}
		if marker, exists := meta.TurnRecoveryMarkers[meta.LastTurn.TurnID]; !exists || marker.State == "" || marker.State == TurnRecoveryObserved {
			recoveryJobs = append(recoveryJobs, [2]string{meta.ID, meta.LastTurn.TurnID})
		}
	}
	if err := h.persistAgentsLocked(); err != nil {
		h.mu.Unlock()
		return nil, fmt.Errorf("persist startup recovery: %w", err)
	}
	for _, meta := range restartInterrupted {
		h.emitLocked(meta.ID, "loom/turn-interrupted", map[string]any{
			"reason": "loom-restart", "task": meta.LastTurn.Task, "turnId": meta.LastTurn.TurnID,
			"recovery": recoveryView(meta),
		})
		h.emitStatusLocked(meta, "interrupted")
	}
	for _, turn := range restartMessageTurns {
		h.finishAgentMessageTurnLocked(turn, "interrupted", "CodexLoom restarted while delivery Turn was running")
	}
	for _, job := range recoveryJobs {
		agentID, predecessorTurnID := job[0], job[1]
		h.scheduleTurnRecoveryLocked(agentID, predecessorTurnID)
	}
	h.mu.Unlock()
	h.background.Add(6)
	go func() { defer h.background.Done(); h.deliveryLoop() }()
	go func() { defer h.background.Done(); h.schedulerLoop() }()
	go func() { defer h.background.Done(); h.inboxLoop() }()
	go func() { defer h.background.Done(); h.remoteLoop() }()
	go func() { defer h.background.Done(); h.eventMaintenanceLoop() }()
	go func() { defer h.background.Done(); h.triggerLoop() }()
	opened = true
	return h, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (h *Hub) loadComms() error {
	repairLatest := map[string]bool{}
	if err := h.st.ReadComms(func(raw json.RawMessage) {
		var rec commRecord
		if err := json.Unmarshal(raw, &rec); err != nil || rec.Message.ID == "" {
			return
		}
		msg := rec.Message
		if _, exists := h.comms[msg.ID]; !exists {
			h.commOrder = append(h.commOrder, msg.ID)
		}
		repairLatest[msg.ID] = normalizeAgentMessage(&msg)
		h.comms[msg.ID] = &msg
	}); err != nil {
		return err
	}
	h.reconcileDeliveredReplyRoots(repairLatest)
	for _, id := range h.commOrder {
		if !repairLatest[id] {
			continue
		}
		msg := h.comms[id]
		if !h.passive {
			if err := h.st.AppendComm(commRecord{Message: *msg}); err != nil {
				return fmt.Errorf("persist repaired message %s: %w", msg.ID, err)
			}
		}
	}
	return nil
}

func (h *Hub) reconcileDeliveredReplyRoots(repairLatest map[string]bool) {
	for _, id := range h.commOrder {
		reply := h.comms[id]
		if reply == nil || reply.ReplyTo == "" || reply.Resolution != "reply" || reply.DeliveryStatus != "delivered" {
			continue
		}
		root := h.comms[reply.ReplyTo]
		if root == nil || root.Status != "open" || root.Response != "required" {
			continue
		}
		resolvedAt := reply.DeliveredAt
		if resolvedAt == "" {
			resolvedAt = reply.UpdatedAt
		}
		if resolvedAt == "" {
			resolvedAt = reply.CreatedAt
		}
		if resolvedAt == "" {
			resolvedAt = now()
		}
		next := *root
		next.Status = "answered"
		next.Resolution = "reply"
		next.ResolvedBy = reply.From
		next.ResolvedAt = resolvedAt
		next.UpdatedAt = resolvedAt
		h.comms[root.ID] = &next
		repairLatest[root.ID] = true
	}
}

func normalizeAgentMessage(msg *AgentMessage) bool {
	repaired := false
	if msg.DeliveredAgentID == "" {
		msg.DeliveredAgentID = msg.DeliveredSessionID
	}
	if msg.DeliveredSessionID == "" {
		msg.DeliveredSessionID = msg.DeliveredAgentID
	}
	if msg.Status == "queued" || msg.Status == "delivering" || msg.Status == "failed" {
		msg.Status = "open"
		if msg.Response == "none" {
			msg.Status = "closed"
		}
	}
	if msg.Status == "" {
		msg.Status = "open"
		if msg.Response == "none" {
			msg.Status = "closed"
		}
	}
	if msg.DeliveryStatus == "" {
		if msg.DeliveredTurnID != "" {
			msg.DeliveryStatus = "delivered"
		} else {
			msg.DeliveryStatus = "queued"
		}
	}
	if msg.DeliveryStatus == "delivering" {
		msg.DeliveryStatus = "queued"
		msg.LastDeliveryError = "recovered from interrupted delivery"
	}
	// Older Loom versions turned an interrupted handling Turn back into a
	// queued delivery. That creates an infinite stop/redeliver loop. Recover
	// those exact records as an already-delivered but held request.
	if msg.DeliveryStatus == "queued" && strings.Contains(msg.LastDeliveryError, "delivery Turn interrupted") {
		msg.DeliveryStatus = "delivered"
		msg.HandlingStatus = "interrupted"
		msg.LastHandlingError = msg.LastDeliveryError
		msg.LastDeliveryError = ""
		repaired = true
	}
	if msg.DeliveryMode == "" && msg.DeliveryStatus == "delivered" && msg.DeliveredTurnID != "" {
		msg.DeliveryMode = "turn_start"
	}
	if msg.HandlingStatus == "" {
		switch msg.DeliveryStatus {
		case "queued", "delivering", "failed":
			msg.HandlingStatus = "pending"
		case "delivered":
			msg.HandlingStatus = "completed"
		}
	}
	if msg.DeliveryStatus == "cancelled" && msg.Status == "open" {
		msg.Status = "closed"
		msg.Resolution = "cancelled"
		msg.ResolvedBy = msg.From
		msg.ResolvedAt = msg.UpdatedAt
		repaired = true
	}
	return repaired
}

func (h *Hub) emitGlobalLocked(typ string, data any) {
	h.globalSeq++
	ev := store.Event{Seq: h.globalSeq, TS: now(), Type: typ, Data: toRaw(data)}
	if err := h.st.AppendEvent(globalEventLogID, ev); err != nil {
		log.Printf("[codex-loom] append global event: %v", err)
	}
	for sub := range h.globalSubs {
		select {
		case sub.ch <- ev:
		default:
			delete(h.globalSubs, sub)
			sub.close()
		}
	}
}

const globalEventLogID = "__global__"

func (h *Hub) LastGlobalSeq() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.globalSeq
}

func (h *Hub) ReadGlobalEvents(since int64, tail int) ([]store.Event, error) {
	return h.st.ReadEvents(globalEventLogID, since, tail)
}

func (h *Hub) persistAgentsLocked() error {
	// Persist only agents CodexLoom owns. Edge mirrors are re-imported from
	// pinix-edge's registry on every startup, so writing them here would only
	// let them drift out of sync.
	own := make(map[string]*Agent, len(h.agents))
	for id, meta := range h.agents {
		if meta.Source == "edge" {
			continue
		}
		if meta.RuntimeBinding.Kind != "" {
			meta.RuntimeBinding.SchemaVersion = RuntimeBindingSchemaVersion
		}
		own[id] = meta
	}
	return h.st.SaveAgents(own)
}

// persistRuntimeProjectionLocked checkpoints observed Codex runtime state. The
// rollout remains authoritative and Open reconciles this projection after a
// crash, so notification callbacks log checkpoint failures instead of blocking
// the shared app-server read loop. User-authored state uses explicit commits.
func (h *Hub) persistRuntimeProjectionLocked() {
	if err := h.persistAgentsLocked(); err != nil {
		log.Printf("[codex-loom] persist: %v", err)
	}
}

// importEdgeLocked merges pinix-edge's name registry into the Agent map as
// read-only mirrors. Existing Agents win by either name or Thread binding. A
// Hub-side rename must not make the old edge name reappear as a second Agent
// for the same Thread.
func (h *Hub) importEdgeLocked() {
	agents, err := store.LoadEdgeAgents()
	if err != nil {
		log.Printf("[codex-loom] load edge registry: %v", err)
		return
	}
	taken := map[string]bool{}
	takenThreads := map[string]bool{}
	for _, meta := range h.agents {
		taken[meta.Name] = true
		if threadID := strings.TrimSpace(meta.RuntimeBinding.NativeRef); threadID != "" {
			takenThreads[threadID] = true
		}
	}
	for _, a := range agents {
		threadID := strings.TrimSpace(a.ThreadID)
		if taken[a.Name] || takenThreads[threadID] {
			continue
		}
		id := stableEdgeAgentID(threadID)
		if _, clash := h.agents[id]; clash {
			continue
		}
		h.agents[id] = &Agent{
			ID: id, Name: a.Name, Cwd: a.Cwd, ThreadID: "thr_" + id,
			RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: threadID},
			Sandbox:        "danger-full-access", ApprovalPolicy: "never",
			Status: "idle", Source: "edge",
			CreatedAt: edgeCreatedAt, UpdatedAt: now(),
		}
		taken[a.Name] = true
		takenThreads[threadID] = true
	}
}

func (h *Hub) resolveLocked(key string) *Agent {
	if s, ok := h.agents[key]; ok {
		return s
	}
	for _, s := range h.agents {
		if s.Name == key {
			return s
		}
	}
	return nil
}

// ---- events ----

func toRaw(data any) json.RawMessage {
	if raw, ok := data.(json.RawMessage); ok {
		if len(raw) == 0 {
			return json.RawMessage("{}")
		}
		return raw
	}
	b, err := json.Marshal(data)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func (h *Hub) emitLocked(agentID, typ string, data any) store.Event {
	h.seqs[agentID]++
	ev := store.Event{Seq: h.seqs[agentID], TS: now(), Type: typ, Data: toRaw(data)}
	if err := h.st.AppendEvent(agentID, ev); err != nil {
		log.Printf("[codex-loom] append event: %v", err)
	}
	for sub := range h.subs[agentID] {
		select {
		case sub.ch <- ev:
		default:
			// Slow observer: drop it; SSE client reconnects and replays by seq.
			delete(h.subs[agentID], sub)
			sub.close()
		}
	}
	// The workbench keeps multiple Agent tabs live over its single global SSE
	// connection. Preserve the Agent-local event unchanged inside the envelope
	// so each mounted Thread view can reduce its own stream independently.
	h.emitGlobalLocked("loom/thread-event", map[string]any{"agentId": agentID, "event": ev})
	return ev
}

func (h *Hub) emitStatusLocked(meta *Agent, status string) {
	processAlive := false
	if rt := h.runtimes[meta.ID]; rt != nil && runtimeBackend(rt) != nil {
		processAlive = runtimeBackend(rt).Alive()
	}
	data := map[string]any{
		"id":                  meta.ID,
		"name":                meta.Name,
		"cwd":                 meta.Cwd,
		"threadId":            meta.ThreadID,
		"runtimeKind":         meta.RuntimeBinding.Kind,
		"source":              meta.Source,
		"status":              status,
		"currentTask":         meta.CurrentTask,
		"currentTurnId":       meta.CurrentTurnID,
		"lastError":           meta.LastError,
		"lastTurn":            meta.LastTurn,
		"model":               meta.Model,
		"effort":              meta.Effort,
		"sandbox":             meta.Sandbox,
		"approvalPolicy":      meta.ApprovalPolicy,
		"providerId":          meta.ProviderID,
		"processAlive":        processAlive,
		"runtimeCapabilities": h.runtimeCapabilitiesLocked(meta),
		"updatedAt":           meta.UpdatedAt,
	}
	if recovery := recoveryView(meta); recovery != nil {
		data["recovery"] = recovery
	}
	data["goal"] = h.goals[meta.ID]
	h.emitGlobalLocked("loom/agent-status", data)
}

func (h *Hub) EmitGlobal(typ string, data map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.emitGlobalLocked(typ, data)
}

// Subscribe returns a channel of live events for an Agent plus a cancel func.
func (h *Hub) Subscribe(key string) (<-chan store.Event, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta := h.resolveLocked(key)
	if meta == nil {
		return nil, nil, errf(404, "agent not found: %s", key)
	}
	sub := &subscriber{ch: make(chan store.Event, subscriberBuffer)}
	if h.subs[meta.ID] == nil {
		h.subs[meta.ID] = map[*subscriber]struct{}{}
	}
	h.subs[meta.ID][sub] = struct{}{}
	cancel := func() {
		h.mu.Lock()
		delete(h.subs[meta.ID], sub)
		h.mu.Unlock()
		sub.close()
	}
	return sub.ch, cancel, nil
}

func (h *Hub) SubscribeGlobal() (<-chan store.Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sub := &subscriber{ch: make(chan store.Event, subscriberBuffer), global: true}
	h.globalSubs[sub] = struct{}{}
	cancel := func() {
		h.mu.Lock()
		delete(h.globalSubs, sub)
		h.mu.Unlock()
		sub.close()
	}
	return sub.ch, cancel
}

func (h *Hub) ReadEvents(key string, since int64, tail int) ([]store.Event, error) {
	h.mu.Lock()
	meta := h.resolveLocked(key)
	h.mu.Unlock()
	if meta == nil {
		return nil, errf(404, "agent not found: %s", key)
	}
	return h.st.ReadEvents(meta.ID, since, tail)
}

// LastSeq returns the highest event seq for the Agent (0 if none). Used by
// the SSE handler to skip replay (live-only) when history is served separately.
func (h *Hub) LastSeq(key string) int64 {
	h.mu.Lock()
	meta := h.resolveLocked(key)
	h.mu.Unlock()
	if meta == nil {
		return 0
	}
	return h.st.LastSeq(meta.ID)
}

// ---- runtime management ----

// getRuntimeLocked returns an Agent binding to the shared CodexHost.
func (h *Hub) getRuntimeLocked(meta *Agent) (*runtime, error) {
	if rt, ok := h.runtimes[meta.ID]; ok && runtimeHandleAlive(rt) {
		select {
		case <-rt.ready:
			if rt.initErr == nil {
				return rt, nil
			}
			// A transient resume/initialize failure must not poison this Agent
			// until the entire shared Host is restarted.
			delete(h.runtimes, meta.ID)
		default:
			return rt, nil
		}
	}
	var rt *runtime
	if h.agentRuntimeFactory != nil {
		backend, err := h.agentRuntimeFactory(meta.RuntimeBinding.Kind)
		if err != nil {
			return nil, err
		}
		rt = &runtime{agentID: meta.ID, agentRuntime: backend, ready: make(chan struct{}), approvals: map[string]*approval{}}
	} else {
		driver, err := h.runtimeHostDriverLocked(meta.RuntimeBinding.Kind)
		if err != nil {
			return nil, err
		}
		handle, err := driver.acquireWhileHubLocked(context.Background(), AgentHostRequest{AgentID: meta.ID})
		if err != nil {
			return nil, err
		}
		rt = &runtime{
			agentID: meta.ID, agentHost: handle, runtimeContract: handle.Contract(),
			ready: make(chan struct{}), approvals: map[string]*approval{},
		}
		if compatibility, ok := handle.(legacyRuntimeHost); ok {
			rt.agentRuntime = compatibility.legacyRuntime()
		}
		if compatibility, ok := handle.(codexCompatibilityHost); ok {
			rt.client, rt.hostGeneration = compatibility.codexCompatibility()
		}
		handle.SetFailureHandler(func(err error) { h.onRuntimeFailure(rt, err) })
		if seeder, ok := rt.runtimeContract.(runtimeTurnCorrelationSeeder); ok {
			seeder.seedTurnBindings(meta.RuntimeTurnBindings)
		}
		rt.runtimeContract.SetEventHandler(func(event runtimecontract.Event) { h.onCanonicalRuntimeEvent(rt, event) })
	}
	if source, ok := runtimeBackend(rt).(RuntimeEventSource); ok {
		source.SetRuntimeEventHandlers(
			func(event RuntimeEvent) { h.onRuntimeEvent(rt, event) },
			func(err error) { h.onRuntimeFailure(rt, err) },
		)
	}
	if source, ok := runtimeBackend(rt).(RuntimeApprovalSource); ok {
		source.SetRuntimeApprovalHandler(func(request RuntimeApprovalRequest) {
			h.onRuntimeApprovalRequest(rt, request)
		})
	}
	h.runtimes[meta.ID] = rt
	if !h.startWorkerLocked(func() { h.initRuntime(meta.ID, rt) }) {
		delete(h.runtimes, meta.ID)
		return nil, errf(503, "CodexLoom is shutting down")
	}
	return rt, nil
}

func runtimeHandleAlive(rt *runtime) bool {
	if rt == nil {
		return false
	}
	if rt.agentHost != nil {
		return rt.agentHost.Alive()
	}
	if rt.runtimeContract != nil {
		// Test and embedded contracts may not require a separately supervised
		// process handle. Production contracts always arrive through AgentHost.
		return true
	}
	backend := runtimeBackend(rt)
	return backend != nil && backend.Alive()
}

func (h *Hub) piDriverLocked() *piRuntimeHostDriver {
	if h.piHostDriver == nil {
		h.piHostDriver = newPiRuntimeHostDriver(h)
	}
	if h.runtimeHostDrivers == nil {
		h.runtimeHostDrivers = map[string]hubLockedRuntimeHostDriver{}
	}
	h.runtimeHostDrivers["pi"] = h.piHostDriver
	return h.piHostDriver
}

func (h *Hub) runtimeHostDriverLocked(kind string) (hubLockedRuntimeHostDriver, error) {
	if h.runtimeHostDrivers != nil {
		if driver := h.runtimeHostDrivers[kind]; driver != nil {
			return driver, nil
		}
	}
	switch kind {
	case "codex":
		return h.codexDriverLocked(), nil
	case "pi":
		return h.piDriverLocked(), nil
	default:
		return nil, errf(400, "unsupported Runtime kind %q", kind)
	}
}

func (h *Hub) bindPiContract(meta *Agent, rt *runtime, contract *piRuntimeContract) {
	if contract == nil {
		return
	}
	contract.seedTurnBindings(meta.RuntimeTurnBindings)
	contract.SetEventHandler(func(event runtimecontract.Event) { h.onCanonicalRuntimeEvent(rt, event) })
}

func (h *Hub) initRuntime(agentID string, rt *runtime) {
	defer close(rt.ready)
	if rt.runtimeContract == nil {
		h.initLegacyRuntime(agentID, rt)
		return
	}
	if readier, ok := rt.agentHost.(runtimeHostReadier); ok {
		if err := readier.waitRuntimeHostReady(context.Background()); err != nil {
			rt.initErr = err
			return
		}
	}
	h.mu.Lock()
	meta := h.agents[agentID]
	if meta == nil {
		h.mu.Unlock()
		rt.initErr = errf(404, "agent vanished")
		return
	}
	threadID, threadName, sandbox, cwd := meta.RuntimeBinding.NativeRef, meta.Name, meta.Sandbox, meta.Cwd
	providerID, model := effectiveProviderBinding(meta)
	disabledSkillPaths := h.disabledSkillPathsLocked(meta.ID)
	skillConfigHash := agentSkillConfigHash(disabledSkillPaths)
	persistedBinding := runtimeContractBinding(meta)
	h.mu.Unlock()
	controls := RuntimeBindingRequest{
		NativeRef: threadID, Name: threadName, Sandbox: sandbox, Cwd: cwd,
		ProviderID: providerID, Model: model, DisabledSkillPaths: disabledSkillPaths,
	}
	if compatibility, ok := rt.runtimeContract.(runtimeCompatibilityControls); ok {
		compatibility.setCompatibilityBinding(controls)
	}
	startBinding := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), h.effectiveThreadResumeTimeout())
		defer cancel()
		binding, outcome := rt.runtimeContract.CreateBinding(ctx, runtimecontract.BindingRequest{AgentID: agentID, Name: threadName, Cwd: cwd})
		if err := compatibilityLifecycleOutcomeError(outcome); err != nil {
			return err
		}
		if strings.TrimSpace(binding.NativeRef) == "" {
			return fmt.Errorf("Runtime accepted binding creation without a native reference")
		}
		rt.binding = binding
		h.mu.Lock()
		if current := h.agents[agentID]; current != nil {
			previous := *current
			current.RuntimeBinding.SchemaVersion = binding.SchemaVersion
			current.RuntimeBinding.Kind = binding.RuntimeKind
			current.RuntimeBinding.NativeRef = binding.NativeRef
			current.UpdatedAt = now()
			if err := h.persistAgentsLocked(); err != nil {
				*current = previous
				h.mu.Unlock()
				return fmt.Errorf("persist started Runtime binding: %w", err)
			}
		}
		h.mu.Unlock()
		if err := h.syncRuntimeBindingName(rt, binding, threadName); err != nil {
			log.Printf("[codex-loom] sync created native binding name for Agent %s: %v", agentID, err)
		}
		h.markRuntimeSkillConfigApplied(agentID, rt, skillConfigHash)
		return nil
	}
	if threadID == "" {
		rt.initErr = startBinding()
		return
	}
	rt.binding = persistedBinding
	ctx, cancel := context.WithTimeout(context.Background(), h.effectiveThreadResumeTimeout())
	outcome := rt.runtimeContract.ResumeBinding(ctx, persistedBinding)
	cancel()
	if err := compatibilityLifecycleOutcomeError(outcome); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no rollout") || strings.Contains(msg, "not found") {
			rt.initErr = startBinding()
		} else {
			rt.initErr = err
		}
		return
	}
	if err := h.syncRuntimeBindingName(rt, persistedBinding, threadName); err != nil {
		log.Printf("[codex-loom] sync resumed native binding name for Agent %s: %v", agentID, err)
	}
	h.markRuntimeSkillConfigApplied(agentID, rt, skillConfigHash)
}

func (h *Hub) markRuntimeSkillConfigApplied(agentID string, rt *runtime, skillConfigHash string) {
	h.mu.Lock()
	meta := h.agents[agentID]
	if meta == nil {
		h.mu.Unlock()
		return
	}
	query := h.captureRuntimeCapabilityQueryLocked(meta, rt)
	h.mu.Unlock()
	snapshot, err := h.queryRuntimeCapabilities(query)
	descriptor, ok := capabilityDescriptor(snapshot, runtimecontract.CapabilitySkillsPolicy)
	available := err == nil && ok && descriptor.Availability == runtimecontract.CapabilityAvailable
	h.mu.Lock()
	if h.runtimeCapabilityQueryCurrentLocked(query) {
		if available {
			rt.skillConfigHash = skillConfigHash
			rt.skillConfigLoaded = true
		} else {
			rt.skillConfigHash = ""
			rt.skillConfigLoaded = false
		}
	}
	h.mu.Unlock()
}

// initLegacyRuntime is retained only for injected v1 test doubles while the
// production Runtime Host registry is fully v2.
func (h *Hub) initLegacyRuntime(agentID string, rt *runtime) {
	h.mu.Lock()
	meta := h.agents[agentID]
	if meta == nil {
		h.mu.Unlock()
		rt.initErr = errf(404, "agent vanished")
		return
	}
	threadID, threadName, sandbox, cwd := meta.RuntimeBinding.NativeRef, meta.Name, meta.Sandbox, meta.Cwd
	providerID, model := effectiveProviderBinding(meta)
	disabledSkillPaths := h.disabledSkillPathsLocked(meta.ID)
	skillConfigHash := agentSkillConfigHash(disabledSkillPaths)
	h.mu.Unlock()

	backend := runtimeBackend(rt)
	if backend == nil {
		rt.initErr = fmt.Errorf("Agent Runtime is unavailable")
		return
	}
	if rt.client != nil {
		h.mu.Lock()
		host := h.codexHost
		h.mu.Unlock()
		if host == nil || host.generation != rt.hostGeneration {
			rt.initErr = fmt.Errorf("CodexHost changed while thread was initializing")
			return
		}
		if err := waitCodexHost(host); err != nil {
			rt.initErr = err
			return
		}
	}
	binding := RuntimeBindingRequest{
		NativeRef: threadID, Name: threadName, Sandbox: sandbox, Cwd: cwd,
		ProviderID: providerID, Model: model, DisabledSkillPaths: disabledSkillPaths,
	}
	startThread := func() error {
		threadID, err := backend.Create(binding)
		if err != nil {
			return err
		}
		h.mu.Lock()
		if m := h.agents[agentID]; m != nil {
			previous := *m
			m.RuntimeBinding.NativeRef = threadID
			m.UpdatedAt = now()
			if err := h.persistAgentsLocked(); err != nil {
				*m = previous
				h.mu.Unlock()
				return fmt.Errorf("persist started Thread binding: %w", err)
			}
		}
		h.mu.Unlock()
		if rt.client != nil {
			if err := setThreadName(rt.client, threadID, threadName); err != nil {
				log.Printf("[codex-loom] set thread name %s to %q: %v", threadID, threadName, err)
			}
		}
		h.mu.Lock()
		if h.runtimes[agentID] == rt {
			rt.skillConfigHash = skillConfigHash
			rt.skillConfigLoaded = true
		}
		h.mu.Unlock()
		return nil
	}
	if threadID == "" {
		rt.initErr = startThread()
		return
	}
	err := backend.Resume(binding, h.effectiveThreadResumeTimeout())
	if err != nil {
		if codex.IsRequestTimeout(err) {
			h.markThreadControlIndeterminate(rt, threadID, "thread/resume")
		}
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no rollout") || strings.Contains(msg, "not found") {
			rt.initErr = startThread()
		} else {
			rt.initErr = err
		}
		return
	}
	if rt.client != nil {
		if err := setThreadName(rt.client, threadID, threadName); err != nil {
			log.Printf("[codex-loom] set thread name %s to %q: %v", threadID, threadName, err)
		}
	}
	h.mu.Lock()
	if h.runtimes[agentID] == rt {
		rt.skillConfigHash = skillConfigHash
		rt.skillConfigLoaded = true
	}
	h.mu.Unlock()
}

func runtimeBackend(rt *runtime) AgentRuntime {
	if rt == nil {
		return nil
	}
	if rt.agentRuntime != nil {
		return rt.agentRuntime
	}
	if rt.client != nil {
		return &codexAgentRuntime{client: rt.client}
	}
	return nil
}

func threadBindingParams(sandbox, cwd, providerID, model string, disabledSkillPaths []string) map[string]any {
	params := map[string]any{"sandbox": sandbox, "cwd": cwd}
	if providerID = strings.TrimSpace(providerID); providerID != "" {
		params["modelProvider"] = providerID
	}
	if model = strings.TrimSpace(model); model != "" {
		params["model"] = model
	}
	params["config"] = codexAgentSkillConfig(disabledSkillPaths)
	return params
}

func resumeThread(client *codex.Client, threadID, sandbox, cwd, providerID, model string, disabledSkillPaths []string) error {
	return resumeThreadWithTimeout(client, threadID, sandbox, cwd, providerID, model, disabledSkillPaths, 60*time.Second)
}

func resumeThreadWithTimeout(client *codex.Client, threadID, sandbox, cwd, providerID, model string, disabledSkillPaths []string, timeout time.Duration) error {
	params := threadBindingParams(sandbox, cwd, providerID, model, disabledSkillPaths)
	params["threadId"] = threadID
	_, err := client.Request("thread/resume", params, timeout)
	return err
}

func (h *Hub) effectiveThreadResumeTimeout() time.Duration {
	if h.threadResumeTimeout > 0 {
		return h.threadResumeTimeout
	}
	return 60 * time.Second
}

func codexSandboxMode(sandbox string) string {
	switch strings.TrimSpace(sandbox) {
	case "danger-full-access", "dangerFullAccess":
		return "dangerFullAccess"
	case "workspace-write", "workspaceWrite":
		return "workspaceWrite"
	case "read-only", "readOnly":
		return "readOnly"
	default:
		return strings.TrimSpace(sandbox)
	}
}

func codexSandboxPolicy(sandbox string) map[string]any {
	return map[string]any{"type": codexSandboxMode(sandbox)}
}

func (h *Hub) resumeAgentThread(agentID string, rt *runtime) error {
	h.mu.Lock()
	meta := h.agents[agentID]
	if meta == nil {
		h.mu.Unlock()
		return errf(404, "agent vanished")
	}
	threadID, name, sandbox, cwd := meta.RuntimeBinding.NativeRef, meta.Name, meta.Sandbox, meta.Cwd
	providerID, model := effectiveProviderBinding(meta)
	disabledSkillPaths := h.disabledSkillPathsLocked(meta.ID)
	skillConfigHash := agentSkillConfigHash(disabledSkillPaths)
	if rt.skillConfigLoaded && rt.skillConfigHash != skillConfigHash {
		h.mu.Unlock()
		return errf(409, "Agent Skill policy changed for a loaded Codex Thread; restart CodexLoom before starting the next Turn")
	}
	h.mu.Unlock()
	if strings.TrimSpace(threadID) == "" {
		return errf(409, "agent has no Runtime conversation binding")
	}
	controls := RuntimeBindingRequest{
		NativeRef: threadID, Name: name, Sandbox: sandbox, Cwd: cwd,
		ProviderID: providerID, Model: model, DisabledSkillPaths: disabledSkillPaths,
	}
	var err error
	if rt.runtimeContract != nil {
		if compatibility, ok := rt.runtimeContract.(runtimeCompatibilityControls); ok {
			controls.Name = name
			compatibility.setCompatibilityBinding(controls)
		}
		ctx, cancel := context.WithTimeout(context.Background(), h.effectiveThreadResumeTimeout())
		outcome := rt.runtimeContract.ResumeBinding(ctx, rt.binding)
		cancel()
		err = compatibilityLifecycleOutcomeError(outcome)
	} else {
		backend := runtimeBackend(rt)
		if backend == nil {
			return errf(500, "Agent Runtime is unavailable")
		}
		err = backend.Resume(controls, h.effectiveThreadResumeTimeout())
		if codex.IsRequestTimeout(err) {
			h.markThreadControlIndeterminate(rt, threadID, "thread/resume")
		}
	}
	if err != nil {
		if isRuntimeIndeterminate(err) {
			h.onRuntimeIndeterminate(rt, err)
		}
		return errf(500, "resume Runtime conversation: %s", err)
	}
	if rt.runtimeContract != nil {
		h.markRuntimeSkillConfigApplied(agentID, rt, skillConfigHash)
	} else {
		h.mu.Lock()
		if h.runtimes[agentID] == rt {
			rt.skillConfigHash = skillConfigHash
			rt.skillConfigLoaded = true
		}
		h.mu.Unlock()
	}
	return nil
}

func setThreadName(client *codex.Client, threadID, name string) error {
	threadID = strings.TrimSpace(threadID)
	name = strings.TrimSpace(name)
	if threadID == "" || name == "" {
		return nil
	}
	_, err := client.Request("thread/name/set", map[string]any{
		"threadId": threadID,
		"name":     name,
	}, 10*time.Second)
	return err
}

func waitReady(rt *runtime) error {
	<-rt.ready
	return rt.initErr
}

// ---- codex message handling ----

type turnParams struct {
	TurnID string `json:"turnId"`
	Turn   struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"turn"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func notificationUserText(params json.RawMessage) string {
	var event struct {
		Item struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	}
	if json.Unmarshal(params, &event) != nil || event.Item.Type != "userMessage" {
		return ""
	}
	if text := strings.TrimSpace(event.Item.Text); text != "" {
		return text
	}
	var parts []string
	for _, content := range event.Item.Content {
		if text := strings.TrimSpace(content.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func displayUserTask(text string) string {
	text = strings.TrimSpace(text)
	if task, ok := displayLoomControlTask(text); ok {
		return task
	}
	managedContext := false
	if index := strings.Index(text, "<loom_context"); index >= 0 {
		managedContext = true
		text = strings.TrimSpace(text[:index])
	}
	if index := strings.Index(text, "\n\n<loom_attachments"); index >= 0 {
		text = strings.TrimSpace(text[:index])
	} else if strings.HasPrefix(text, "<loom_attachments") {
		text = ""
	}
	if text == "" {
		if managedContext {
			return "CodexLoom managed work"
		}
		return "Attached files"
	}
	return text
}

func displayLoomControlTask(text string) (string, bool) {
	decoder := xml.NewDecoder(strings.NewReader(text))
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "owner_topic_input":
			var payload struct {
				Message string `xml:"message"`
			}
			if err := decoder.DecodeElement(&payload, &start); err == nil {
				if message := summarizeTopicText(payload.Message); message != "" {
					return message, true
				}
			}
		case "owner_topic_intervention":
			var payload struct {
				Guidance string `xml:"guidance"`
			}
			if err := decoder.DecodeElement(&payload, &start); err == nil {
				if guidance := summarizeTopicText(payload.Guidance); guidance != "" {
					return "Owner guidance: " + guidance, true
				}
			}
		}
	}
}

func (h *Hub) adoptRemoteTurnLocked(meta *Agent, rt *runtime, nativeTurnID string) *turnState {
	turn := &turnState{
		turnID: newIntegrationID("turn"), nativeTurnID: nativeTurnID, startedConfirmed: true, task: "Remote turn", source: "remote",
		startedAt: time.Now(), lastActivity: time.Now(), stopWatchdog: make(chan struct{}),
	}
	h.recordRuntimeTurnBindingLocked(meta, turn.turnID, nativeTurnID)
	rt.activeTurn = turn
	meta.Status = "running"
	meta.CurrentTask = turn.task
	meta.CurrentTurnID = turn.turnID
	meta.LastError = ""
	meta.UpdatedAt = now()
	h.persistRuntimeProjectionLocked()
	h.emitStatusLocked(meta, "running")
	h.emitLocked(meta.ID, "loom/turn-started", map[string]any{
		"turnId": turn.turnID, "task": turn.task, "source": turn.source,
		"providerId": publicProviderID(meta.ProviderID), "model": meta.Model,
	})
	return turn
}

func (h *Hub) bindActiveNativeTurnIDLocked(meta *Agent, turn *turnState, nativeTurnID string) {
	nativeTurnID = strings.TrimSpace(nativeTurnID)
	if turn == nil || turn.finished || nativeTurnID == "" || turn.nativeTurnID == nativeTurnID {
		return
	}
	firstBinding := turn.nativeTurnID == ""
	turn.nativeTurnID = nativeTurnID
	if firstBinding && turn.nativeTurnReady != nil {
		close(turn.nativeTurnReady)
	}
	h.recordRuntimeTurnBindingLocked(meta, turn.turnID, nativeTurnID)
	h.persistRuntimeProjectionLocked()
}

func (h *Hub) recordRuntimeTurnBindingLocked(meta *Agent, turnID, nativeTurnID string) {
	if meta == nil || turnID == "" || nativeTurnID == "" {
		return
	}
	if meta.RuntimeTurnBindings == nil {
		meta.RuntimeTurnBindings = map[string]string{}
	}
	meta.RuntimeTurnBindings[turnID] = nativeTurnID
	if rt := h.runtimes[meta.ID]; rt != nil {
		if contract, ok := rt.runtimeContract.(runtimeTurnCorrelationBinder); ok {
			contract.bindTurn(turnID, "", nativeTurnID)
		}
	}
}

func (h *Hub) onNotification(rt *runtime, method string, params json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta := h.agents[rt.agentID]
	if meta == nil {
		return
	}
	runtimeEvents := normalizeRuntimeEvents(rt, method, params)
	for _, runtimeEvent := range runtimeEvents {
		h.onRuntimeEventLocked(meta, rt, runtimeEvent, method, params)
	}
	// Codex reports model-route failures as raw `error` notifications. They do
	// not normalize into a RuntimeEvent, but still need to affect the active
	// Turn exactly once.
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
	h.emitLocked(meta.ID, method, runtimePublicPayload(params, meta.ThreadID, turnID, len(runtimeEvents) > 0))
}

func (h *Hub) onRuntimeEvent(rt *runtime, event RuntimeEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta := h.agents[rt.agentID]
	if meta != nil {
		h.onRuntimeEventLocked(meta, rt, event, "", nil)
	}
}

func (h *Hub) onCanonicalRuntimeEvent(rt *runtime, event runtimecontract.Event) {
	h.onRuntimeEvent(rt, compatibilityRuntimeEvent(event))
}

func compatibilityRuntimeEvent(event runtimecontract.Event) RuntimeEvent {
	result := RuntimeEvent{LoomTurnID: event.TurnID, NativeTurnID: event.RuntimeTurnRef}
	switch event.Kind {
	case runtimecontract.EventTurnStarted:
		result.Kind = RuntimeTurnStarted
	case runtimecontract.EventContent:
		if event.Content == nil {
			return result
		}
		result.ItemID = event.Content.ID
		result.Text = event.Content.Text
		if len(event.Content.Diagnostic) > 0 {
			_ = json.Unmarshal(event.Content.Diagnostic, &result.Item)
		}
		switch event.Content.Kind {
		case runtimecontract.ContentUserText:
			result.Kind = RuntimeUserInput
		case runtimecontract.ContentAssistantText:
			if event.ContentPhase == runtimecontract.ContentPhaseDelta {
				result.Kind = RuntimeTextDelta
			} else {
				result.Kind = RuntimeTextCompleted
			}
		case runtimecontract.ContentReasoning:
			if event.ContentPhase == runtimecontract.ContentPhaseDelta {
				result.Kind = RuntimeReasoningDelta
			} else {
				result.Kind = RuntimeReasoningCompleted
			}
		case runtimecontract.ContentToolCall:
			if event.Content.ToolCall != nil {
				_ = json.Unmarshal(event.Content.ToolCall.Arguments, &result.Item)
			}
			switch event.ContentPhase {
			case runtimecontract.ContentPhaseUpdated:
				result.Kind = RuntimeToolUpdated
			case runtimecontract.ContentPhaseCompleted:
				result.Kind = RuntimeToolCompleted
			default:
				result.Kind = RuntimeToolStarted
			}
		case runtimecontract.ContentToolResult:
			result.Kind = RuntimeToolCompleted
			if event.Content.ToolResult != nil {
				if result.Item == nil {
					result.Item = map[string]any{}
				}
				status := "failed"
				if event.Content.ToolResult.Success {
					status = "completed"
				}
				if _, ok := result.Item["id"]; !ok {
					result.Item["id"] = event.Content.ToolResult.ToolCallID
				}
				if _, ok := result.Item["output"]; !ok {
					result.Item["output"] = event.Content.ToolResult.Text
				}
				if _, ok := result.Item["status"]; !ok {
					result.Item["status"] = status
				}
			}
		}
	case runtimecontract.EventTerminal:
		result.Kind = RuntimeTurnCompleted
		if event.Outcome != nil {
			if event.Outcome.Failure != nil {
				result.Error = event.Outcome.Failure.Message
			}
			switch event.Outcome.State {
			case runtimecontract.LifecycleFailed, runtimecontract.LifecycleRejected, runtimecontract.LifecycleIndeterminate:
				result.Kind = RuntimeTurnFailed
			case runtimecontract.LifecycleInterrupted:
				result.Kind = RuntimeTurnInterrupted
			}
		}
	}
	return result
}

func (h *Hub) onRuntimeFailure(rt *runtime, err error) {
	agentID, predecessorTurnID, checkpointed := h.checkpointRuntimeFailure(rt, err)
	if !checkpointed {
		return
	}
	h.mu.Lock()
	h.scheduleTurnRecoveryLocked(agentID, predecessorTurnID)
	h.mu.Unlock()
}

func (h *Hub) onRuntimeIndeterminate(rt *runtime, err error) {
	agentID, predecessorTurnID, checkpointed := h.checkpointRuntimeFailure(rt, err)
	// The observed truth is durable before the old effect domain is closed;
	// even a failed checkpoint must fence the old effect domain so its late
	// effects cannot pollute the restored in-memory Turn snapshot.
	h.invalidateRuntimeEffectDomain(rt, err)
	if !checkpointed {
		return
	}
	h.mu.Lock()
	h.scheduleTurnRecoveryLocked(agentID, predecessorTurnID)
	h.mu.Unlock()
}

func (h *Hub) checkpointRuntimeFailure(rt *runtime, err error) (string, string, bool) {
	if err == nil {
		return "", "", false
	}
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return "", "", false
	}
	meta := h.agents[rt.agentID]
	if meta == nil {
		h.mu.Unlock()
		return "", "", false
	}
	if rt.activeTurn == nil || rt.activeTurn.finished {
		if isRuntimeIndeterminate(err) || recoveryView(meta) != nil {
			h.mu.Unlock()
			return "", "", false
		}
		meta.LastError = err.Error()
		meta.UpdatedAt = now()
		h.persistRuntimeProjectionLocked()
		h.emitLocked(meta.ID, "loom/runtime-failed", map[string]any{"error": err.Error()})
		h.mu.Unlock()
		return "", "", false
	}
	predecessorTurnID := rt.activeTurn.turnID
	checkpointed := h.finishTurnForRecoveryLocked(meta, rt, err)
	agentID := meta.ID
	h.mu.Unlock()
	return agentID, predecessorTurnID, checkpointed
}

func (h *Hub) onRuntimeEventLocked(meta *Agent, rt *runtime, runtimeEvent RuntimeEvent, method string, params json.RawMessage) {
	if recoveryEventFenced(meta, rt) {
		return
	}
	nativeTurnID := runtimeEvent.NativeTurnID
	eventTurnID := runtimeEvent.LoomTurnID
	if eventTurnID == "" && nativeTurnID != "" {
		for loomTurnID, boundNativeTurnID := range meta.RuntimeTurnBindings {
			if boundNativeTurnID == nativeTurnID {
				eventTurnID = loomTurnID
				break
			}
		}
	}
	// Correlate before any adoption or product event. A late response from an
	// uncertain predecessor must never render on or supersede its successor.
	if rt.activeTurn != nil && !rt.activeTurn.finished && eventTurnID != "" && eventTurnID != rt.activeTurn.turnID {
		return
	}
	if rt.activeTurn != nil && !rt.activeTurn.finished && eventTurnID == rt.activeTurn.turnID && nativeTurnID != "" {
		// Canonical Loom correlation is authoritative. The adapter may repair a
		// stale native binding (for example after an interrupt race) without
		// exposing or asking the control plane to interpret that native identity.
		h.bindActiveNativeTurnIDLocked(meta, rt.activeTurn, nativeTurnID)
	}

	if runtimeEvent.Kind == RuntimeTurnStarted {
		switch turn := rt.activeTurn; {
		case turn == nil || turn.finished:
			if nativeTurnID != "" {
				h.adoptRemoteTurnLocked(meta, rt, nativeTurnID)
			}
		case turn.nativeTurnID == "":
			h.bindActiveNativeTurnIDLocked(meta, turn, nativeTurnID)
			turn.startedConfirmed = true
		case nativeTurnID == "", turn.nativeTurnID == nativeTurnID:
			turn.startedConfirmed = true
		case !turn.startedConfirmed:
			h.bindActiveNativeTurnIDLocked(meta, turn, nativeTurnID)
			turn.startedConfirmed = true
		default:
			previousTurnID := turn.turnID
			h.finishTurnLocked(meta, rt, "interrupted", "superseded by active Runtime Turn "+nativeTurnID)
			h.adoptRemoteTurnLocked(meta, rt, nativeTurnID)
			log.Printf("[codex-loom] adopted successor Turn for %s after %s", meta.Name, previousTurnID)
		}
	}
	activeEvent := rt.activeTurn != nil && !rt.activeTurn.finished &&
		(runtimeEvent.LoomTurnID == "" || rt.activeTurn.turnID == runtimeEvent.LoomTurnID) &&
		(nativeTurnID == "" || rt.activeTurn.nativeTurnID == nativeTurnID)
	if text := runtimeEvent.Text; runtimeEvent.Kind == RuntimeUserInput && text != "" && activeEvent {
		task := displayUserTask(text)
		rt.activeTurn.task = task
		meta.CurrentTask = task
		meta.UpdatedAt = now()
		h.persistRuntimeProjectionLocked()
		h.emitStatusLocked(meta, "running")
	}
	if activeEvent {
		rt.activeTurn.lastActivity = time.Now()
		if runtimeEvent.Kind == RuntimeTextCompleted && runtimeEvent.Text != "" {
			rt.activeTurn.finalAnswer = runtimeEvent.Text
		}
		if runtimeEventIsModelProduced(runtimeEvent.Kind) {
			h.observeContextModelEventLocked(meta, rt.activeTurn)
		}
	}

	h.emitRuntimeEventsLocked(meta, rt, []RuntimeEvent{runtimeEvent})

	switch runtimeEvent.Kind {
	case RuntimeTurnCompleted, RuntimeTurnFailed, RuntimeTurnInterrupted:
		if rt.activeTurn == nil || rt.activeTurn.finished {
			return
		}
		if nativeTurnID != "" && rt.activeTurn.nativeTurnID == "" {
			log.Printf("[codex-loom] ignored terminal event without an authoritative Runtime Turn binding on %s: event=%s", meta.Name, nativeTurnID)
			return
		}
		if nativeTurnID != "" && rt.activeTurn.nativeTurnID != "" && rt.activeTurn.nativeTurnID != nativeTurnID {
			log.Printf("[codex-loom] ignored terminal event for stale Runtime Turn on %s: active=%s event=%s", meta.Name, rt.activeTurn.nativeTurnID, nativeTurnID)
			return
		}
		status := "completed"
		errMsg := runtimeEvent.Error
		if rt.activeTurn.forcedFailure == "" {
			rt.activeTurn.forcedFailure = customProviderModelRouteFailureDetail(meta.ProviderID, meta.Model, errMsg)
		}
		switch runtimeEvent.Kind {
		case RuntimeTurnFailed:
			status = "failed"
		case RuntimeTurnInterrupted:
			status = "interrupted"
		}
		h.finishTurnLocked(meta, rt, status, errMsg)
	}
}

func normalizeRuntimeEvents(rt *runtime, method string, params json.RawMessage) []RuntimeEvent {
	if backend := runtimeBackend(rt); backend != nil {
		return backend.NormalizeEvent(method, params)
	}
	// Unit-level notification tests historically construct a Runtime without a
	// live client. Their events are Codex-shaped, so use the same adapter.
	return (&codexAgentRuntime{}).NormalizeEvent(method, params)
}

func runtimeEventIsModelProduced(kind RuntimeEventKind) bool {
	switch kind {
	case RuntimeTextDelta, RuntimeTextCompleted, RuntimeReasoningDelta, RuntimeReasoningCompleted,
		RuntimeToolStarted, RuntimeToolUpdated, RuntimeToolCompleted:
		return true
	default:
		return false
	}
}

func runtimePublicPayload(params json.RawMessage, threadID, turnID string, compatibility bool) any {
	var payload map[string]any
	if json.Unmarshal(params, &payload) == nil {
		public := make(map[string]any, len(payload))
		nativeItemID, _ := payload["itemId"].(string)
		if item, ok := payload["item"].(map[string]any); ok && nativeItemID == "" {
			nativeItemID, _ = item["id"].(string)
		}
		itemID := publicRuntimeItemID(turnID, nativeItemID)
		for key, value := range payload {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "threadid":
				if threadID != "" {
					public["threadId"] = threadID
				}
			case "thread":
				public["thread"] = runtimePublicIdentityObject(value, threadID, "")
			case "turnid":
				if turnID != "" {
					public["turnId"] = turnID
				}
			case "turn":
				public["turn"] = runtimePublicIdentityObject(value, "", turnID)
			case "itemid":
				public["itemId"] = itemID
			case "item":
				if item, ok := value.(map[string]any); ok {
					public["item"] = canonicalRuntimeItem(item, itemID, threadID, turnID)
				}
			default:
				if runtimePublicActionKey(key) {
					public[key] = projectRuntimePublicAction(value, threadID, turnID)
				}
			}
		}
		if compatibility {
			public["compatibility"] = true
		}
		return public
	}
	return map[string]any{"compatibility": compatibility, "redacted": true}
}

func publicRuntimeItemID(turnID, nativeItemID string) string {
	if nativeItemID == "" {
		nativeItemID = "stream"
	}
	digest := sha256Hex([]byte(turnID + "\x00" + nativeItemID))
	return "item_" + digest[:16]
}

func runtimePublicIdentityObject(value any, threadID, turnID string) map[string]any {
	result := map[string]any{}
	if threadID != "" {
		result["id"] = threadID
	}
	if turnID != "" {
		result["id"] = turnID
	}
	if object, ok := value.(map[string]any); ok {
		for key, nested := range object {
			if strings.EqualFold(key, "id") {
				continue
			}
			if runtimePublicActionKey(key) {
				result[key] = projectRuntimePublicAction(nested, threadID, turnID)
			}
		}
	}
	return result
}

func canonicalRuntimeItem(item map[string]any, itemID, threadID, turnID string) map[string]any {
	result := map[string]any{"id": itemID}
	for key, value := range item {
		if strings.EqualFold(key, "id") {
			continue
		}
		if runtimePublicActionKey(key) {
			result[key] = projectRuntimePublicAction(value, threadID, turnID)
		}
	}
	return result
}

func projectRuntimePublicAction(value any, threadID, turnID string) any {
	switch typed := value.(type) {
	case map[string]any:
		result := map[string]any{}
		for key, nested := range typed {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "threadid":
				if threadID != "" {
					result["threadId"] = threadID
				}
			case "turnid":
				if turnID != "" {
					result["turnId"] = turnID
				}
			default:
				if runtimePublicActionKey(key) {
					result[key] = projectRuntimePublicAction(nested, threadID, turnID)
				}
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, nested := range typed {
			result[index] = projectRuntimePublicAction(nested, threadID, turnID)
		}
		return result
	default:
		return value
	}
}

func runtimePublicActionKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "type", "text", "delta", "status", "command", "cwd", "path", "filepath", "source", "destination", "target",
		"url", "data", "mimetype", "name", "toolname", "args", "arguments", "input", "content", "summary", "phase",
		"changes", "kind", "action", "diff", "patch", "aggregatedoutput", "output", "exitcode", "durationms", "iserror",
		"error", "message", "reason", "objective", "tokenbudget", "tokensused", "timeusedseconds", "createdat", "updatedat",
		"willretry", "goal", "result", "partialresult", "model", "providerid", "usage", "inputtokens", "cachedinputtokens",
		"outputtokens", "reasoningoutputtokens", "totaltokens", "width", "height":
		return true
	default:
		return false
	}
}

func (h *Hub) emitRuntimeEventsLocked(meta *Agent, rt *runtime, events []RuntimeEvent) {
	for _, event := range events {
		turnID := event.LoomTurnID
		if rt.activeTurn != nil {
			if turnID == "" {
				turnID = rt.activeTurn.turnID
			}
		}
		itemID := publicRuntimeItemID(turnID, event.ItemID)
		data := map[string]any{
			"turnId": turnID,
			"itemId": itemID,
			"text":   event.Text,
			"delta":  event.Text,
			"item":   canonicalRuntimeItem(event.Item, itemID, meta.ThreadID, turnID),
		}
		switch event.Kind {
		case RuntimeUserInput:
			if rt.activeTurn != nil && rt.activeTurn.source == "remote" {
				h.emitLocked(meta.ID, "loom/user-message", map[string]any{"text": event.Text})
			}
		case RuntimeTextDelta:
			h.emitLocked(meta.ID, "loom/text-delta", data)
		case RuntimeTextCompleted:
			h.emitLocked(meta.ID, "loom/text-completed", data)
		case RuntimeReasoningDelta:
			h.emitLocked(meta.ID, "loom/reasoning-delta", data)
		case RuntimeReasoningCompleted:
			h.emitLocked(meta.ID, "loom/reasoning-completed", data)
		case RuntimeToolStarted:
			h.emitLocked(meta.ID, "loom/tool-started", data)
		case RuntimeToolUpdated:
			h.emitLocked(meta.ID, "loom/tool-updated", data)
		case RuntimeToolCompleted:
			h.emitLocked(meta.ID, "loom/tool-completed", data)
		}
	}
}

func (h *Hub) onServerRequest(rt *runtime, id json.RawMessage, method string, params json.RawMessage) {
	h.mu.Lock()
	meta := h.agents[rt.agentID]
	if strings.Contains(strings.ToLower(method), "approval") {
		turnID, runtimeKind := "", ""
		if rt.activeTurn != nil && !rt.activeTurn.finished {
			turnID = rt.activeTurn.turnID
		}
		if meta != nil {
			runtimeKind = meta.RuntimeBinding.Kind
		}
		respond := func(decision string) error {
			if rt.client == nil {
				return nil
			}
			return rt.client.Respond(id, map[string]any{"decision": codexApprovalDecision(decision)})
		}
		created, err := h.requestRuntimeApprovalLocked(runtimeApprovalRequest{
			AgentID: rt.agentID, TurnID: turnID, RuntimeKind: runtimeKind, Method: method, Params: params,
		}, respond)
		if err != nil {
			if meta != nil {
				h.emitLocked(meta.ID, "loom/error", map[string]any{"message": "persist Approval request: " + err.Error()})
			}
			h.mu.Unlock()
			return
		}
		rt.approvals[created.ApprovalID].rpcID = append(json.RawMessage(nil), id...)
		h.mu.Unlock()
		return
	}
	// Unknown server->client request: answer with an error so codex won't hang.
	if meta != nil {
		h.emitLocked(meta.ID, "loom/server-request", map[string]any{"method": method, "params": params})
	}
	client := rt.client
	h.mu.Unlock()
	if client != nil {
		_ = client.RespondError(id, -32601, "CodexLoom does not handle "+method)
	}
}

func (h *Hub) onRuntimeApprovalRequest(rt *runtime, request RuntimeApprovalRequest) {
	h.mu.Lock()
	meta := h.agents[rt.agentID]
	if meta == nil || h.runtimes[rt.agentID] != rt || rt.activeTurn == nil || rt.activeTurn.finished {
		if request.Respond != nil {
			h.startWorkerLocked(func() { _ = request.Respond("abort") })
		}
		h.mu.Unlock()
		return
	}
	created, err := h.requestRuntimeApprovalLocked(runtimeApprovalRequest{
		AgentID: rt.agentID, TurnID: rt.activeTurn.turnID, RuntimeKind: meta.RuntimeBinding.Kind,
		Method: request.Method, Params: request.Params,
	}, request.Respond)
	if err != nil {
		h.emitLocked(meta.ID, "loom/error", map[string]any{"message": "persist Approval request: " + err.Error()})
		h.mu.Unlock()
		return
	}
	waiter := rt.approvals[created.ApprovalID]
	if request.Timeout > 0 && waiter != nil {
		agentID, approvalID, timeout, stop := meta.ID, created.ApprovalID, request.Timeout, h.stop
		h.startWorkerLocked(func() {
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				_, _ = h.ResolveApproval(agentID, approvalID, "timeout")
			case <-waiter.done:
			case <-stop:
			}
		})
	}
	h.mu.Unlock()
}

func (h *Hub) ResolveApproval(key, approvalID, decision string) (map[string]any, error) {
	decision, status, ok := normalizeApprovalDecision(decision)
	if !ok {
		return nil, errf(400, "unsupported Approval decision: %s", decision)
	}
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return nil, errf(404, "agent not found: %s", key)
	}
	current := h.approvals[approvalID]
	if current == nil || current.AgentID != meta.ID || current.Status != "pending" {
		h.mu.Unlock()
		return nil, errf(404, "no pending approval %s", approvalID)
	}
	rt := h.runtimes[meta.ID]
	if rt == nil {
		h.mu.Unlock()
		return nil, errf(409, "Approval %s cannot be resumed because its Runtime is unavailable", approvalID)
	}
	ap, ok := rt.approvals[approvalID]
	if !ok {
		h.mu.Unlock()
		return nil, errf(409, "Approval %s cannot be resumed because its Runtime request is unavailable", approvalID)
	}
	next := *current
	next.Status = status
	next.Decision = decision
	next.ResolvedAt = now()
	next.ResolutionError = ""
	if err := h.commitApprovalLocked(next); err != nil {
		// The durable terminal append failed. Stop presenting the request as
		// actionable in this process and deny it so the Runtime is not blocked.
		next.Status = "aborted"
		next.Decision = "abort"
		next.ResolutionError = "persist Approval terminal state: " + err.Error()
		h.approvals[approvalID] = &next
		closeApprovalWaiter(ap)
		delete(rt.approvals, approvalID)
		h.emitLocked(meta.ID, "loom/error", map[string]any{"message": next.ResolutionError})
		h.emitLocked(meta.ID, "loom/approval-resolved", approvalEventPayload(next))
		respond := ap.respond
		h.mu.Unlock()
		if respond != nil {
			_ = respond("abort")
		}
		return nil, errf(500, "%s", next.ResolutionError)
	}
	closeApprovalWaiter(ap)
	delete(rt.approvals, approvalID)
	respond := ap.respond
	h.mu.Unlock()

	if respond != nil {
		if err := respond(decision); err != nil {
			h.mu.Lock()
			failed := next
			failed.Status = "aborted"
			failed.Decision = "abort"
			failed.ResolvedAt = now()
			failed.ResolutionError = "respond Approval: " + err.Error()
			if appendErr := h.commitApprovalLocked(failed); appendErr != nil {
				failed.ResolutionError += "; persist failure: " + appendErr.Error()
				h.approvals[approvalID] = &failed
			}
			h.emitLocked(meta.ID, "loom/approval-resolved", approvalEventPayload(failed))
			h.mu.Unlock()
			return nil, errf(500, "%s", failed.ResolutionError)
		}
	}
	h.mu.Lock()
	h.emitLocked(meta.ID, "loom/approval-resolved", approvalEventPayload(next))
	h.mu.Unlock()
	return map[string]any{"approvalId": approvalID, "decision": decision, "status": status}, nil
}

func (h *Hub) finishTurnLocked(meta *Agent, rt *runtime, status, errMsg string) {
	h.finishTurnWithPendingLocked(meta, rt, status, errMsg, true)
}

func (h *Hub) finishTurnForRecoveryLocked(meta *Agent, rt *runtime, runtimeErr error) bool {
	turn := rt.activeTurn
	if turn == nil || turn.finished {
		return false
	}
	previous := *meta
	if meta.LastTurn != nil {
		last := *meta.LastTurn
		previous.LastTurn = &last
	}
	previous.TurnRecoveryMarkers = make(map[string]TurnRecoveryMarker, len(meta.TurnRecoveryMarkers))
	for predecessorTurnID, marker := range meta.TurnRecoveryMarkers {
		previous.TurnRecoveryMarkers[predecessorTurnID] = marker
	}
	cause, phase, code, summary := runtimeRecoveryDescriptor(runtimeErr)
	meta.Status = "interrupted"
	meta.CurrentTask = ""
	meta.CurrentTurnID = ""
	meta.LastError = "interrupted: " + summary
	meta.LastTurn = &TurnSummary{TurnID: turn.turnID, Task: turn.task, Status: "interrupted", CompletedAt: now()}
	meta.UpdatedAt = now()
	if meta.TurnRecoveryMarkers == nil {
		meta.TurnRecoveryMarkers = map[string]TurnRecoveryMarker{}
	}
	stamp := now()
	meta.TurnRecoveryMarkers[turn.turnID] = TurnRecoveryMarker{
		PredecessorTurnID: turn.turnID, NativeTurnID: turn.nativeTurnID,
		RuntimeKind: meta.RuntimeBinding.Kind, Cause: cause, FailurePhase: phase,
		FailureCode: code, Summary: summary, State: TurnRecoveryObserved,
		TopicID: turn.topicID, CreatedAt: stamp, UpdatedAt: stamp,
	}
	if err := h.persistAgentsLocked(); err != nil {
		*meta = previous
		log.Printf("[codex-loom] persist interrupted Turn before recovery: %v", err)
		return false
	}
	turn.finished = true
	close(turn.stopWatchdog)
	rt.activeTurn = nil
	h.abortTurnApprovalsLocked(meta.ID, turn.turnID, rt, "the Runtime Turn ended before the Approval was resolved")
	rt.approvals = map[string]*approval{}
	errMsg := ""
	if runtimeErr != nil {
		errMsg = runtimeErr.Error()
	}
	h.finishInboxAttemptLocked(turn, "interrupted", errMsg)
	h.finishAgentMessageTurnLocked(turn, "interrupted", errMsg)
	payload := map[string]any{
		"turnId": turn.turnID, "task": turn.task, "source": turn.source,
		"durationMs": time.Since(turn.startedAt).Milliseconds(), "topicId": turn.topicID,
	}
	if errMsg != "" {
		payload["error"] = summary
	}
	payload["recovery"] = recoveryView(meta)
	h.emitLocked(meta.ID, "loom/turn-interrupted", payload)
	if turn.topicID != "" {
		h.recordTopicWorkEventLocked(turn.topicID, TopicEvent{
			Type: "turn_interrupted", Actor: meta.Name, AgentID: meta.ID, Agent: meta.Name,
			Summary: "interrupted: " + summarizeTopicText(turn.task), Ref: &TopicRef{Type: "turn", ID: turn.turnID, Label: meta.Name}, CreatedAt: now(),
		})
	}
	h.emitStatusLocked(meta, "interrupted")
	return true
}

func runtimeRecoveryDescriptor(err error) (cause, phase, code, summary string) {
	if failure := runtimeFailureFromError(err); failure != nil {
		return "command_indeterminate", string(failure.Phase), failure.Code, "Runtime command outcome is indeterminate"
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "protocol") {
		return "protocol_failure", "", "", "Runtime protocol failed before the Turn outcome was confirmed"
	}
	cause = "process_exit"
	summary = "Runtime process exited before the Turn outcome was confirmed"
	return cause, phase, code, summary
}

func (h *Hub) finishTurnWithPendingLocked(meta *Agent, rt *runtime, status, errMsg string, startPending bool) {
	turn := rt.activeTurn
	if turn == nil || turn.finished {
		return
	}
	if turn.forcedFailure != "" {
		status = "failed"
		errMsg = turn.forcedFailure
	}
	previous := *meta
	if meta.LastTurn != nil {
		last := *meta.LastTurn
		previous.LastTurn = &last
	}
	previous.TurnRecoveryMarkers = make(map[string]TurnRecoveryMarker, len(meta.TurnRecoveryMarkers))
	for predecessorTurnID, marker := range meta.TurnRecoveryMarkers {
		previous.TurnRecoveryMarkers[predecessorTurnID] = marker
	}
	turn.finished = true
	close(turn.stopWatchdog)
	rt.activeTurn = nil

	evType := "loom/turn-completed"
	if status == "failed" {
		evType = "loom/turn-failed"
	} else if status == "interrupted" {
		evType = "loom/turn-interrupted"
	}
	payload := map[string]any{
		"turnId":     turn.turnID,
		"task":       turn.task,
		"source":     turn.source,
		"durationMs": time.Since(turn.startedAt).Milliseconds(),
		"topicId":    turn.topicID,
	}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	meta.Status = "idle"
	meta.CurrentTask = ""
	meta.CurrentTurnID = ""
	if status == "completed" {
		meta.LastError = ""
	} else if errMsg != "" {
		meta.LastError = errMsg
	} else {
		meta.LastError = status
	}
	meta.LastTurn = &TurnSummary{TurnID: turn.turnID, Task: turn.task, Status: status, CompletedAt: now()}
	for predecessorTurnID, marker := range meta.TurnRecoveryMarkers {
		if marker.RecoveryTurnID == turn.turnID {
			marker.State = TurnRecoveryCompleted
			marker.UpdatedAt = now()
			meta.TurnRecoveryMarkers[predecessorTurnID] = marker
		}
	}
	meta.UpdatedAt = now()
	if err := h.persistAgentsLocked(); err != nil {
		*meta = previous
		log.Printf("[codex-loom] persist terminal Turn before publish: %v", err)
		return
	}
	h.abortTurnApprovalsLocked(meta.ID, turn.turnID, rt, "the Runtime Turn ended before the Approval was resolved")
	rt.approvals = map[string]*approval{}
	h.finishInboxAttemptLocked(turn, status, errMsg)
	h.finishAgentMessageTurnLocked(turn, status, errMsg)
	h.emitLocked(meta.ID, evType, payload)
	if turn.topicID != "" {
		h.recordTopicWorkEventLocked(turn.topicID, TopicEvent{
			Type: "turn_" + status, Actor: meta.Name, AgentID: meta.ID, Agent: meta.Name,
			Summary: status + ": " + summarizeTopicText(turn.task), Ref: &TopicRef{Type: "turn", ID: turn.turnID, Label: meta.Name}, CreatedAt: now(),
		})
	}
	h.emitStatusLocked(meta, "idle")
	if startPending {
		h.startPendingWorkersLocked(meta.ID)
	}
}

// finishAgentMessageTurnLocked completes the handling attempt associated with
// this Turn. Once turn/start succeeded, delivery remains delivered forever;
// an interrupted or failed attempt is held until an explicit retry.
func (h *Hub) finishAgentMessageTurnLocked(turn *turnState, status, errMsg string) {
	if turn == nil || turn.agentMessageID == "" {
		return
	}
	msg := h.comms[turn.agentMessageID]
	if msg == nil {
		return
	}
	if msg.DeliveryStatus == "delivering" && turn.turnID != "" {
		if err := h.markAgentMessageHandlingRunningLocked(turn, msg.ToAgentID); err != nil {
			log.Printf("[codex-loom] establish message handling before finish %s: %v", msg.ID, err)
			return
		}
		msg = h.comms[turn.agentMessageID]
	}
	if msg == nil || (msg.DeliveryStatus != "delivering" && msg.DeliveryStatus != "delivered") {
		return
	}
	if msg.DeliveredTurnID != "" && turn.turnID != "" && msg.DeliveredTurnID != turn.turnID {
		return
	}

	next := *msg
	next.HandlingAttempts = cloneAgentMessageHandlingAttempts(msg.HandlingAttempts)
	next.UpdatedAt = now()

	// A failure before Codex confirms a Turn is still a delivery failure. It
	// requires an explicit retry and must never enter the automatic queue.
	if next.DeliveryStatus == "delivering" {
		next.DeliveryStatus = "failed"
		next.LastDeliveryError = strings.TrimSpace(errMsg)
		if next.LastDeliveryError == "" {
			next.LastDeliveryError = "turn did not start"
		}
		next.HandlingStatus = "pending"
		if err := h.commitAgentMessageLocked(next); err != nil {
			log.Printf("[codex-loom] save pre-start message failure: %v", err)
		}
		return
	}

	attemptIndex := -1
	for i := range next.HandlingAttempts {
		attempt := &next.HandlingAttempts[i]
		if (turn.handlingAttemptID != "" && attempt.ID == turn.handlingAttemptID) ||
			(turn.handlingAttemptID == "" && next.ActiveHandlingID != "" && attempt.ID == next.ActiveHandlingID) ||
			(turn.handlingAttemptID == "" && next.ActiveHandlingID == "" && turn.turnID != "" && attempt.TurnID == turn.turnID) {
			attemptIndex = i
			break
		}
	}
	if attemptIndex < 0 {
		startedAt := next.DeliveredAt
		if startedAt == "" {
			startedAt = next.UpdatedAt
		}
		attempt := AgentMessageHandlingAttempt{
			ID: newIntegrationID("matt"), TurnID: turn.turnID, Status: "running", StartedAt: startedAt,
		}
		next.HandlingAttempts = append(next.HandlingAttempts, attempt)
		attemptIndex = len(next.HandlingAttempts) - 1
	}

	attempt := &next.HandlingAttempts[attemptIndex]
	attempt.CompletedAt = next.UpdatedAt
	attempt.Error = strings.TrimSpace(errMsg)
	next.ActiveHandlingID = ""
	next.LastHandlingError = ""
	switch status {
	case "completed":
		attempt.Status = "completed"
		attempt.Error = ""
		next.HandlingStatus = "completed"
	case "interrupted":
		attempt.Status = "interrupted"
		next.HandlingStatus = "interrupted"
		next.LastHandlingError = attempt.Error
		if next.LastHandlingError == "" {
			next.LastHandlingError = "handling Turn interrupted"
			attempt.Error = next.LastHandlingError
		}
	default:
		attempt.Status = "failed"
		next.HandlingStatus = "failed"
		next.LastHandlingError = attempt.Error
		if next.LastHandlingError == "" {
			next.LastHandlingError = "handling Turn failed"
			attempt.Error = next.LastHandlingError
		}
	}
	if err := h.commitAgentMessageLocked(next); err != nil {
		log.Printf("[codex-loom] save message handling result: %v", err)
	}
}

// ---- public API ----

func (h *Hub) viewLocked(meta *Agent) AgentView {
	bindings := make(map[string]string, len(meta.RuntimeTurnBindings))
	for turnID, nativeTurnID := range meta.RuntimeTurnBindings {
		bindings[turnID] = nativeTurnID
	}
	markers := make(map[string]TurnRecoveryMarker, len(meta.TurnRecoveryMarkers))
	for turnID, marker := range meta.TurnRecoveryMarkers {
		markers[turnID] = marker
	}
	view := AgentView{
		Agent: *meta, PendingApprovals: []ApprovalView{}, LastSeq: h.seqs[meta.ID],
		nativeRuntimeRef: meta.RuntimeBinding.NativeRef, nativeTurnBindings: bindings, turnRecoveryMarkers: markers,
	}
	view.Recovery = recoveryView(meta)
	if meta.LastTurn != nil {
		last := *meta.LastTurn
		view.LastTurn = &last
	}
	view.RuntimeBinding.NativeRef = ""
	view.RuntimeBinding.SchemaVersion = 0
	view.RuntimeTurnBindings = nil
	view.TurnRecoveryMarkers = nil
	view.RuntimeCapabilities = h.runtimeCapabilitiesLocked(meta)
	if goal := h.goals[meta.ID]; goal != nil {
		copy := *goal
		view.Goal = &copy
	}
	view.PendingApprovals = h.pendingApprovalsLocked(meta.ID)
	if rt, ok := h.runtimes[meta.ID]; ok && runtimeBackend(rt) != nil && runtimeBackend(rt).Alive() {
		view.ProcessAlive = true
	}
	return view
}

func recoveryView(meta *Agent) *RecoveryView {
	if meta == nil || meta.LastTurn == nil {
		return nil
	}
	marker, ok := meta.TurnRecoveryMarkers[meta.LastTurn.TurnID]
	if !ok || marker.State == TurnRecoveryCompleted {
		return nil
	}
	return &RecoveryView{
		PredecessorTurnID: marker.PredecessorTurnID, RuntimeKind: marker.RuntimeKind,
		State: marker.State, Cause: marker.Cause, FailurePhase: marker.FailurePhase,
		FailureCode: marker.FailureCode, Summary: marker.Summary,
	}
}

func (h *Hub) runtimeCapabilitiesLocked(meta *Agent) RuntimeCapabilities {
	if meta == nil {
		return RuntimeCapabilities{}
	}
	backend := runtimeForKind(meta.RuntimeBinding.Kind)
	if rt := h.runtimes[meta.ID]; rt != nil {
		backend = runtimeBackend(rt)
	}
	if backend == nil {
		return RuntimeCapabilities{}
	}
	return backend.Capabilities()
}

func unsupportedRuntimeCapability(agent *Agent, capability string) error {
	kind := "Agent"
	if agent != nil {
		if runtimeKind := strings.TrimSpace(agent.RuntimeBinding.Kind); runtimeKind != "" {
			kind = strings.ToUpper(runtimeKind[:1]) + runtimeKind[1:]
		}
	}
	return errf(409, "%s Runtime does not support %s", kind, capability)
}

func runtimeForKind(kind string) AgentRuntime {
	switch kind {
	case "codex":
		return &codexAgentRuntime{}
	case "pi":
		return &piAgentRuntime{}
	default:
		return nil
	}
}

func applyRolloutStatus(view *AgentView) {
	if view.nativeRuntimeRef == "" {
		return
	}
	if view.Status == "running" && view.ProcessAlive {
		return
	}
	kind := view.RuntimeBinding.Kind
	if kind == "" {
		kind = "codex"
	}
	backend := runtimeForKind(kind)
	if backend == nil || !backend.Capabilities().History {
		return
	}
	latest, err := backend.LatestTurn(view.nativeRuntimeRef)
	if err != nil || latest == nil || latest.ID == "" {
		return
	}
	latest.ID = loomTurnIDFor(view, latest.ID)
	// A restart-interrupted Turn remains visible until explicitly continued or
	// dismissed. Its rollout has no terminal event by definition, so do not
	// mistake a recently-written orphan for a live external Turn.
	if view.LastTurn != nil && view.LastTurn.TurnID == latest.ID && view.LastTurn.Status == "interrupted" {
		return
	}
	switch latest.Status {
	case "running":
		if !externalTurnLooksLive(view.nativeRuntimeRef, latest.UpdatedAt) {
			view.Status = "interrupted"
			view.CurrentTask = ""
			view.CurrentTurnID = ""
			view.LastError = "interrupted: active Turn ended without a terminal event"
			view.LastTurn = &TurnSummary{
				TurnID:      latest.ID,
				Task:        displayRolloutTask(latest.Task),
				Status:      "interrupted",
				CompletedAt: latest.UpdatedAt,
			}
			if latest.UpdatedAt != "" {
				view.UpdatedAt = latest.UpdatedAt
			}
			return
		}
		task := displayRolloutTask(latest.Task)
		if task == "" {
			task = "external turn " + latest.ID
		}
		view.Status = "running"
		view.CurrentTask = task
		view.CurrentTurnID = latest.ID
		view.LastError = ""
		if latest.UpdatedAt != "" {
			view.UpdatedAt = latest.UpdatedAt
		}
	case "completed", "interrupted":
		if !view.ProcessAlive {
			view.Status = "idle"
			view.CurrentTask = ""
			view.CurrentTurnID = ""
		}
		view.LastTurn = &TurnSummary{
			TurnID:      latest.ID,
			Task:        displayRolloutTask(latest.Task),
			Status:      latest.Status,
			CompletedAt: latest.UpdatedAt,
		}
	}
}

func displayRolloutTask(task string) string {
	if strings.TrimSpace(task) == "" {
		return ""
	}
	return displayUserTask(task)
}

func externalTurnLooksLive(threadID, updatedAt string) bool {
	return timestampWithin(updatedAt, externalRunningStaleAfter)
}

func timestampWithin(ts string, d time.Duration) bool {
	if strings.TrimSpace(ts) == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return false
	}
	age := time.Since(t)
	return age >= 0 && age <= d
}
