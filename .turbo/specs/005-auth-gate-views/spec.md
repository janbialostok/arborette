# Feature Specification: Authentication Required for All Views

**Feature Branch**: `fix_authorization`

**Created**: 2026-08-14

**Status**: Draft

**Input**: User description: "There is a bug where when I am not authenticated, I am still able to browse the /datasets and /goals and I assume other views/routes as well, although I see an 'Unauthorized' flash box at the top right of the page, and where the datasets would be rendered. A user must be logged in to see anything other than the login page."

## Overview

The console's views (Datasets, Goals, Heuristics, Settings, Users, the root inventory, and their deep links) are analyst surfaces that depend on a signed-in session. Today an unauthenticated visitor can still reach them: the page frame renders with its header, navigation, and corner chip, the data areas come up empty, and the corner chip surfaces an "Unauthorized" error flash — while the sign-in screen is bypassed entirely. The visitor can browse the routes without being logged in, which defeats the login gate.

This feature makes authentication a hard requirement for every view. A visitor who is not signed in — no session at all, or a session that has expired or been revoked — is shown the login page and nothing else: no console chrome, no navigation, no empty data frames, and no error flashes. Only the login page itself (and the sign-in/account-creation actions it carries) is reachable without authentication. Signed-in analysts use the console exactly as they do today; nothing about the gate interferes with legitimate use.

## Users

- **Analyst** — the primary console user. Signs in to browse datasets, goals, heuristics, and their details, and expects that a signed-out visit routes to the login page rather than a broken shell.
- **Administrator** — an analyst with the admin role who additionally uses the Users roster; subject to the same gate.
- **Visitor** — an unauthenticated person who opens the console (possibly via a bookmarked deep link). Must see only the login page.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Only the login page for signed-out visitors (Priority: P1)

As a visitor who is not signed in, I want any URL I open to present the login page and nothing else, so I am never able to look at analyst surfaces without authentication.

**Why this priority**: This is the entire point of the request — being logged in must be a precondition for seeing anything other than the login page, and the current behavior demonstrably leaks the page frame and routes.

**Independent Test**: Can be fully tested by clearing the session and visiting every console route in turn (root, `/datasets`, `/goals`, `/heuristics`, `/settings`, `/users`, and a dataset detail) and confirming each lands on the login page with no header, navigation, corner chip, or page content rendered.

**Acceptance Scenarios**:

1. **Given** I am not signed in, **When** I open the root URL or any console route (`/datasets`, `/goals`, `/heuristics`, `/settings`, `/users`, or a deep link), **Then** I am shown the login page and only the login page — no console header, navigation, corner chip, or view content appears.
2. **Given** I am not signed in, **When** I open a view that would render analyst data, **Then** no data area appears at all (no empty frame where datasets would render) — the only thing on screen is the sign-in surface.
3. **Given** I am not signed in, **When** I request any console route, **Then** the URL I end up on is the login page, and re-navigating or refreshing does not reveal the view.

---

### User Story 2 - No error flashes for signed-out visitors (Priority: P1)

As a visitor, I want authentication failure to result in the login page, not a page decorated with an "Unauthorized" error, so the gate reads as one clean decision instead of a broken page with a warning.

**Why this priority**: The "Unauthorized" flash at the top right is the visible symptom of the bug; eliminating it is what makes the gate feel intentional rather than half-working.

**Independent Test**: Can be fully tested by confirming that, while signed out, no console route ever renders an authorization-error message in the corner chip or anywhere else — the only outcome is the login page.

**Acceptance Scenarios**:

1. **Given** I am not signed in, **When** a page load or any background read fails authorization, **Then** I am taken to the login page and never shown an "Unauthorized" (or equivalent) error flash on a console view.
2. **Given** I am not signed in, **When** the console attempts to load my identity or any data, **Then** the failure routes me to login instead of leaving an error visible on the page.

---

### User Story 3 - Expired or revoked sessions behave like signed out (Priority: P1)

