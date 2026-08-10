# Drive Pi through its RPC mode

The Go Hub will supervise Pi's official `pi --mode rpc` process instead of introducing a Node SDK sidecar. RPC preserves Pi's coding-agent Session, tools, extensions, compaction, and event semantics across a supported cross-language boundary while avoiding another service and dependency layer; the integration requires Pi `0.84.1` or newer and contract-tests that baseline, while allowing newer versions to report explicit protocol errors at runtime.
