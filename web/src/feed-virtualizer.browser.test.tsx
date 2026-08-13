import { useVirtualizer } from "@tanstack/react-virtual";
import { act, useEffect } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, describe, expect, it } from "vitest";
import { AgentPane } from "./AgentPane";
import { publishThreadEvent } from "./thread-events";
import type { Agent } from "./types";
import "./i18n";
import "./index.css";

(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

type Row = { id: string; height: number; label: string };

function FeedHarness({ active, clearMeasurements, rows }: { active: boolean; clearMeasurements?: boolean; rows: Row[] }) {
  const virtualizer = useVirtualizer({
    count: rows.length,
    enabled: active,
    estimateSize: () => 96,
    getItemKey: (index) => rows[index]?.id || index,
    getScrollElement: () => document.querySelector<HTMLDivElement>("[data-feed-viewport]"),
    overscan: 6,
  });
  useEffect(() => {
    if (clearMeasurements) virtualizer.measure();
  }, [clearMeasurements, rows.length, virtualizer]);

  return (
    <div data-feed-viewport style={{ height: 600, overflowY: "auto", position: "relative" }}>
      <div style={{ height: virtualizer.getTotalSize(), position: "relative" }}>
        {active ? virtualizer.getVirtualItems().map((virtualRow) => {
          const row = rows[virtualRow.index];
          return (
            <div
              key={virtualRow.key}
              data-index={virtualRow.index}
              data-row={row.id}
              ref={virtualizer.measureElement}
              style={{
                height: row.height,
                left: 0,
                position: "absolute",
                top: 0,
                transform: `translateY(${virtualRow.start}px)`,
                width: "100%",
              }}
            >
              {row.label}
            </div>
          );
        }) : null}
      </div>
    </div>
  );
}

async function waitForGeometry(expectedRows: Row[]) {
  await expect.poll(() => {
    const elements = expectedRows.map((row) => document.querySelector<HTMLElement>(`[data-row="${row.id}"]`));
    if (elements.some((element) => !element)) return null;
    return elements.map((element) => ({
      height: element!.getBoundingClientRect().height,
      top: element!.getBoundingClientRect().top,
    }));
  }).toEqual(expectedRows.reduce<Array<{ height: number; top: number }>>((geometry, row, index) => {
    geometry.push({
      height: row.height,
      top: index === 0 ? 0 : geometry[index - 1].top + geometry[index - 1].height,
    });
    return geometry;
  }, []));
}

describe("Agent feed virtual row geometry", () => {
  let container: HTMLDivElement;
  let root: Root;

  afterEach(async () => {
    if (root) await act(() => root.unmount());
    container?.remove();
  });

  it("keeps usage and later Turns below a tall streaming message across updates and reactivation", async () => {
    container = document.createElement("div");
    document.body.replaceChildren(container);
    root = createRoot(container);

    const first = [{ id: "message", height: 320, label: "Tall Markdown message" }];
    await act(() => root.render(<FeedHarness active rows={first} />));
    await waitForGeometry(first);

    const appended = [
      first[0],
      { id: "usage", height: 36, label: "Token usage" },
      { id: "turn", height: 72, label: "Next Turn" },
    ];
    await act(() => root.render(<FeedHarness active rows={appended} />));
    await waitForGeometry(appended);

    const streaming = [{ ...appended[0], height: 548 }, appended[1], appended[2]];
    await act(() => root.render(<FeedHarness active rows={streaming} />));
    await waitForGeometry(streaming);

    await act(() => root.render(<FeedHarness active={false} rows={streaming} />));
    expect(document.querySelectorAll("[data-row]")).toHaveLength(0);
    await act(() => root.render(<FeedHarness active rows={streaming} />));
    await waitForGeometry(streaming);

    window.dispatchEvent(new Event("resize"));
    await waitForGeometry(streaming);
  });

  it("detects the historical cache-clear overlap without waiting for another resize entry", async () => {
    container = document.createElement("div");
    document.body.replaceChildren(container);
    root = createRoot(container);

    const first = [{ id: "message", height: 420, label: "Tall Markdown message" }];
    await act(() => root.render(<FeedHarness active rows={first} />));
    await waitForGeometry(first);

    const appended = [first[0], { id: "usage", height: 36, label: "Token usage" }];
    await act(() => root.render(<FeedHarness active clearMeasurements rows={appended} />));
    await expect.poll(() => {
      const message = document.querySelector<HTMLElement>('[data-row="message"]');
      const usage = document.querySelector<HTMLElement>('[data-row="usage"]');
      return message && usage ? usage.getBoundingClientRect().top - message.getBoundingClientRect().bottom : 0;
    }).toBeLessThan(0);
  });
});

const agent: Agent = {
  id: "geometry-agent",
  name: "geometry-agent",
  cwd: "/workspace/geometry-agent",
  threadId: "geometry-thread",
  runtimeBinding: { kind: "codex" },
  capabilitySnapshot: { revision: "geometry", capabilities: [] },
  sandbox: "workspace-write",
  approvalPolicy: "on-request",
  status: "idle",
  currentTask: "",
  currentTurnId: "",
  lastError: "",
  createdAt: "2026-08-01T00:00:00Z",
  updatedAt: "2026-08-01T00:00:00Z",
  processAlive: true,
  pendingApprovals: [],
  lastSeq: 0,
};

function assertNoFeedOverlap() {
  const rows = Array.from(document.querySelectorAll<HTMLElement>("[data-index]"));
  if (rows.length < 3) return false;
  return {
    rows: rows.length,
    gaps: rows.slice(1).map((row, index) =>
      Math.round((row.getBoundingClientRect().top - rows[index].getBoundingClientRect().bottom) * 100) / 100),
  }.gaps.every((gap) => gap >= 0);
}

describe("AgentPane feed layout", () => {
  let container: HTMLDivElement;
  let root: Root;
  let originalFetch: typeof window.fetch;

  afterEach(async () => {
    if (root) await act(() => root.unmount());
    container?.remove();
    if (originalFetch) window.fetch = originalFetch;
    delete window.codexLoom;
    delete window.codexHub;
  });

  it("never overlaps a growing Markdown message with usage or the next Turn", async () => {
    originalFetch = window.fetch;
    window.fetch = async (input) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
      const body = url.includes("/thread/history") ? { total: 0, turns: [] } : { artifacts: [] };
      return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    };
    container = document.createElement("div");
    container.style.height = "720px";
    container.style.display = "flex";
    document.body.replaceChildren(container);
    root = createRoot(container);
    const props = {
      agent,
      active: true,
      configRequestNonce: 0,
      pendingWork: [],
      humanRequests: [],
      onOpenPendingWork: () => {},
      onOpenHumanRequest: () => {},
      onHumanRequestChanged: () => {},
      onPendingWorkChanged: () => {},
      onOpenUsage: () => {},
      onTrackTopic: () => {},
      onError: (message: string) => { throw new Error(message); },
      onAgentUpdated: () => {},
    };
    await act(() => root.render(<AgentPane {...props} />));
    await act(() => publishThreadEvent(agent.id, {
      seq: 1,
      ts: "2026-08-13T00:00:00Z",
      type: "loom/runtime-event",
      data: { kind: "content", turnId: "turn-1", contentPhase: "delta", content: { id: "answer-1", kind: "assistant_text", text: "Initial answer\n\n" + "A measured Markdown paragraph. ".repeat(180) } },
    }));
    await expect.poll(() => document.querySelector('[data-index="0"]')?.getBoundingClientRect().height || 0).toBeGreaterThan(320);

    await act(() => {
      publishThreadEvent(agent.id, {
        seq: 2,
        ts: "2026-08-13T00:00:01Z",
        type: "loom/runtime-event",
        data: { kind: "usage", turnId: "turn-1", usage: { inputTokens: 500, cachedInputTokens: 200, outputTokens: 100, reasoningOutputTokens: 20, totalTokens: 600, calls: 1 } },
      });
      publishThreadEvent(agent.id, {
        seq: 3,
        ts: "2026-08-13T00:00:02Z",
        type: "loom/runtime-event",
        data: { kind: "content", turnId: "turn-2", contentPhase: "completed", content: { id: "user-2", kind: "user_text", text: "Next Turn" } },
      });
    });
    await expect.poll(assertNoFeedOverlap).toBe(true);

    await act(() => publishThreadEvent(agent.id, {
      seq: 4,
      ts: "2026-08-13T00:00:03Z",
      type: "loom/runtime-event",
      data: { kind: "content", turnId: "turn-1", contentPhase: "delta", content: { id: "answer-1", kind: "assistant_text", text: "\n\n" + "Streaming growth remains measured. ".repeat(140) } },
    }));
    await expect.poll(assertNoFeedOverlap).toBe(true);

    await act(() => root.render(<AgentPane {...props} active={false} />));
    await act(() => root.render(<AgentPane {...props} active />));
    window.dispatchEvent(new Event("resize"));
    await expect.poll(assertNoFeedOverlap).toBe(true);
  });
});
