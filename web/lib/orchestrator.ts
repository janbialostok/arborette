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
  | { type: "confidence_distribution"; payload: ConfidenceDistribution }
  | { type: "loop_complete"; payload?: undefined };

// A run publishes one of two triplet shapes under the same frame type, decided
// by what the goal reads: a tabular goal measures a segment and reports an
// effect, while a document goal extracts a field and reports what it found and
// how sure it was. A run's stream is homogeneous, so the two never interleave —
// but a client that assumed the measuring shape would break the moment it met an
// extraction, which is why this is a union rather than one widened interface.
export type TripletPayload = QueryTripletPayload | ExtractionTripletPayload;

interface TripletIdentity {
  state_id: string;
  intervention_id: string;
  outcome_id: string;
}

export interface QueryTripletPayload extends TripletIdentity {
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

// One field pulled out of a source document: the value read, the approach that
// found it, the model's confidence, and where in the document it sits — null
// when the value could not be located verbatim, which is what sends a review to
// the whole-document excerpt.
export interface ExtractionTripletPayload extends TripletIdentity {
  field: string;
  method: string;
  value: string;
  confidence: number;
  provenance: ProvenanceLocator | null;
}

// isExtraction discriminates the two shapes on the field only an extraction
// carries. Testing for a present field rather than an absent one keeps a future
// addition to the measuring shape from silently reclassifying it.
export function isExtraction(
  payload: TripletPayload,
): payload is ExtractionTripletPayload {
  return typeof (payload as ExtractionTripletPayload).field === "string";
}

export interface BranchFailurePayload {
  error: string;
}

// A snapshot — never a delta — of the run's extraction-confidence distribution:
// ten equal-width buckets spanning 0.0–1.0, and how many outcomes the run has
// counted. A resolution moves an outcome between buckets, so a later frame can
// report a lower count in a bucket than the one before it.
export interface ConfidenceDistribution {
  bins: number[];
  total: number;
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

// Where in the source document an extracted value was found; null when the
// extraction produced no exact match.
export interface ProvenanceLocator {
  page: number;
  char_start: number;
  char_end: number;
}

// One row of the review queue. `status` is pending or resolved; `resolution`
// and `corrected_value` are present only once it is resolved.
export interface VerificationEntry {
  queue_id: string;
  outcome_id: string;
  field: string;
  extracted_value: string;
  provenance: ProvenanceLocator | null;
  confidence: number;
  status: string;
  resolution?: string;
  corrected_value?: string;
  created_at: string;
  resolved_at?: string;
}

// The queue plus the settings that produced it, so the view can say which
// threshold sent these extractions to review without a second call.
export interface VerificationList {
  effective_threshold: number;
  epoch_mode: string;
  entries: VerificationEntry[];
}

// An extraction as the graph holds it — every extract-type outcome, not only
// the queued ones. Structurally distinct from a queue entry: the value is a map
// keyed by field rather than a flat string, and the status is the graph's
// verification status rather than the queue's.
export interface ExtractionOutcome {
  outcome_id: string;
  field: string;
  method: string;
  value: Record<string, unknown>;
  provenance: ProvenanceLocator | null;
  verification_status: string;
  confidence: number;
}

// The source text behind an extracted value. `fallback` marks the whole-document
// shape, where only `pages` is populated; otherwise `excerpt` is the located span
// and `page_text` the page it sits in, delimited by char_start/char_end.
export interface OutcomeExcerpt {
  fallback: boolean;
  page: number;
  char_start: number;
  char_end: number;
  excerpt?: string;
  page_text?: string;
  pages?: string[];
}

export type ResolveAction = "confirm" | "correct" | "reject";

export interface ResolveResult {
  outcome_id: string;
  resolution: string;
  verification_status: string;
  confidence: number;
}

// One item of a streamed chat turn. Kept out of OrchestratorEvent because these
// frames only ever arrive on a chat response, never on a run stream — the two
// unions share the wire format and nothing else. A tool's name arrives on
// chat_tool_use alone, but both tool frames carry `tool_id` — the API's own
// identifier for the call — which is what pairs a result with the call it
// answers.
export type ChatFrame =
  | { type: "chat_text"; text?: string }
  | { type: "chat_tool_use"; tool?: string; tool_id?: string }
  | { type: "chat_tool_result"; tool_id?: string; is_error?: boolean }
  | { type: "chat_done" }
  | { type: "chat_error"; message?: string };

export interface ChatMessage {
  role: "user" | "assistant";
  content: string;
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

// errorFrom builds the OrchestratorError for a non-2xx response. Exported for
// streaming callers that own their own fetch: a streamed endpoint commits its
// 200 only on the first frame, so a fault before then arrives as a JSON error
// the analyst should see.
export async function errorFrom(res: Response): Promise<OrchestratorError> {
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

// listVerifications reads a goal's review queue. `status` narrows it to the rows
// still awaiting a verdict or to those already resolved; omitting it returns the
// queue's whole history, which reads as a work list only until the first
// resolution lands in it.
export function listVerifications(
  id: string,
  status?: "pending" | "resolved",
): Promise<VerificationList> {
  const query = status ? `?status=${status}` : "";
  return requestJSON<VerificationList>(
    `${API_BASE}/goals/${encodeURIComponent(id)}/verifications${query}`,
  );
}

export function listOutcomes(id: string): Promise<ExtractionOutcome[]> {
  return requestJSON<ExtractionOutcome[]>(
    `${API_BASE}/goals/${encodeURIComponent(id)}/outcomes`,
  );
}

export function getOutcomeExcerpt(
  id: string,
  outcomeID: string,
): Promise<OutcomeExcerpt> {
  return requestJSON<OutcomeExcerpt>(
    `${API_BASE}/goals/${encodeURIComponent(id)}/outcomes/${encodeURIComponent(outcomeID)}/excerpt`,
  );
}

// resolveVerification applies an analyst's verdict. correctedValue is required
// by the orchestrator for a "correct" and ignored for the other two verdicts.
export function resolveVerification(
  id: string,
  outcomeID: string,
  action: ResolveAction,
  correctedValue = "",
): Promise<ResolveResult> {
  return requestJSON<ResolveResult>(
    `${API_BASE}/goals/${encodeURIComponent(id)}/verifications/${encodeURIComponent(outcomeID)}`,
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ action, corrected_value: correctedValue }),
    },
  );
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

// chatUrl is posted to, not fetched as JSON: the reply streams, so its caller
// owns the fetch and consumes the body with consumeStream.
export function chatUrl(id: string): string {
  return `${API_BASE}/goals/${encodeURIComponent(id)}/chat`;
}
