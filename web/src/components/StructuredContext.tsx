import type { ReactNode } from "react";

export function StructuredContextRow({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <div className="grid min-w-0 gap-2 py-2.5 sm:grid-cols-[112px_1fr] sm:gap-3">
      <span className="font-mono text-[9px] font-semibold uppercase text-muted-foreground">
        {label}
      </span>
      <div className="min-w-0 break-words text-[11.5px] leading-5 text-foreground/75">
        {children}
      </div>
    </div>
  );
}

export function RawEnvelope({ raw }: { raw: string }) {
  return (
    <details className="mt-2 text-[10px] text-muted-foreground">
      <summary className="cursor-pointer select-none font-mono hover:text-foreground">raw envelope</summary>
      <pre className="mt-2 max-h-80 overflow-auto whitespace-pre-wrap break-words border-t border-border/60 bg-muted/25 px-3 py-2 font-mono text-[10.5px] leading-5 text-foreground/75">
        {raw}
      </pre>
    </details>
  );
}
