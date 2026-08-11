import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  globalEventState,
  resetGlobalEventsForTests,
  subscribeGlobalEvents,
  subscribeGlobalEventState,
} from "./global-events";

class FakeEventSource {
  static instances: FakeEventSource[] = [];

  readonly url: string;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  close = vi.fn();

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  open() {
    this.onopen?.();
  }

  error() {
    this.onerror?.();
  }

  message(value: unknown) {
    this.onmessage?.(new MessageEvent("message", { data: JSON.stringify(value) }));
  }
}

describe("global event stream", () => {
  beforeEach(() => {
    resetGlobalEventsForTests();
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  afterEach(() => {
    resetGlobalEventsForTests();
    vi.unstubAllGlobals();
  });

  it("shares one EventSource across all workspace subscribers", () => {
    const first = vi.fn();
    const second = vi.fn();
    const unsubscribeFirst = subscribeGlobalEvents(first);
    const unsubscribeSecond = subscribeGlobalEvents(second);

    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.instances[0].url).toBe("/api/agents/events");
    FakeEventSource.instances[0].open();
    FakeEventSource.instances[0].message({ seq: 8, ts: "now", type: "loom/thread-event", data: { agentId: "agent-1" } });

    expect(first).toHaveBeenCalledWith(expect.objectContaining({ type: "loom/thread-event" }));
    expect(second).toHaveBeenCalledWith(expect.objectContaining({ type: "loom/thread-event" }));
    unsubscribeFirst();
    expect(FakeEventSource.instances[0].close).not.toHaveBeenCalled();
    unsubscribeSecond();
    expect(FakeEventSource.instances[0].close).toHaveBeenCalledOnce();
  });

  it("reports reconnecting and reconciles when EventSource reopens", () => {
    const events = vi.fn();
    const states: string[] = [];
    const unsubscribeState = subscribeGlobalEventState((value) => states.push(value));
    const unsubscribeEvents = subscribeGlobalEvents(events);
    const first = FakeEventSource.instances[0];
    first.open();
    expect(globalEventState()).toBe("live");

    first.error();
    expect(globalEventState()).toBe("reconnecting");
    first.open();
    expect(globalEventState()).toBe("live");
    expect(events).toHaveBeenCalledWith(expect.objectContaining({ type: "loom/reconcile" }));
    expect(states).toEqual(expect.arrayContaining(["connecting", "live", "reconnecting"]));

    unsubscribeEvents();
    unsubscribeState();
  });

  it("keeps the shared stream open when an embedded browser reports hidden", () => {
    const unsubscribe = subscribeGlobalEvents(vi.fn());
    const first = FakeEventSource.instances[0];
    first.open();

    Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
    document.dispatchEvent(new Event("visibilitychange"));

    expect(first.close).not.toHaveBeenCalled();
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(globalEventState()).toBe("live");
    unsubscribe();
  });
});
