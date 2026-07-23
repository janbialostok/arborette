import type { TripletPayload } from "@/lib/orchestrator";
import { cn } from "@/components/ui";

// EffectReadout renders one causal triplet as a diverging effect-size bar:
// baseline at the center axis, the bar extending right for a positive delta
// (green) or left for a negative one (coral), width normalized against the
// largest-magnitude effect currently in the feed. effect_size is a signed
// magnitude (value − baseline), not a judgement of good/bad — direction of
// "improvement" depends on the objective, which the live stream doesn't carry.
export function EffectReadout({
  triplet,
  scale,
  index,
}: {
  triplet: TripletPayload;
  scale: number;
  index: number;
}) {
  const { baseline, value, effect_size } = triplet;
  const positive = effect_size > 0;
  const negative = effect_size < 0;
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
            positive && "text-signal",
            negative && "text-neg",
            !positive && !negative && "text-muted",
          )}
        >
          {positive ? "+" : ""}
          {fmt(effect_size)}
        </span>
      </div>

      <div className="relative my-3 h-2 rounded-full bg-surface-2">
        <div className="absolute inset-y-0 left-1/2 w-px bg-line-strong" />
        <div
          className={cn(
            "animate-bar absolute inset-y-0 rounded-full",
            positive ? "bg-signal" : "bg-neg",
          )}
          style={
            positive
              ? { left: "50%", width: `${pct}%` }
              : { right: "50%", width: `${pct}%` }
          }
        />
      </div>

      <div className="flex items-center justify-between font-mono text-xs tabular">
        <span className="text-faint">
          base <span className="text-muted">{fmt(baseline)}</span>
        </span>
        <span className="text-faint" aria-hidden>
          →
        </span>
        <span className="text-faint">
          value <span className="text-fg">{fmt(value)}</span>
        </span>
      </div>
    </div>
  );
}

function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

function fmt(n: number): string {
  if (!Number.isFinite(n)) return "—";
  if (Number.isInteger(n)) return n.toLocaleString("en-US");
  const abs = Math.abs(n);
  const digits = abs >= 100 ? 1 : abs >= 1 ? 2 : 4;
  return n.toLocaleString("en-US", { maximumFractionDigits: digits });
}
