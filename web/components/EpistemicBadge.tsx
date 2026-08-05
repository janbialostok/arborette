import { CAUSAL_INFERRED_CAVEAT, type EpistemicSource } from "@/lib/causal";
import { Badge } from "@/components/ui";

// EpistemicBadge says what kind of evidence stands behind a finding, and carries
// the qualifier with it. A causal reading is never shown bare: the caveat is the
// difference between "the data support this given the model we discovered" and
// "we proved this", and dropping it is how an analyst comes to believe the second.
export function EpistemicBadge({
  source,
  caveat = true,
}: {
  source: EpistemicSource;
  caveat?: boolean;
}) {
  if (source !== "causal_inferred") {
    return (
      <Badge tone="neutral" title="a measured association, not an adjusted effect">
        observational
      </Badge>
    );
  }
  return (
    <span className="inline-flex flex-wrap items-baseline gap-x-2 gap-y-1">
      <Badge tone="positive" title={CAUSAL_INFERRED_CAVEAT}>
        causal
      </Badge>
      {caveat && (
        <span className="text-[11px] leading-tight text-faint">
          {CAUSAL_INFERRED_CAVEAT}
        </span>
      )}
    </span>
  );
}