As an analyst whose session has expired or whose account has been deactivated, I want to be treated as signed out and returned to the login page on my next navigation or action, so I am never left staring at a dead page or a partial frame.

**Why this priority**: A present-but-stale session is exactly as unauthenticated as no session; failing to treat it that way is how the current page-frame leak survives.

**Independent Test**: Can be fully tested by starting a session, expiring or revoking it server-side, then navigating across console routes and confirming each bounces to the login page without rendering content.

**Acceptance Scenarios**:

1. **Given** my session has expired or been revoked, **When** I open a console route, **Then** I am sent to the login page rather than shown a page frame with empty data.
2. **Given** my session expires while I have the console open, **When** I perform the next action that reads data or navigates, **Then** I am returned to the login page rather than left on a page with a dead session.
3. **Given** my account has been deactivated, **When** I try to use the console, **Then** I am treated as signed out and sent to the login page.

---

### User Story 4 - Sign-in returns the analyst to where they were going (Priority: P2)

As a signed-out analyst who followed a bookmark or link to a specific view, I want to land on that view after signing in, so a deep link does not strand me on a generic screen.

**Why this priority**: Redirecting signed-out visitors to login is necessary (US1–3); remembering where they were headed and resuming there after sign-in keeps deep links useful instead of punishing. It refines the gate without expanding it.

**Independent Test**: Can be fully tested by opening a bookmarked deep link while signed out, signing in, and confirming the intended view opens.

**Acceptance Scenarios**:

