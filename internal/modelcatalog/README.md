# Managed model catalog

`models.json` is the process-wide catalog loaded by CodexLoom's shared Codex
app-server. Codex treats `model_catalog_json` as a full replacement, so this
file must contain both the bundled OpenAI entries and supported custom models.

Sources for `codex-0.144.1+deepseek-v4-flash-0731`:

- OpenAI Codex `rust-v0.144.1`, `codex-rs/models-manager/models.json`
  - SHA-256: `dcab00231a5178a9c84b7aef4cc06a1e1359e37ee0dd7e69d5822c4b1de723b1`
- DeepSeek Codex integration `models.json`, retrieved 2026-08-01
  - SHA-256: `9c188ff25d2b573d4fd962f029384b25f83c32f726ac7ba66e7744d61011ceb4`
  - Only `deepseek-v4-flash` is included. `deepseek-v4-pro` is omitted until
    its public API availability is verified.

The eight OpenAI model objects are retained unchanged and in source order.
The DeepSeek Flash object is retained except for `priority`, which is changed
from `1` to `50` so it remains selectable without replacing Codex's current
OpenAI default model.

The static catalog disables Codex's dynamic native model-catalog refresh for
this process. When the supported Codex baseline changes, regenerate the union
from that exact Codex catalog and repeat the app-server and model canaries.
