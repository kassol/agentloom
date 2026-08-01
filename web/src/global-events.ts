import type { LoomEvent } from "./types";

export type GlobalEventState = "connecting" | "live" | "reconnecting" | "paused";

type EventListener = (event: LoomEvent) => void;
type StateListener = (state: GlobalEventState) => void;

const eventListeners = new Set<EventListener>();
const stateListeners = new Set<StateListener>();

let source: EventSource | null = null;
let state: GlobalEventState = "connecting";
let openedOnce = false;

function setState(next: GlobalEventState) {
  if (state === next) return;
  state = next;
  for (const listener of stateListeners) listener(state);
}

function publish(event: LoomEvent) {
  for (const listener of eventListeners) listener(event);
}

function closeSource(nextState: GlobalEventState) {
  source?.close();
  source = null;
  setState(nextState);
}

function ensureSource() {
  if (source || eventListeners.size === 0 || typeof EventSource === "undefined") return;

  setState(openedOnce ? "reconnecting" : "connecting");
  const next = new EventSource("/api/events");
  source = next;
  next.onopen = () => {
    if (source !== next) return;
    openedOnce = true;
    setState("live");
    publish({
      seq: 0,
      type: "loom/reconcile",
      ts: new Date().toISOString(),
      data: { reason: "global SSE connected" },
    });
  };
  next.onerror = () => {
    if (source === next) setState("reconnecting");
  };
  next.onmessage = (message) => {
    if (source !== next) return;
    try {
      publish(JSON.parse(message.data) as LoomEvent);
    } catch {
      // A malformed event must not terminate the browser-managed SSE retry loop.
    }
  };
}

export function subscribeGlobalEvents(listener: EventListener) {
  eventListeners.add(listener);
  ensureSource();
  return () => {
    eventListeners.delete(listener);
    if (eventListeners.size === 0) closeSource("paused");
  };
}

export function subscribeGlobalEventState(listener: StateListener) {
  stateListeners.add(listener);
  listener(state);
  return () => {
    stateListeners.delete(listener);
  };
}

export function globalEventState() {
  return state;
}

export function resetGlobalEventsForTests() {
  closeSource("connecting");
  eventListeners.clear();
  stateListeners.clear();
  openedOnce = false;
}