1. **Given** I am signed out and open a deep link to a specific view (e.g. a dataset's detail), **When** I sign in, **Then** I land on that same view.
2. **Given** I sign in from the login page directly (no pending destination), **When** authentication succeeds, **Then** I land on the console's root view.

---

### User Story 5 - Signed-in analysts use the console unchanged (Priority: P2)

As a signed-in analyst, I want every view to work exactly as it does today, so the authentication gate fixes the leak without introducing regressions for legitimate users.

**Why this priority**: The gate must never false-bounce an authenticated user; preserving the signed-in experience is the guardrail that makes the fix safe to ship.

**Independent Test**: Can be fully tested by signing in and exercising the full console (browse datasets, open a dataset detail, goals, heuristics, settings, admin users) and confirming every view renders and every action succeeds as before.

**Acceptance Scenarios**:

1. **Given** I am signed in, **When** I navigate across `/datasets`, `/goals`, `/heuristics`, `/settings`, `/users`, and any detail view, **Then** each renders normally and the usual actions succeed.
2. **Given** I am signed in and actively using the console, **When** I refresh any view, **Then** I remain signed in and the view reloads correctly — no false bounce to the login page.
3. **Given** I sign out, **When** I confirm the sign-out, **Then** I land on the login page and cannot navigate back into a console view via browser history or the back button without signing in again.

---

### Edge Cases

- What about data requests made while signed out? They must keep returning a machine-readable authorization failure (not a redirect to the login page), so the client can recognize the dead session and route the analyst to login — while the analyst never sees the failure rendered on a console view.
- What about static assets and framework internals (`_next`, images, fonts)? They are not views; they must never be gated as if they were pages.
- What about the login page itself? It is the only route reachable without authentication, and its sign-in and account-creation actions must continue to work for signed-out visitors.
- What about a visitor with a session cookie that exists but is no longer valid? They must be treated as signed out (US3): sent to login, never shown a page frame with the corner chip's error flash.
- What about multiple browser tabs sharing a session? When the session dies, every tab's next navigation or action returns to the login page; no tab continues to render console views.
- What about browser history after sign-out? Back/forward must not restore a console view to a signed-out visitor; a refresh of any such view re-runs the gate and lands on login.
- What about an analyst whose destination no longer exists after sign-in (e.g. a deleted dataset)? They land on a valid default view instead of a dead route.
- What about a page already rendered before the session dies? It is not enough to leave it static; the next interactive action must be gated so the analyst is returned to login rather than using a dead session.

## Requirements *(mandatory)*

### Functional Requirements

**Hard gate on views**

- **FR-001**: System MUST require an active session for every view other than the login page; a visitor without a valid session MUST be served the login page and MUST NOT see console chrome (header, navigation, corner chip), page content, or empty data frames.
- **FR-002**: System MUST treat a present-but-stale, expired, or revoked session (including a deactivated account) as unauthenticated and route the visitor to the login page rather than rendering a partial page.
- **FR-003**: System MUST NOT render an "Unauthorized" or equivalent authorization-error flash to an unauthenticated visitor on any view; the only visible outcome of being unauthenticated MUST be the login page.
- **FR-004**: System MUST apply the gate to every console view: the root URL, `/datasets`, `/goals`, `/heuristics`, `/settings`, `/users`, and all deep links (e.g. a dataset's detail), so no route is browsable without an active session.

**Session lifecycle handling**

- **FR-005**: When an analyst's session expires or is revoked mid-use, System MUST return them to the login page on their next navigation or data action rather than leaving a dead page or error visible.
- **FR-006**: System MUST return machine-readable authorization failures (not login-page redirects) for data requests from a signed-out client, so the client can route the analyst to login without the analyst ever seeing the failure on a console view.
- **FR-007**: System MUST continue to serve the login page and its sign-in/account-creation actions to signed-out visitors; it is the single unauthenticated surface.

**Post-auth and sign-out behavior**

- **FR-008**: After a successful sign-in, System MUST land the analyst on the view they originally requested (deep-link recovery); when that destination no longer exists, System MUST land them on a valid default view.
- **FR-009**: Sign-out MUST return the analyst to the login page, and subsequent browser back/forward or refresh MUST NOT restore a console view to a signed-out visitor.
- **FR-010**: System MUST NOT introduce false sign-outs: an authenticated analyst with a valid session MUST be able to navigate, refresh, and act across all views without being bounced to the login page.

### Key Entities

- **Session** — The server-issued authenticated identity the console depends on. Its state (valid, absent, expired, revoked) decides whether a visitor sees the console at all; absence, expiry, or revocation all mean "not authenticated."
- **View** — Every console URL surface other than the login page (root, Datasets, Goals, Heuristics, Settings, Users, and detail/deep links). The gate's subject: none of them may render without a valid session.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of signed-out visitors who request any URL other than the login page are served the login page and nothing else; none see console chrome, navigation, corner chip, or view content.
- **SC-002**: 0 "Unauthorized" (or equivalent) error flashes are shown to signed-out visitors across the root URL, `/datasets`, `/goals`, `/heuristics`, `/settings`, `/users`, and dataset detail views.
- **SC-003**: 100% of stale, expired, or revoked sessions route to the login page within the next navigation or data action, with no partial console content rendered in between.
- **SC-004**: 100% of signed-in analysts reach every view normally — zero false bounces to the login page across the full console flow (browse, detail, goals, heuristics, settings, admin users, refresh).
- **SC-005**: Deep-link recovery works: an analyst who signs in after opening a bookmarked view lands on that view, with a valid default fallback when the destination no longer exists.
- **SC-006**: After sign-out, back/forward navigation and refresh never restore a console view to the signed-out visitor.

## Assumptions

- The security boundary remains server-side: the orchestrator's authorization failure is authoritative, and this feature makes the web client honor it by never exposing a view (or its frame) to an unauthenticated visitor and always routing them to the login page.
- The login page remains the single unauthenticated surface; there is no public read-only mode or anonymous browsing.
- The BFF/data layer continues to answer signed-out requests with machine-readable authorization failures (not login-page redirects) so the client drives the redirect; only page views redirect to the login page.
- A session that exists but is invalid (expired, revoked, account deactivated) is equivalent to no session for gating purposes; the login page is the outcome in both cases.
- Browser back/forward after sign-out is governed by normal page behavior; a refresh of any cached view re-runs the gate and lands on the login page.
- The sign-in and account-creation flows on the login page are unchanged; the first-account-becomes-admin rule and all existing credential rules continue to apply.
