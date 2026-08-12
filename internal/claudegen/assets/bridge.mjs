import "@anthropic-ai/claude-agent-sdk";
import { readFile } from "node:fs/promises";
import { createInterface } from "node:readline";

const pkg = JSON.parse(await readFile(new URL("./node_modules/@anthropic-ai/claude-agent-sdk/package.json", import.meta.url), "utf8"));
const capabilities = ["interrupt", "approval", "hooks", "mcp", "session_resume"];
const identity = {
  protocolVersion: 1,
  bridgeBuild: "claude-bridge-v1",
  nodeVersion: process.versions.node,
  sdkVersion: pkg.version,
  claudeCodeVersion: pkg.claudeCodeVersion,
  capabilities
};

if (process.argv[2] === "--self-test") {
  process.stdout.write(JSON.stringify(identity) + "\n");
  process.exit(0);
}

const write = (frame) => process.stdout.write(JSON.stringify(frame) + "\n");
write({kind: "hello", ...identity, os: process.platform, arch: process.arch === "x64" ? "amd64" : process.arch});

let initialized = false;
const lines = createInterface({input: process.stdin, crlfDelay: Infinity, terminal: false});
for await (const line of lines) {
  let frame;
  try {
    frame = JSON.parse(line);
  } catch {
    process.stderr.write("Claude bridge received malformed JSON\n");
    process.exit(65);
  }
  if (!initialized) {
    if (frame.kind !== "initialize" || typeof frame.requestId !== "string" || typeof frame.agentId !== "string") {
      process.stderr.write("Claude bridge expected initialize\n");
      process.exit(65);
    }
    initialized = true;
    write({kind: "ready", requestId: frame.requestId, capabilities});
    continue;
  }
  if (frame.kind === "close") process.exit(0);
  if (frame.kind !== "command" || typeof frame.requestId !== "string" || typeof frame.operation !== "string") {
    process.stderr.write("Claude bridge received unknown control frame\n");
    process.exit(65);
  }
  write({kind: "response", requestId: frame.requestId, turnId: frame.turnId, operation: frame.operation, accepted: false, error: "command is not implemented by this bridge build"});
}
