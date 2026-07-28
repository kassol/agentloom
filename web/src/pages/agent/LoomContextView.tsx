import type { ReactNode } from "react";
import {
  RawEnvelope,
  StructuredContextRow as ContextRow,
} from "../../components/StructuredContext";
import { MarkdownContent } from "./markdown";

export interface LoomContextRelationship {
  id: string;
  type: string;
  direction: string;
  counterpart: string;
  description: string;
}

export interface LoomContextEnvelope {
  raw: string;
  version: string;
  compiledAt: string;
  epochId: string;
  policies: string[];
  turn: {
    origin: string;
    trust: string;
    authority: string;
    kind: string;
  };
  agentPrompt?: {
    revision: string;
    content: string;
  };
  agentProfile?: {
    revision: string;
    name: string;
    identity: string;
    domain: string;
    scope: string;
  };
  relationships?: {
    revision: string;
    scope: string;
    entries: LoomContextRelationship[];
    scopeNote: string;
  };
  coverage?: {
    attemptId: string;
    mode: string;
    fragments: Array<{ key: string; revision: string }>;
  };
}

function directChild(root: Element, name: string): Element | null {
  return Array.from(root.children).find((child) => child.nodeName === name) || null;
}

function directChildText(root: Element | null, name: string): string {
  return root ? directChild(root, name)?.textContent?.trim() || "" : "";
}

export function splitTrailingLoomContext(raw: string): {
  content: string;
  context: LoomContextEnvelope | null;
} {
  const trimmed = raw.trimEnd();
  if (!trimmed.endsWith("</loom_context>")) {
    return { content: raw, context: null };
  }

  const candidates: number[] = [];
  const startPattern = /<loom_context\b/g;
  let match: RegExpExecArray | null;
  while ((match = startPattern.exec(trimmed)) !== null) {
    candidates.push(match.index);
  }

  for (const start of candidates) {
    const xml = trimmed.slice(start);
    const document = new DOMParser().parseFromString(xml, "application/xml");
    const root = document.documentElement;
    if (
      root?.nodeName !== "loom_context" ||
      document.getElementsByTagName("parsererror").length > 0
    ) {
      continue;
    }

    const policy = directChild(root, "context_policy");
    const turn = directChild(root, "loom_turn_context");
    const prompt = directChild(root, "loom_agent_prompt");
    const profile = directChild(root, "loom_agent_profile");
    const relationshipSnapshot = directChild(root, "loom_agent_relationships");
    const relationshipsRoot = relationshipSnapshot
      ? directChild(relationshipSnapshot, "relationships")
      : null;
    const manifest = directChild(root, "coverage_manifest");

    return {
      content: trimmed.slice(0, start).trimEnd(),
      context: {
        raw: xml,
        version: root.getAttribute("version") || "",
        compiledAt: root.getAttribute("compiled_at") || "",
        epochId: root.getAttribute("epoch_id") || "",
        policies: policy
          ? Array.from(policy.children)
              .filter((child) => child.nodeName === "rule")
              .map((child) => child.textContent?.trim() || "")
              .filter(Boolean)
          : [],
        turn: {
          origin: turn?.getAttribute("origin") || "",
          trust: turn?.getAttribute("trust") || "",
          authority: turn?.getAttribute("authority") || "",
          kind: turn?.getAttribute("kind") || "",
        },
        agentPrompt: prompt
          ? {
              revision: prompt.getAttribute("revision") || "",
              content: directChildText(prompt, "content"),
            }
          : undefined,
        agentProfile: profile
          ? {
              revision: profile.getAttribute("revision") || "",
              name: profile.getAttribute("name") || "",
              identity: directChildText(profile, "identity"),
              domain: directChildText(profile, "domain"),
              scope: directChildText(profile, "scope"),
            }
          : undefined,
        relationships: relationshipSnapshot
          ? {
              revision: relationshipSnapshot.getAttribute("revision") || "",
              scope: relationshipSnapshot.getAttribute("scope") || "",
              entries: relationshipsRoot
                ? Array.from(relationshipsRoot.children)
                    .filter((child) => child.nodeName === "relationship")
                    .map((relationship) => ({
                      id: relationship.getAttribute("id") || "",
                      type: relationship.getAttribute("type") || "",
                      direction: relationship.getAttribute("direction") || "",
                      counterpart:
                        relationship.getAttribute("counterpart_name") ||
                        relationship.getAttribute("counterpart_agent_id") ||
                        "",
                      description: directChildText(relationship, "description"),
                    }))
                : [],
              scopeNote: directChildText(relationshipSnapshot, "scope_note"),
            }
          : undefined,
        coverage: manifest
          ? {
              attemptId: manifest.getAttribute("attempt_id") || "",
              mode: manifest.getAttribute("mode") || "",
              fragments: Array.from(manifest.children)
                .filter((child) => child.nodeName === "fragment")
                .map((fragment) => ({
                  key: fragment.getAttribute("key") || "",
                  revision: fragment.getAttribute("revision") || "",
                })),
            }
          : undefined,
      },
    };
  }

  return { content: raw, context: null };
}

