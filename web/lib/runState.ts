// Client-side per-goal run lifecycle, persisted in localStorage. The
// orchestrator has no endpoint to distinguish "run finished" from "never
// triggered": a fresh /stream for either id replays nothing and then blocks
// silently forever. This record lets the live view tell the two apart —
// `triggered` is set when the client's hypothesis-loop POST returns 202,
// `completed` on the first loop_complete — so a silent stream resolves to the
// neutral "run finished or not yet started" state instead of a perpetual
// spinner, while a freshly-triggered (slow-starting) run keeps waiting.

const PREFIX = "arborette:run:";

export interface RunRecord {
  triggered: boolean;
  completed: boolean;
}

function key(id: string): string {
  return `${PREFIX}${id}`;
}

export function getRunRecord(id: string): RunRecord {
  if (typeof window === "undefined") return { triggered: false, completed: false };
  try {
    const raw = window.localStorage.getItem(key(id));
    if (!raw) return { triggered: false, completed: false };
    const parsed = JSON.parse(raw) as Partial<RunRecord>;
    return {
      triggered: Boolean(parsed.triggered),
      completed: Boolean(parsed.completed),
    };
  } catch {
    return { triggered: false, completed: false };
  }
}

function write(id: string, record: RunRecord): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(key(id), JSON.stringify(record));
  } catch {
    // localStorage unavailable (private mode / quota) — the view degrades to
    // its idle-timeout fallback, so this is non-fatal.
  }
}

// markTriggered records that a run was launched for this id. A fresh trigger
// clears any prior completed flag so a re-run is treated as in-progress again.
export function markTriggered(id: string, completed = false): void {
  write(id, { triggered: true, completed });
}

export function markCompleted(id: string): void {
  const current = getRunRecord(id);
  write(id, { triggered: current.triggered, completed: true });
}
