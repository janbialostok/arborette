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
  | { type: "loop_complete"; payload?: undefined }
  | { type: "verification"; payload: VerificationTransition }
  | { type: "causal_verification_dispatched"; payload: { intervention_id: string } }
  | { type: "causal_graph_corrected"; payload: CorrectionResult };

// A Verifier transition, relayed by the orchestrator rather than emitted by it:
// the relay wraps the worker's own event whole, so the frame's top-level type is
// the constant "verification" and the transition name arrives one level down. A
// consumer switching on the outer type alone never sees the transition, which is
// why this payload carries its own `type`. It is typed open — a Verifier release can
// add a transition, and a frame this UI does not know must still parse.
export interface VerificationTransition {
  type: string;
  intervention_id?: string;
  status?: string;
  reason?: string;
}

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
  dataset_id?: string;
}

// The dataset lifecycle DTOs. They mirror the dataset endpoints exactly: the
// summary carries the derived usage in addition to the stored status, and the
// detail adds the dataset's objectives (children, ascending by created_at).
export interface DatasetSummary {
  id: string;
  name: string;
  description: string;
  status: "active" | "archived";
  usage: "empty" | "in_use";
  objective_count: number;
  created_at: string;
  updated_at: string;
  data_source_ref: string;
  // When the detail view was last opened, null (omitted from JSON) until the
  // first open. The inventory orders by it most-recently-first.
  last_accessed_at: string | null;
}

// One objective as the dataset detail and the non-empty delete 409 report it.
// `status` is the goal's latest run state or the synthetic "no run".
export interface ObjectiveSummary {
  optimization_function_id: string;
  goal_text: string;
  dataset_id: string;
  status: string;
  created_at: string;
}

export interface DatasetDetail extends DatasetSummary {
  objectives: ObjectiveSummary[];
}

