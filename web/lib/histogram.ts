// The confidence plot's geometry, kept out of the component so the normalization
// the bars encode is testable.

// PLOT_SCALE leaves a band above the fullest column for its value label, so a
// direct label can never be clipped by the top of the plot.
const PLOT_SCALE = 88;

// MIN_VISIBLE_PCT keeps a bucket holding a single outcome from rendering as
// nothing next to a bucket holding hundreds.
const MIN_VISIBLE_PCT = 3;

export interface HistogramColumn {
  count: number;
  // Height as a percentage of the plot area.
  pct: number;
  // The confidence range this bucket spans, e.g. "0.3–0.4".
  range: string;
  // Whether this column carries the plot's one direct value label.
  labeled: boolean;
}

// histogramColumns normalizes the run's buckets against the fullest one. The
// bucket width is derived from how many buckets arrived rather than assumed, so
// a change to the orchestrator's bucket count relabels the axis instead of
// silently misreporting every range. Only the fullest bucket is labeled: a
// number over every column reads as noise.
export function histogramColumns(bins: number[]): HistogramColumn[] {
  const max = bins.reduce((m, n) => Math.max(m, n), 0);
  const peak = max > 0 ? bins.indexOf(max) : -1;
  const buckets = bins.length || 1;
  return bins.map((count, i) => {
    const share = max > 0 ? (count / max) * PLOT_SCALE : 0;
    return {
      count,
      pct: count > 0 ? Math.max(share, MIN_VISIBLE_PCT) : 0,
      range: `${(i / buckets).toFixed(1)}–${((i + 1) / buckets).toFixed(1)}`,
      labeled: i === peak,
    };
  });
}
