package hub

const (
	RuntimeInterruptionClean     = "clean"
	RuntimeInterruptionAmbiguous = "ambiguous"
	RuntimeInterruptionTerminal  = "terminal"
)

// RuntimeInterruptionEvidence is the bounded runtime-neutral evidence used by
// Loom's restart recovery policy. Native payloads stay inside each adapter.
type RuntimeInterruptionEvidence struct {
	Status          string
	TerminalStatus  string
	LeafEntryID     string
	UnfinishedTools []RuntimeToolEvidence
}

type RuntimeToolEvidence struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Command   string `json:"command,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
}