// Metadata-only patch: datasource_ref is immutable and never accepted.
export interface DatasetPatch {
  name?: string;
  description?: string;
  status?: "active" | "archived";
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

// One column of the discovered causal graph. `kind` is how discovery treated it —
// categorical or numeric — which is what decided the independence test it took.
export interface CausalColumn {
  name: string;
  kind: string;
}

// One discovered edge, keyed on the canonical (col_a < col_b) pair with the
// orientation carried as `direction` rather than in the pair's order.
// `provenance` says who oriented it (statistical, llm_prior, analyst) and
// `status` whether the pair was actually tested — an untested pair (unknown,
// budget_capped) stays undirected on purpose, so effects that depend on it report
// as not identifiable rather than resting on an orientation nobody established.
export interface CausalGraphEdge {
  col_a: string;
  col_b: string;
  direction: EdgeDirection;
  provenance: string;
  confidence: number;
  status: string;
}

export type EdgeDirection = "a_to_b" | "b_to_a" | "undirected" | "unknown";

// What the discovery run covered. `excluded_columns` and `budget_truncated` are
// the coverage limits an analyst has to see to read the graph honestly: a column
// discovery never looked at cannot appear as a confounder.
export interface CausalGraphMeta {
  version: number;
  excluded_columns: string[];
  budget_truncated: boolean;
  test_count: number;
  discovered_at: string;
}

export interface CausalGraph {
  columns: CausalColumn[];
  edges: CausalGraphEdge[];
  meta: CausalGraphMeta;
}

// The statuses a causal verification reaches. causally_verified is the sole
// confirming one and is deliberately distinct from the human-review `confirmed` of
// an extraction outcome: one says the data supports a causal effect, the other that a
// person agreed with a value. Kept as a union so the copy and tone map keyed by it
// must cover every case; the DTO's own `status` stays a string because a later
// release can add one, which must render rather than crash.
export type CausalVerificationStatus =
  | "pending"
  | "causally_verified"
  | "confounded"
  | "not_identifiable"
  | "unsupported_objective"
  | "failed";

// One causal-verification record. The four numeric fields are null — never 0 —
// until the run reaches a terminal outcome, and the short-circuit outcomes leave
// some of them null forever, so every render of them is null-safe. `stale` marks
// a record a graph correction invalidated: it is being re-verified, and its
// numbers describe a model the analyst has already corrected.
export interface CausalVerification {
  id: string;
  intervention_id: string;
  objective_label: string;
  filters: string[];
  graph_version: number;
  status: string;
  naive_effect: number | null;
  adjusted_effect: number | null;
  adjustment_set: string[];
  refutation_score: number | null;
  confidence: number | null;
  stale: boolean;
  created_at: string;
  updated_at: string;
}

// The orientations a correction may assert. Narrower than EdgeDirection on
// purpose: "unknown" is a value the graph serves for a pair discovery never
// tested, and asserting it is a 400 — an analyst either states a direction or
// withdraws one.
export type CorrectionDirection = "a_to_b" | "b_to_a" | "undirected";

// An analyst's edit to the discovered graph. from/to read as cause→effect for
// flip and add and are ignored by delete; `direction` overrides that reading,
// which is the only way to return an edge to undirected.
export interface EdgeCorrection {
  op: "flip" | "delete" | "add";
  from: string;
  to: string;
  direction?: CorrectionDirection;
}

// What a correction changed: the columns it touched (in the graph's own spelling,
// which is what adjustment sets hold), the new graph version it is served as, and
// how many invalidated verifications were re-dispatched because of it.
export interface CorrectionResult {
  op: string;
  columns: string[];
  graph_version: number;
  redispatched: number;
}

export interface VerifyDispatch {
  optimization_function_id: string;
  intervention_id: string;
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

// AuthRedirectHandler is invoked when a session-secured call answers 401 and a
// handler is registered. The shell installs it to route the analyst to the
// login page on a dying session; tests substitute a spy. A null handler disables
// the redirect (the failure then surfaces as a normal OrchestratorError).
export type AuthRedirectHandler = () => void;

let authRedirectHandler: AuthRedirectHandler | null = null;

// registerAuthRedirect installs (or, with null, clears) the 401-redirect
// handler. It is a module-level seam so the data client can route a mid-session
// 401 to login without every view owning the navigation, and so vitest (no
// window.location) can substitute a spy. Exactly one handler is active.
export function registerAuthRedirect(handler: AuthRedirectHandler | null): void {
  authRedirectHandler = handler;
}

// AuthRedirected is the sentinel thrown after the registered handler runs. A
// caller's catch can distinguish "navigating away to login" from a genuine
// failure and must not render an error flash for a session that is dying.
export class AuthRedirected extends Error {
  constructor() {
    super("session expired, redirecting to login");
    this.name = "AuthRedirected";
  }
}

// RequestOptions tailors a single requestJSON/request call. redirectOn401=false
// opts a call out of the 401-redirect -- the sign-in calls use it so their 401
// (the orchestrator's generic bad-credentials verdict) surfaces inline on the
// login page instead of looping back to it. Every other call inherits the
// default: a 401 with a registered handler invokes it and throws AuthRedirected.
interface RequestOptions {
  redirectOn401?: boolean;
}

// raiseIfSessionGone runs the registered auth-redirect handler for a 401 on a
// session-secured call and throws AuthRedirected so no view paints an error
// flash while the navigation happens. With no handler (or the opt-out set) it
// returns and the caller's normal error path applies.
function raiseIfSessionGone(err: OrchestratorError, opts: RequestOptions): void {
  if (opts.redirectOn401 === false) return;
  if (err.status !== 401) return;
  if (!authRedirectHandler) return;
  authRedirectHandler();
  throw new AuthRedirected();
}

// requestJSON performs a same-origin BFF call, surfacing the orchestrator's
// {error} body verbatim on a non-2xx response and returning the parsed JSON.
async function requestJSON<T>(
  input: string,
  init?: RequestInit,
  opts: RequestOptions = {},
): Promise<T> {
  const res = await fetch(input, init);
  if (!res.ok) {
    const err = await errorFrom(res);
    raiseIfSessionGone(err, opts);
    throw err;
  }
  return (await res.json()) as T;
}

// request performs a BFF call whose response has no JSON body (a 204 delete).
async function request(
  input: string,
  init?: RequestInit,
  opts: RequestOptions = {},
): Promise<void> {
  const res = await fetch(input, init);
  if (!res.ok) {
    const err = await errorFrom(res);
    raiseIfSessionGone(err, opts);
    throw err;
  }
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

export function listGoals(datasetId?: string): Promise<GoalListItem[]> {
  const query = datasetId ? `?dataset_id=${encodeURIComponent(datasetId)}` : "";
  return requestJSON<GoalListItem[]>(`${API_BASE}/goals${query}`);
}

export function listDatasets(q?: string): Promise<DatasetSummary[]> {
  const query = q ? `?q=${encodeURIComponent(q)}` : "";
  return requestJSON<DatasetSummary[]>(`${API_BASE}/datasets${query}`);
}

export function getDataset(id: string): Promise<DatasetDetail> {
  return requestJSON<DatasetDetail>(
    `${API_BASE}/datasets/${encodeURIComponent(id)}`,
  );
}

export function createDataset(form: FormData): Promise<DatasetSummary> {
  return requestJSON<DatasetSummary>(`${API_BASE}/datasets`, {
    method: "POST",
    body: form,
  });
}

export function updateDataset(
  id: string,
  patch: DatasetPatch,
): Promise<DatasetSummary> {
  return requestJSON<DatasetSummary>(`${API_BASE}/datasets/${encodeURIComponent(id)}`, {
    method: "PATCH",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(patch),
  });
}

export function deleteDataset(id: string): Promise<void> {
  return request(`${API_BASE}/datasets/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export function deleteGoal(id: string): Promise<void> {
  return request(`${API_BASE}/goals/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export function deleteHeuristic(id: string): Promise<void> {
  return request(`${API_BASE}/heuristics/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
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

// getCausalGraph reads the graph discovery committed for a goal's data source. It
// rejects with a 404 OrchestratorError until a discovery has committed one, which
// is an empty state rather than a fault — no run has needed a graph yet.
export function getCausalGraph(id: string): Promise<CausalGraph> {
  return requestJSON<CausalGraph>(
    `${API_BASE}/goals/${encodeURIComponent(id)}/causal-graph`,
  );
}

// correctCausalEdge applies one analyst edit to the discovered graph. A 409 means
// another correction or a discovery sweep holds the graph, so the edit was not
// applied and the view it was made against may already be stale.
export function correctCausalEdge(
  id: string,
  correction: EdgeCorrection,
): Promise<CorrectionResult> {
  return requestJSON<CorrectionResult>(
    `${API_BASE}/goals/${encodeURIComponent(id)}/causal-graph/corrections`,
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(correction),
    },
  );
}

// verifyFinding asks for one observational finding to be verified causally. It
// answers 202 and nothing else: verification runs for minutes and reports through
// the goal's stream and the verification list.
export function verifyFinding(
  id: string,
  interventionID: string,
): Promise<VerifyDispatch> {
  return requestJSON<VerifyDispatch>(
    `${API_BASE}/goals/${encodeURIComponent(id)}/findings/${encodeURIComponent(interventionID)}/verify`,
    { method: "POST" },
  );
}

export function listCausalVerifications(
  id: string,
): Promise<CausalVerification[]> {
  return requestJSON<CausalVerification[]>(
    `${API_BASE}/goals/${encodeURIComponent(id)}/causal-verifications`,
  );
}

export function searchHeuristics(
  q: string,
  k?: number,
  goalId?: string,
): Promise<HeuristicMatch[]> {
  const params = new URLSearchParams({ q });
  if (k != null) params.set("k", String(k));
  // An empty goalId omits the param, which the orchestrator reads as a
  // cross-goal (whole-corpus) search; a set goalId narrows to one goal.
  if (goalId) params.set("goal_id", goalId);
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

// --- Accounts & session surface ---------------------------------------------

// AccountDto is the account shape the roster rows carry. It is the wire form of
// the orchestrator's user row: role is the single extensible value, and active
// reflects deactivation (a deactivated account rejects its sessions server-side
// at their next request).
export interface AccountDto {
  id: string;
  username: string;
  role: "admin" | "member";
  active: boolean;
  created_at: string;
  updated_at: string;
}

// MeDto is the identity bootstrap every signed-in page and the corner chip use:
// the username's first letter for the chip, the role for the admin Users entry,
// and admin_notice, the one-shot first-sign-in admin-grant notice that shows
// until the admin dismisses it via PATCH /me.
export interface MeDto extends AccountDto {
  admin_notice: boolean;
}

// registerUser creates an account and signs it in: the response carries the
// Set-Cookie the proxy relays as an HttpOnly session cookie. The username/password
// validation rules live server-side, so the caller just forwards what the user
// typed and surfaces the {error} verbatim on failure. It opts out of the
// 401-redirect: its 401 is the orchestrator's generic bad-credentials verdict
// and must keep surfacing inline on the login page, never looping back to it.
export function registerUser(username: string, password: string): Promise<MeDto> {
  return requestJSON<MeDto>(
    `${API_BASE}/register`,
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ username, password }),
    },
    { redirectOn401: false },
  );
}

// login verifies credentials and signs the user in the same way. A failure is
// the single generic 401 the orchestrator returns (unknown user, wrong
// password, or deactivated account are indistinguishable on purpose) and is
// exempt from the 401-redirect for the same reason as registerUser.
export function login(username: string, password: string): Promise<MeDto> {
  return requestJSON<MeDto>(
    `${API_BASE}/login`,
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ username, password }),
    },
    { redirectOn401: false },
  );
}

// logout ends the session server-side (row delete) and clears the cookie. The
// orchestrator answers 204, which request() treats as success.
export function logout(): Promise<void> {
  return request(`${API_BASE}/logout`, { method: "POST" });
}

// getMe is the identity bootstrap every signed-in page and the corner chip
// read. A 401 here (cookie gone, session dead, account deactivated) surfaces as
// an OrchestratorError the shell maps to a redirect to /login.
export function getMe(): Promise<MeDto> {
  return requestJSON<MeDto>(`${API_BASE}/me`);
}

// updateMe applies one or more self-service account changes and returns the
// refreshed identity. Username changes are unique-checked server-side (409);
// password changes require current_password (wrong -> 403). The chip re-reads
// /me after navigating, so the refreshed identity here is a courtesy, not the
// source of truth.
export function updateMe(
  patch: Partial<{
    username: string;
    password: string;
    current_password: string;
    acknowledge_admin_notice: boolean;
  }>,
): Promise<MeDto> {
  return requestJSON<MeDto>(`${API_BASE}/me`, {
    method: "PATCH",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(patch),
  });
}

// --- Admin account surface --------------------------------------------------

// UserAdminDto is the roster row shape the admin Users view renders. It is the
// wire form of the orchestrator's admin listing: role is the single extensible
// value, active reflects whether the account's next signed-in request works.
export type UserAdminDto = AccountDto;

// listUsers returns the full account roster to an admin. A member gets the 403
// the orchestrator returns; the view surfaces it verbatim.
export function listUsers(): Promise<UserAdminDto[]> {
  return requestJSON<UserAdminDto[]>(`${API_BASE}/users`);
}

// createUser creates a member account (always active, role=member) that can
// sign in immediately. Server-side validation reasons and the 409 clash surface
// verbatim through OrchestratorError.
export function createUser(username: string, password: string): Promise<UserAdminDto> {
  return requestJSON<UserAdminDto>(`${API_BASE}/users`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ username, password }),
  });
}

// updateUser applies an admin mutation to an account: promote/demote (role),
// deactivate/reactivate (active), or reset the password (new_password). The
// last-active-admin guard's 409 and the role gate's 403 surface verbatim.
export function updateUser(
  id: string,
  patch: Partial<{ role: "admin" | "member"; active: boolean; new_password: string }>,
): Promise<UserAdminDto> {
  return requestJSON<UserAdminDto>(`${API_BASE}/users/${encodeURIComponent(id)}`, {
    method: "PATCH",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(patch),
  });
}
