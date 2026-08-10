# Do not migrate legacy Agent records

This fork requires the new Loom-owned Thread identity and Runtime Binding on every Agent and will not migrate Agent records written by the original Codex-only storage model. The fork has no existing Agent data to preserve, so carrying transitional fields, dual sources of truth, and recovery logic would add permanent complexity for a migration with no current user; existing Agents must be recreated.
