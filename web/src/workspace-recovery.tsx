import { Component, useEffect, type ErrorInfo, type ReactNode } from "react";
import { RotateCw, TriangleAlert } from "lucide-react";
import { Button } from "./components/ui/button";

const reloadKey = "codexloom-stale-chunk-reload";
const reloadParam = "_loom_reload";

export function isChunkLoadError(reason: unknown) {
  const message = String((reason as { message?: unknown } | null)?.message || reason || "");
  return /dynamically imported module|importing a module script failed|failed to load module script|error loading dynamically imported module|unable to preload (?:css|module)|preload.*failed|module script.*mime type/i.test(message);
}

export function requestWorkspaceReload(force = false) {
  const now = Date.now();
  const lastReload = Number(sessionStorage.getItem(reloadKey) || "0");
  if (!force && now - lastReload < 30_000) return false;

  sessionStorage.setItem(reloadKey, String(now));
  const url = new URL(window.location.href);
  url.searchParams.set(reloadParam, String(now));
  window.location.replace(url);
  return true;
}

export function markWorkspaceReady() {
  sessionStorage.removeItem(reloadKey);
  const url = new URL(window.location.href);
  if (!url.searchParams.has(reloadParam)) return;
  url.searchParams.delete(reloadParam);
  window.history.replaceState(window.history.state, "", url);
}

export function WorkspaceReady({ view }: { view: string }) {
  useEffect(() => markWorkspaceReady(), [view]);
  return null;
}

type WorkspaceErrorBoundaryProps = {
  children: ReactNode;
  resetKey: string;
  onReload?: (force?: boolean) => boolean;
};

type WorkspaceErrorBoundaryState = {
  error: Error | null;
  reloading: boolean;
};

export class WorkspaceErrorBoundary extends Component<WorkspaceErrorBoundaryProps, WorkspaceErrorBoundaryState> {
  state: WorkspaceErrorBoundaryState = { error: null, reloading: false };

  static getDerivedStateFromError(error: Error): WorkspaceErrorBoundaryState {
    return { error, reloading: false };
  }

  componentDidCatch(error: Error, _info: ErrorInfo) {
    (window as typeof window & { __codexLoomWorkspaceError?: { message: string; stack?: string } }).__codexLoomWorkspaceError = {
      message: error.message,
      stack: error.stack,
    };
    if (!isChunkLoadError(error)) return;
    const reloading = (this.props.onReload || requestWorkspaceReload)();
    if (reloading) this.setState({ reloading: true });
  }

  componentDidUpdate(previous: WorkspaceErrorBoundaryProps) {
    if (previous.resetKey !== this.props.resetKey && this.state.error) {
      this.setState({ error: null, reloading: false });
    }
  }

  render() {
    if (!this.state.error) return this.props.children;
    const chunkFailure = isChunkLoadError(this.state.error);

    return (
      <main className="flex min-h-0 min-w-0 flex-1 items-center justify-center bg-background px-5" role="alert">
        <div className="w-full max-w-sm border border-border bg-card p-5 text-center shadow-card">
          {this.state.reloading ? <RotateCw className="mx-auto size-5 animate-spin text-primary" /> : <TriangleAlert className="mx-auto size-5 text-warning" />}
          <h1 className="mt-3 text-[13px] font-semibold">{this.state.reloading ? "Loading the current workspace" : "This workspace could not load"}</h1>
          <p className="mt-1.5 text-[11px] leading-5 text-muted-foreground">
            {this.state.reloading
              ? "CodexLoom is refreshing after an update."
              : chunkFailure
                ? "The page may still be using files from an earlier CodexLoom build."
                : this.state.error.message || "CodexLoom encountered an unexpected workspace error."}
          </p>
          {!this.state.reloading ? (
            <Button className="mt-4" onClick={() => (this.props.onReload || requestWorkspaceReload)(true)}>
              <RotateCw />
              Reload workspace
            </Button>
          ) : null}
        </div>
      </main>
    );
  }
}
