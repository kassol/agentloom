import "@anthropic-ai/claude-agent-sdk";
import { readFile } from "node:fs/promises";

if (process.argv[2] !== "--self-test") process.exit(64);
const pkg = JSON.parse(await readFile(new URL("./node_modules/@anthropic-ai/claude-agent-sdk/package.json", import.meta.url), "utf8"));
process.stdout.write(JSON.stringify({
  protocolVersion: 1,
  bridgeBuild: "claude-bridge-v1",
  nodeVersion: process.versions.node,
  sdkVersion: pkg.version,
  claudeCodeVersion: pkg.claudeCodeVersion,
  capabilities: ["interrupt", "approval", "hooks", "mcp", "session_resume"]
}) + "\n");
