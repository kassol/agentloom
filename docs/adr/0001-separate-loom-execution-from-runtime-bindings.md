# Separate Loom execution from Runtime Bindings

Loom owns the logical Thread and Turn identities and a small runtime-neutral execution event vocabulary, while each Agent's Runtime Binding stores the runtime kind and an opaque native conversation reference. Runtime-native history remains authoritative and is projected through a runtime-specific reader; optional capabilities are exposed explicitly instead of leaking Codex identifiers, forcing false parity, or treating missing data as empty data.
