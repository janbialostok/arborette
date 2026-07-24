// Typed client for the orchestrator, called from the browser against the
// same-origin BFF proxy at /api/orchestrator/* (see app/api/orchestrator). The
// proxy forwards to ${ORCHESTRATOR_URL}; ORCHESTRATOR_URL is never exposed to
// the client bundle.

export const API_BASE = "/api/orchestrator";

// SSE frame envelope. The orchestrator emits `data: <json>\n\n` with no
// `event:` line, so clients discriminate on this top-level `type`, never on the
// SSE event type. `payload` is omitted on the terminal loop_complete frame. A
// frame with an unrecognized `type` still parses into this union and is ignored
// by the consumer's switch — no catch-all member is needed, and omitting it
// keeps the discriminated union narrowable without casts.
export type OrchestratorEvent =
  | { type: "triplet"; payload: TripletPayload }
  | { type: "branch_failure"; payload: BranchFailurePayload }
  | { type: "loop_complete"; payload?: undefined };

export interface TripletPayload {
  state_id: string;
  intervention_id: string;
  outcome_id: string;
  baseline: number;
  value: number;
  effect_size: number;
  // The objective this triplet measured, its optimization direction, and the
  // segment's effective/added filters rendered as predicate chips. `direction`
  // decides which sign of effect_size is an improvement; `filters` is the segment
  // the triplet measured, `new_filters` the filter this candidate added.
  objective_label: string;
  direction: "maximize" | "minimize";
  filters: string[];
  new_filters: string[];
}

export interface BranchFailurePayload {
  error: string;
}

export interface SubmitGoalResponse {
  optimization_function_id: string;
}

export interface HeuristicMatch {
  id: string;
  definition: string;
}

// A registered objective plus its latest run status, as projected by GET /goals.
// `status` is one of the run lifecycle states or a synthetic "no run"; the
// backend omits `failure_reason` unless the latest run failed with a reason.
export interface GoalListItem {
  optimization_function_id: string;
  goal_text: string;
  created_at: string;
  status: string;
  failure_reason?: string;
}

// The trace DTO is richer than the live triplet payload: it carries full node
// properties, whereas the SSE frame carries only ids and numeric baseline/value.
export interface TraceTriplet {
  state: { id: string; properties: Record<string, unknown> };
  intervention: {
    id: string;
    type: string;
    properties: Record<string, unknown>;
  };
  outcome: {
    id: string;
    verification_status: string;
    value: Record<string, unknown>;
  };
}

// OrchestratorError carries a message the orchestrator returned in its {error}
// body. That message is analyst-safe to surface verbatim; transport failures are
// masked into a generic message by the proxy and by the helpers below.
export class OrchestratorError extends Error {
  readonly status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = "OrchestratorError";
    this.status = status;
  }
}

async function errorFrom(res: Response): Promise<OrchestratorError> {
  let message = `request failed (${res.status})`;
  try {
    const body = (await res.json()) as { error?: string };
    if (body && typeof body.error === "string" && body.error) {
      message = body.error;
    }
  } catch {
    // Non-JSON body — keep the generic message.
  }
  return new OrchestratorError(message, res.status);
}

// errorMessage centralizes the surface-vs-mask policy for UI call sites: an
// OrchestratorError's message is the orchestrator's analyst-safe {error} body
// and is surfaced verbatim; anything else (a transport/parse failure) is masked
// behind the caller's generic fallback.
export function errorMessage(err: unknown, fallback: string): string {
  return err instanceof OrchestratorError ? err.message : fallback;
}

// requestJSON performs a same-origin BFF call, surfacing the orchestrator's
// {error} body verbatim on a non-2xx response and returning the parsed JSON.
async function requestJSON<T>(input: string, init?: RequestInit): Promise<T> {
  const res = await fetch(input, init);
  if (!res.ok) throw await errorFrom(res);
  return (await res.json()) as T;
}

export function submitGoal(form: FormData): Promise<SubmitGoalResponse> {
  return requestJSON<SubmitGoalResponse>(`${API_BASE}/goals`, {
    method: "POST",
    body: form,
  });
}

export function triggerHypothesisLoop(id: string): Promise<SubmitGoalResponse> {
  return requestJSON<SubmitGoalResponse>(
    `${API_BASE}/goals/${encodeURIComponent(id)}/hypothesis-loop`,
    { method: "POST" },
  );
}

export function triggerSleepCycle(id: string): Promise<SubmitGoalResponse> {
  return requestJSON<SubmitGoalResponse>(
    `${API_BASE}/goals/${encodeURIComponent(id)}/sleep-cycle`,
    { method: "POST" },
  );
}

export function listGoals(): Promise<GoalListItem[]> {
  return requestJSON<GoalListItem[]>(`${API_BASE}/goals`);
}

export function searchHeuristics(
  q: string,
  k?: number,
): Promise<HeuristicMatch[]> {
  const params = new URLSearchParams({ q });
  if (k != null) params.set("k", String(k));
  return requestJSON<HeuristicMatch[]>(
    `${API_BASE}/heuristics/search?${params.toString()}`,
  );
}

export function traceHeuristic(id: string): Promise<TraceTriplet[]> {
  return requestJSON<TraceTriplet[]>(
    `${API_BASE}/heuristics/${encodeURIComponent(id)}/trace`,
  );
}

export function streamUrl(id: string): string {
  return `${API_BASE}/goals/${encodeURIComponent(id)}/stream`;
}
