# Drive Pi through its RPC mode

The Go Hub will supervise Pi's official `pi --mode rpc` process instead of introducing a Node SDK sidecar. RPC preserves Pi's coding-agent Session, tools, extensions, compaction, and event semantics across a supported cross-language boundary while avoiding another service and dependency layer; the integration will require a verified minimum Pi version and contract-test that baseline, while allowing newer versions to report explicit protocol errors at runtime.
