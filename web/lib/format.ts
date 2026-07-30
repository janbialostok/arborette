// renderValue turns an orchestrator-supplied value of unknown shape into the one
// display form every view uses for graph values.
export function renderValue(val: unknown): string {
  if (val == null) return "—";
  if (typeof val === "object") return JSON.stringify(val);
  return String(val);
}
