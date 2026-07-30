import type { ConfidenceDistribution } from "@/lib/orchestrator";
import { histogramColumns, type HistogramColumn } from "@/lib/histogram";
import { Panel, SectionLabel } from "@/components/ui";

// ConfidenceHistogram plots a run's extraction confidence over 0.0–1.0. One
// measure in one hue, so no legend. Counts other than the peak are reachable on
// hover and, for a reader who cannot hover, in the sr-only table this plot is
// the visual form of.
export function ConfidenceHistogram({
  distribution,
}: {
  distribution: ConfidenceDistribution;
}) {
  const { bins, total } = distribution;
  const columns = histogramColumns(bins);

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-baseline justify-between">
        <SectionLabel>Extraction confidence</SectionLabel>
        <span className="font-mono text-[11px] text-faint">
          <span className="tabular text-muted">{total}</span> outcome
          {total === 1 ? "" : "s"} counted
        </span>
      </div>

      <Panel className="px-5 pb-3 pt-5">
        <div className="relative h-24">
          <div className="flex h-full items-end gap-0.5" aria-hidden>
            {columns.map((column, i) => (
              <Column key={i} column={column} />
            ))}
          </div>
          {total === 0 && (
            <span className="absolute inset-0 flex items-center justify-center text-xs text-faint">
              No extractions counted yet.
            </span>
          )}
        </div>

        <div
          className="mt-2 flex justify-between border-t border-line pt-2 font-mono text-[10px] tabular text-faint"
          aria-hidden
        >
          <span>0.0</span>
          <span>0.5</span>
          <span>1.0</span>
        </div>

        <table className="sr-only">
          <caption>Extraction confidence distribution</caption>
          <thead>
            <tr>
              <th scope="col">Confidence</th>
              <th scope="col">Outcomes</th>
            </tr>
          </thead>
          <tbody>
            {columns.map((column, i) => (
              <tr key={i}>
                <th scope="row">{column.range}</th>
                <td>{column.count}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </Panel>
    </section>
  );
}

function Column({ column }: { column: HistogramColumn }) {
  const { count, pct, range, labeled } = column;
  return (
    <div
      className="relative h-full flex-1"
      title={`${range} · ${count} outcome${count === 1 ? "" : "s"}`}
    >
      {labeled && (
        <span
          className="absolute inset-x-0 text-center font-mono text-[10px] tabular text-muted"
          style={{ bottom: `calc(${pct}% + 6px)` }}
        >
          {count}
        </span>
      )}
      <div
        className="absolute inset-x-0 bottom-0 mx-auto max-w-[24px] rounded-t-[4px] bg-signal/75 motion-safe:transition-[height] motion-safe:duration-300 motion-safe:ease-out"
        style={{ height: `${pct}%` }}
      />
    </div>
  );
}
