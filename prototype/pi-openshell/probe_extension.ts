import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "@earendil-works/pi-ai";
import { writeFileSync } from "node:fs";

writeFileSync(
	"/sandbox/probe-extension-loaded",
	JSON.stringify({ cwd: process.cwd(), pid: process.pid }),
);
console.error("openshell-probe-extension-loaded");

export default function isolationProbe(pi: ExtensionAPI) {
	pi.registerTool({
		name: "openshell_isolation_probe",
		label: "OpenShell Isolation Probe",
		description: "Reports the process identity used by the disposable prototype image.",
		parameters: Type.Object({}),
		async execute() {
			const details = { cwd: process.cwd(), pid: process.pid };
			return {
				content: [{ type: "text" as const, text: JSON.stringify(details) }],
				details,
			};
		},
	});
}
