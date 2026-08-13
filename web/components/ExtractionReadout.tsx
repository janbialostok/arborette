import { formatConfidence } from "@/lib/format";
import type { ExtractionTripletPayload } from "@/lib/orchestrator";
import { cn } from "@/components/ui";

// LOW_CONFIDENCE tints an extraction the service's default review threshold
// would queue. It is a fixed reading cue, not the queue's own bar: a dataset may
// override that bar, and the live stream carries no threshold to read it from,
// so the Verify tab — which is given the effective value — stays the authority
// on what was actually queued.
const LOW_CONFIDENCE = 0.8;

// ExtractionReadout renders one field pulled out of a source document. It is the
// document run's counterpart to EffectReadout: an extraction has no baseline to
// diverge from, so what matters is the value read, how sure the model was, and
// whether the value could be traced back to a span of the document — an
// untraceable value is the one a reviewer has to read the whole page to judge.
export function ExtractionReadout({
  triplet,
  index,
}: {
  triplet: ExtractionTripletPayload;
  index: number;
}) {
  const { field, value, method, confidence, provenance } = triplet;
  const low = confidence < LOW_CONFIDENCE;

  return (
    <div
      className="animate-rise rounded-xl border border-line bg-surface/70 p-4 transition-colors hover:border-line-strong"
      style={{ animationDelay: `${Math.min(index, 6) * 40}ms` }}
    >
      <div className="flex items-center justify-between gap-3">
        <span className="truncate font-mono text-[11px] uppercase tracking-wider text-faint">
          {field}
        </span>
        <span
          className={cn(
            "font-mono text-sm font-semibold tabular",
            low ? "text-warn" : "text-signal",
          )}
        >
          {formatConfidence(confidence)}
        </span>
      </div>

      <p className="mt-2 break-words text-sm leading-relaxed text-fg">
        {value || "—"}
      </p>

      <div className="mt-3 flex items-center justify-between gap-3 font-mono text-[10px] text-faint">
        <span className="truncate" title={method}>
          {method}
        </span>
        <span className={cn("shrink-0", provenance ? "text-muted" : "text-warn")}>
          {provenance ? `page ${provenance.page + 1}` : "not located"}
        </span>
      </div>
    </div>
  );
}
