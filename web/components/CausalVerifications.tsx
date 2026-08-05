import {
  REVERIFYING_LABEL,
  SUFFICIENCY_CAVEAT,
  latestByIntervention,
  outcomeCopy,
  verificationView,
  type VerificationView,
} from "@/lib/causal";
import { formatConfidence, formatNumber, shortId } from "@/lib/format";
import type { CausalVerification } from "@/lib/orchestrator";
import { EpistemicBadge } from "@/components/EpistemicBadge";
import { Badge, cn, Panel, SectionLabel } from "@/components/ui";

// CausalVerifications is the answer side of the causal surface: what happened when
// a finding was checked against the discovered model. The naive and adjusted
// effects sit side by side because the gap between them is the finding — a segment
// whose effect survives adjustment is worth acting on, one whose effect collapses
// was never about the segment.
export function CausalVerifications({
  verifications,
}: {
  verifications: CausalVerification[];
}) {
  const latest = latestByIntervention(verifications);
  return (
    <section className="flex flex-col gap-3">
      <SectionLabel>
        Causal verifications · {verifications.length} · newest first
      </SectionLabel>

      {verifications.length === 0 ? (
        <Panel className="px-6 py-12 text-center text-sm leading-relaxed text-faint">
          Nothing has been verified causally yet. Use <em>Verify causally</em> on a
          finding — on the run feed or in the heuristic browser — to adjust its
          effect for the confounders this model names.
        </Panel>
      ) : (
        <ul className="flex flex-col gap-3">
          {verifications.map((record) => (
            <li key={record.id}>
              <VerificationCard record={record} view={verificationView(record, latest)} />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function VerificationCard({
  record,
  view,
}: {
  record: CausalVerification;
  view: VerificationView;
}) {
  const copy = outcomeCopy(record.status);
  const { superseded, reverifying, source } = view;

  return (
    <Panel className="flex flex-col gap-4 p-5">
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-[11px] uppercase tracking-wider text-faint">
            finding · {shortId(record.intervention_id)}
          </span>
          <Badge tone={copy.tone}>{record.status || "unknown"}</Badge>
          {reverifying && <Badge tone="warn">{REVERIFYING_LABEL}</Badge>}
          {superseded && <Badge tone="neutral">superseded</Badge>}
        </div>
        <span className="font-mono text-[11px] text-faint">
          model version <span className="tabular text-muted">{record.graph_version}</span>
        </span>
      </div>

      <div className="flex flex-col gap-1.5">
        <p className="text-base leading-snug text-fg">{copy.headline}</p>
        <p className="max-w-2xl text-sm leading-relaxed text-muted">{copy.detail}</p>
        {reverifying && (
          <p className="text-sm leading-relaxed text-warn">
            A correction to the model invalidated this result — it is being
            re-verified against the corrected graph.
          </p>
        )}
      </div>

      <div className="grid gap-x-6 gap-y-4 border-t border-line pt-4 sm:grid-cols-2">
        <Effect
          label="Unadjusted effect"
          value={record.naive_effect}
          caption="what the segment looks like before confounders are accounted for"
        />
        {/* The adjusted number is the answer only where the verdict says it
            survived; on a confounded or superseded record the accent would read as
            an endorsement of a number nobody should act on. */}
        <Effect
          label="Adjusted effect"
          value={record.adjusted_effect}
          caption="after blocking the back-door paths the model names"
          emphasis={source === "causal_inferred"}
        />
      </div>

      <div className="flex flex-col gap-3 border-t border-line pt-4">
        <div className="flex flex-col gap-1.5">
          <SectionLabel>Adjusted for</SectionLabel>
          {record.adjustment_set.length === 0 ? (
            <span className="text-xs text-faint">
              no adjustment needed — the model names no confounder of this effect
            </span>
          ) : (
            <div className="flex flex-wrap gap-1">
              {record.adjustment_set.map((column) => (
                <span
                  key={column}
                  className="rounded border border-line bg-surface-2 px-1.5 py-0.5 font-mono text-[10px] text-muted"
                >
                  {column}
                </span>
              ))}
            </div>
          )}
        </div>

        <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
          <span className="font-mono text-[11px] text-faint">
            refutation{" "}
            <span className="tabular text-muted">
              {formatConfidence(record.refutation_score)}
            </span>
            {record.confidence != null && (
              <>
                {" · confidence "}
                <span className="tabular text-muted">
                  {formatConfidence(record.confidence)}
                </span>
              </>
            )}
          </span>
          {/* The caveat rides the footer sentence below in full, so the badge
              carries it on hover rather than saying it twice in one card. */}
          <EpistemicBadge source={source} caveat={false} />
        </div>
      </div>

      {source === "causal_inferred" && (
        <p className="border-t border-line pt-3 text-xs leading-relaxed text-faint">
          Read as causal only {SUFFICIENCY_CAVEAT}.
        </p>
      )}
    </Panel>
  );
}

function Effect({
  label,
  value,
  caption,
  emphasis,
}: {
  label: string;
  value: number | null;
  caption: string;
  emphasis?: boolean;
}) {
  return (
    <div className="flex flex-col gap-1">
      <SectionLabel>{label}</SectionLabel>
      <span
        className={cn(
          "font-mono text-2xl font-semibold tabular",
          // An absent value is never emphasized: a green em dash reads as a
          // result, and the whole point of the dash is that there isn't one.
          emphasis && value != null ? "text-signal" : "text-muted",
        )}
        title={value == null ? "not measured" : undefined}
      >
        {formatNumber(value)}
      </span>
      <span className="text-[11px] leading-relaxed text-faint">{caption}</span>
    </div>
  );
}

