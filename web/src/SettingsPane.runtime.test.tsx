import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SettingsPane } from "./SettingsPane";

describe("Claude Runtime generation settings", () => {
  beforeEach(() => {
	vi.stubGlobal("matchMedia", vi.fn(() => ({ matches: true, addEventListener: vi.fn(), removeEventListener: vi.fn() })));
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
      if (path === "/api/runtime-generations/claude/install") {
        expect(init?.method).toBe("POST");
        expect(JSON.parse(String(init?.body))).toEqual({ acceptTerms: true });
      }
      return new Response(JSON.stringify({ generation: {
        state: path.endsWith("/install") ? "staged" : "install_required",
        developerPreview: true, productionReady: false, termsAccepted: false, termsRevision: "terms-1", termsUrl: "https://example.test/terms",
        required: { id: "claude-v1", nodeVersion: "24.19.0", sdkVersion: "0.3.228", claudeCodeVersion: "2.1.228" },
        platform: { os: "darwin", arch: "arm64", supported: true },
        staged: path.endsWith("/install") ? { id: "claude-v1" } : undefined,
      }}), { status: 200, headers: { "Content-Type": "application/json" } });
    }));
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("shows the exact preview row and requires explicit terms before install", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(<QueryClientProvider client={client}><SettingsPane
      section="system" remote={null} backupStatus={{ dir: "", count: 0, totalBytes: 0, backups: [], retention: { minCount: 1, maxCount: 1, maxBytes: 1, maxAgeDays: 1 } }}
      backingUp={false} restarting={false} restartStatus={{}} onSectionChange={() => {}} onRemoteUpdated={() => {}} onBackup={() => {}} onRestart={() => {}} onOpenExternal={() => {}} onError={() => {}}
    /></QueryClientProvider>);
    await view.findByText(/Node 24\.19\.0/);
    const install = view.getByRole("button", { name: "Install generation" });
    expect(install).toBeDisabled();
    fireEvent.click(view.getByRole("checkbox", { name: /Anthropic terms/ }));
    fireEvent.click(install);
    await waitFor(() => expect(fetch).toHaveBeenCalledWith("/api/runtime-generations/claude/install", expect.objectContaining({ method: "POST" })));
    await view.findByText(/staged/i);
  });
});
