# Keep Agent Sessions in the Loom data directory

Pi Agents inherit the Owner's native Pi authentication, settings, skills, and extensions, but their Session JSONL files live in a fixed per-Agent location under the Loom data directory. This keeps Runtime customization native while making the long-lived Agent's primary working history part of Loom's stable data and recovery boundary instead of an incidental entry in the user's global Pi session directory.
