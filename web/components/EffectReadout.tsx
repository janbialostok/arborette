import { formatNumber, shortId } from "@/lib/format";
import type { QueryTripletPayload } from "@/lib/orchestrator";
import { VerifyFindingButton } from "@/components/VerifyFindingButton";
import { cn } from "@/components/ui";

// EffectReadout renders one causal triplet as a diverging effect-size bar:
// baseline at the center axis, the bar extending right for a positive delta or
// left for a negative one, width normalized against the largest-magnitude effect
// currently in the feed. effect_size is a signed magnitude (value − baseline); the
// stream now carries the objective `direction`, so the bar and number are colored
// by whether the delta is an improvement (green) or a regression (coral) — for a
// minimize objective a negative delta is the improvement. The segment's filters
// render as predicate chips so the card shows which segment was measured, not just
// the delta. The delta itself is a measured association — verifying it is what
// decides whether the segment caused it or merely moved with it — so each card
// carries the affordance to ask.
export function EffectReadout({
  goalID,
  triplet,
  scale,
  index,
}: {
  goalID: string;
  triplet: QueryTripletPayload;
  scale: number;
  index: number;
}) {
  const { baseline, value, effect_size, direction, filters } = triplet;
  const improved =
    direction === "minimize" ? effect_size < 0 : effect_size > 0;
  const worsened =
    direction === "minimize" ? effect_size > 0 : effect_size < 0;
  const rightward = effect_size > 0;
  const pct = scale > 0 ? Math.min(Math.abs(effect_size) / scale, 1) * 50 : 0;

  return (
    <div
      className="animate-rise rounded-xl border border-line bg-surface/70 p-4 transition-colors hover:border-line-strong"
      style={{ animationDelay: `${Math.min(index, 6) * 40}ms` }}
    >
      <div className="flex items-center justify-between">
        <span className="font-mono text-[11px] uppercase tracking-wider text-faint">
          triplet · {shortId(triplet.intervention_id)}
        </span>
        <span
          className={cn(
            "font-mono text-lg font-semibold tabular",
            improved && "text-signal",
            worsened && "text-neg",
            !improved && !worsened && "text-muted",
          )}
        >
          {effect_size > 0 ? "+" : ""}
          {formatNumber(effect_size)}
        </span>
      </div>

      {filters.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-1">
          {filters.map((f, i) => (
            <span
              key={i}
              className="rounded border border-line bg-surface-2 px-1.5 py-0.5 font-mono text-[10px] text-muted"
            >
              {f}
            </span>
          ))}
        </div>
      )}

      <div className="relative my-3 h-2 rounded-full bg-surface-2">
        <div className="absolute inset-y-0 left-1/2 w-px bg-line-strong" />
        <div
          className={cn(
            "animate-bar absolute inset-y-0 rounded-full",
            improved ? "bg-signal" : "bg-neg",
          )}
          style={
            rightward
              ? { left: "50%", width: `${pct}%` }
              : { right: "50%", width: `${pct}%` }
          }
        />
      </div>

      <div className="flex items-center justify-between font-mono text-xs tabular">
        <span className="text-faint">
          base <span className="text-muted">{formatNumber(baseline)}</span>
        </span>
        <span className="text-faint" aria-hidden>
          →
        </span>
        <span className="text-faint">
          value <span className="text-fg">{formatNumber(value)}</span>
        </span>
      </div>

      <div className="mt-3 border-t border-line pt-3">
        <VerifyFindingButton
          goalID={goalID}
          interventionID={triplet.intervention_id}
        />
      </div>
    </div>
  );
}