function readable(value: string) {
  return value
    .split("_")
    .filter(Boolean)
    .map((part) => part[0]?.toUpperCase() + part.slice(1))
    .join(" ");
}

function shortEpoch(epochId: string) {
  const epoch = epochId.split(":").at(-1) || epochId;
  return epoch ? `...${epoch.slice(-8)}` : "-";
}

function SourceDetails({
  label,
  revision,
  summary,
  children,
}: {
  label: string;
  revision: string;
  summary: string;
  children: ReactNode;
}) {
  return (
    <details className="group/source border-b border-border/60 last:border-b-0">
      <summary className="flex min-w-0 cursor-pointer list-none items-center gap-2 py-2 text-[11px] hover:text-foreground">
        <span className="text-[8px] transition-transform group-open/source:rotate-90">▶</span>
        <span className="shrink-0 font-medium text-foreground/80">{label}</span>
        <span className="min-w-0 flex-1 truncate text-muted-foreground">{summary}</span>
        {revision && (
          <span className="max-w-[42%] shrink-0 truncate font-mono text-[9px] text-muted-foreground/70">
            {revision}
          </span>
        )}
      </summary>
      <div className="mb-2 border-l-2 border-border/80 pl-3">{children}</div>
    </details>
  );
}

export function LoomContextView({ context }: { context: LoomContextEnvelope }) {
  const durableSources = [
    context.agentPrompt,
    context.agentProfile,
    context.relationships,
  ].filter(Boolean).length;

  return (
    <details className="group/context mt-2 border-t border-border/70 pt-2 text-[10.5px] text-muted-foreground">
      <summary className="flex min-h-9 cursor-pointer list-none select-none items-center gap-2 rounded-sm py-1 font-mono outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/35">
        <span className="text-[8px] transition-transform group-open/context:rotate-90">▶</span>
        <span>loom context</span>
        <span className="ml-auto shrink-0 text-[9px]">
          {durableSources > 0 ? `${durableSources} sources · ` : ""}
          {shortEpoch(context.epochId)}
        </span>
      </summary>

      <div className="mt-2 divide-y divide-border/60 border-y border-border/60">
        <ContextRow label="Snapshot">
          <span className="font-mono text-[10.5px]">
            {context.compiledAt || `version ${context.version || "1"}`}
          </span>
        </ContextRow>
        <ContextRow label="Turn">
          {[
            readable(context.turn.origin),
            readable(context.turn.trust),
            readable(context.turn.authority),
            readable(context.turn.kind),
          ]
            .filter(Boolean)
            .join(" · ") || "-"}
        </ContextRow>

        <div className="grid min-w-0 gap-2 py-2.5 sm:grid-cols-[112px_1fr] sm:gap-3">
          <span className="font-mono text-[9px] font-semibold uppercase text-muted-foreground">
            Durable context
          </span>
          <div className="min-w-0 border-y border-border/60">
            {context.agentPrompt && (
              <SourceDetails
                label="Agent prompt"
                revision={context.agentPrompt.revision}
                summary="Loom operating context"
              >
                <div className="py-2 text-[11.5px] leading-5 text-foreground/75">
                  <MarkdownContent content={context.agentPrompt.content} />
                </div>
              </SourceDetails>
            )}
            {context.agentProfile && (
              <SourceDetails
                label="Agent profile"
                revision={context.agentProfile.revision}
                summary={context.agentProfile.name || context.agentProfile.identity}
              >
                <ContextRow label="Identity">{context.agentProfile.identity || "-"}</ContextRow>
                <ContextRow label="Domain">
                  <MarkdownContent content={context.agentProfile.domain} />
                </ContextRow>
                <ContextRow label="Scope">
                  <MarkdownContent content={context.agentProfile.scope} />
                </ContextRow>
              </SourceDetails>
            )}
            {context.relationships && (
              <SourceDetails
                label="Relationships"
                revision={context.relationships.revision}
                summary={`${context.relationships.entries.length} direct active`}
              >
                {context.relationships.entries.map((relationship) => (
                  <section
                    key={relationship.id}
                    className="border-b border-border/60 py-2 last:border-b-0"
                  >
                    <div className="flex min-w-0 flex-wrap items-center gap-2">
                      <span className="font-mono text-[9px] font-semibold uppercase text-primary">
                        {relationship.type}
                      </span>
                      <span className="font-medium text-foreground/80">
                        {relationship.counterpart || relationship.id}
                      </span>
                      <span className="font-mono text-[9px] text-muted-foreground">
                        {readable(relationship.direction)}
                      </span>
                    </div>
                    {relationship.description && (
                      <div className="mt-1 text-[11.5px] leading-5 text-foreground/70">
                        {relationship.description}
                      </div>
                    )}
                  </section>
                ))}
                {context.relationships.entries.length === 0 && (
                  <div className="py-2 text-muted-foreground">
                    No direct active Organization or Collaboration relationships.
                  </div>
                )}
                {context.relationships.scopeNote && (
                  <div className="border-t border-border/60 py-2 text-[10.5px] leading-5 text-muted-foreground">
                    {context.relationships.scopeNote}
                  </div>
                )}
              </SourceDetails>
            )}
            {durableSources === 0 && (
              <div className="py-2 text-[11px] text-muted-foreground">
                No durable fragments included in this Turn.
              </div>
            )}
          </div>
        </div>

        {context.coverage && (
          <ContextRow label="Coverage">
            <div className="font-mono text-[10px] text-muted-foreground">
              {[context.coverage.mode, context.coverage.attemptId].filter(Boolean).join(" · ")}
            </div>
            {context.coverage.fragments.length > 0 && (
              <div className="mt-1.5 flex flex-wrap gap-1.5">
                {context.coverage.fragments.map((fragment) => (
                  <span
                    key={`${fragment.key}:${fragment.revision}`}
                    className="rounded-sm border border-border px-1.5 py-0.5 font-mono text-[9px]"
                  >
                    {fragment.key.replace(/^loom_agent_/, "")} · {fragment.revision}
                  </span>
                ))}
              </div>
            )}
          </ContextRow>
        )}

        {context.policies.length > 0 && (
          <details className="group/policy py-2">
            <summary className="flex cursor-pointer list-none items-center gap-2 font-mono text-[10px] hover:text-foreground">
              <span className="text-[8px] transition-transform group-open/policy:rotate-90">▶</span>
              context policy
              <span className="text-[9px]">{context.policies.length} rules</span>
            </summary>
            <ol className="mt-2 list-decimal space-y-1 border-l-2 border-border/80 pl-7 pr-2 text-[10.5px] leading-5 text-foreground/70">
              {context.policies.map((rule, index) => (
                <li key={`${index}:${rule}`}>{rule}</li>
              ))}
            </ol>
          </details>
        )}

        <RawEnvelope raw={context.raw} />
      </div>
    </details>
  );
}
