# Feature Specification: User Accounts, Sign-In & Profile

**Feature Branch**: `003-user-management`

**Created**: 2026-08-13

**Status**: Draft

**Input**: User description: "Add a simple user management system, just allow the typical username and password lifecycle flow, no need to confirm via email, keep it simple for now. Also add the concept of admins and possibly other groups later. Remove the 'objectives' and 'Heuristics' links from the header. Put the first letter of their username up in the corner as a clickable icon as Google Docs does to indicate who's looking at a doc to alter their user settings, just username/password management for now. Also, have the logout link under this icon. Follow the existing conventions for specs in the .turbo directory. DO NOT COMMIT TO GIT UNTIL TOLD TO DO SO."

## Overview

The console is today a single-operator tool with no notion of a person: every action is stamped with the same stub identity, there are no accounts or sign-in, and the header offers **Objectives**, **Datasets**, and **Heuristics** navigation on every page. This feature introduces the first real identity layer: a simple username-and-password account system — sign-up, sign-in, sign-out, and self-service username/password management — no email, no confirmation, deliberately uncomplicated.

Every signed-in user gets a Google-Docs-style identity chip in the top corner showing the first letter of their username. Clicking it reveals who is using the console and opens a small menu for account settings and sign-out, so a person can always see who they are acting as and change their own username or password.

The header is simplified at the same time: the **Objectives** and **Heuristics** links are removed, and the brand mark and **Datasets** link remain — the data inventory is the console's home, and the objectives that live under each dataset are reached from there, as in the current home-page flow.

Accounts carry a role, beginning with two values — **admin** and **member** — so an administrator can manage who has an account and reset credentials. The role is a single extensible concept so other groups can be added later without restructuring.

Because the console becomes signed-in, the stub identity stamped on activity is replaced by the real signed-in user, making every action attributable to the person who took it.

## Clarifications

### Design decisions (2026-08-13)

Resolved during specification; each default is recorded in Assumptions so planning can revisit.

- Q: Must a signed-out visitor be able to use the console, or is sign-in required? → A: Sign-in is required to use the console; a signed-out visitor sees a sign-in/registration screen, not app content. This is what makes the identity chip meaningful ("who's looking") and keeps the account model simple.
- Q: Who can create accounts, and how does the first admin come to exist? → A: Self-registration is open — anyone can create an account with a username and password. An account is seeded as admin whenever no active admin exists (a fresh system, a store reset, or after the sole admin is removed); a registration that happens while an admin is active creates a member. Admins can also create accounts directly.
- Q: Without email, how is a forgotten password recovered? → A: There is no self-service recovery in this version (there is no email address to verify). An admin resets the password to a temporary value the user chooses to change at next sign-in, or confirms the new value on the user's behalf.
- Q: Do the new accounts alter who sees what data? → A: No. The console remains a single shared workspace — all signed-in members see the same datasets, goals, and heuristics. Roles govern account administration in this version, not data tenancy (data scoping is a known backlog item, out of scope here).

### Session 2026-08-13

- Q: How should the first-user admin grant be surfaced in the initial setup flow, given the UX must be "elegant and quick"? → A: The grant is automatic — no extra setup step — and the first sign-in shows a lightweight, dismissible one-line acknowledgment ("You're the first user; this account is admin — you can grant admin access to others from the account list"). The user is informed of their rights and of delegation without slowing setup.
- Q: When does the automatic admin seed trigger — only the first account in a fresh system, or whenever an admin is missing? → A: Whenever no active admin exists. The first registrant on a fresh system still becomes admin (unchanged happy path), and the same rule self-heals after a store reset or removal of the sole admin: the next registrant is seeded as admin.

## Users

- **Analyst (primary)** — sign into a single shared workspace carrying a username and password; reaches every current view (datasets, and their objectives) through the Datasets link; manages their own username and password from the corner chip; identifiable in the corner by their initial.
- **Administrator** — an analyst who additionally manages accounts: views the account list, creates accounts, resets forgotten passwords, and grants or revokes the admin role. Initiated by the first account in a fresh system; there is always at least one.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Create an account and sign in (Priority: P1)

As an analyst without an account, I want to create one with just a username and password and then sign in, so that I can identify myself to the console without any email step.

**Why this priority**: Accounts and sign-in are the foundation of the whole feature; nothing else — the corner chip, account settings, admin management — exists until sign-in works.

**Independent Test**: Can be fully tested on a fresh system by creating an account, signing out, signing back in with those credentials, and confirming the app loads and the corner shows the username's initial.

**Acceptance Scenarios**:

1. **Given** a console with no signed-in user, **When** I visit any address, **Then** I am shown the sign-in screen with a way to register, not the application content.
2. **Given** the registration form, **When** I submit a new username and password, **Then** an account is created, I am signed in, and the console opens with my initial in the corner.
3. **Given** the same console (first account ever), **When** I check my role, **Then** my account is an admin.
4. **Given** a username that is already taken, **When** I try to register it, **Then** I am told the username is unavailable and no account is created.
5. **Given** an invalid password (below the minimum length), **When** I submit registration, **Then** I am told the password is too short and no account is created.
6. **Given** my existing account, **When** I sign out and then sign in with the wrong password, **Then** sign-in fails and the message does not reveal whether the username existed.
7. **Given** the first account ever on a fresh system, **When** I complete registration and land on the console, **Then** I am signed in as admin and a lightweight, dismissible notice acknowledges the grant and points to delegating admin access from account administration.

---

### User Story 2 - See who I am and sign out from the corner chip (Priority: P1)

As a signed-in analyst, I want my username's first letter shown as an icon in the top corner, and a menu under it with sign-out, so I can always see which identity is acting and end my session cleanly.

**Why this priority**: The Google-Docs-style chip is the requested centerpiece of the UI and the only exit from the application; it pairs with US1 to make the account model real on screen.

**Independent Test**: Can be fully tested by signing in and, on any console page, confirming the corner chip shows the first letter of the signed-in username, that clicking it opens a menu, and that its sign-out action returns to the sign-in screen.

**Acceptance Scenarios**:

1. **Given** a signed-in user, **When** I look at any console page, **Then** the top-right corner shows a chip with the first letter of the signed-in username.
2. **Given** the corner chip, **When** I click it, **Then** a small menu opens with sign-out (and, per US3, account settings).
3. **Given** the sign-out action in the menu, **When** I choose it, **Then** my session ends and I am returned to the sign-in screen; the console no longer shows application content until I sign back in.

---

### User Story 3 - Manage my username and password (Priority: P2)

As a signed-in analyst, I want to change my username or my password from my account settings, reached via the corner chip, so that I control my own credential without help from anyone.

**Why this priority**: Self-service account management is the "user settings" the corner chip is explicitly for; it builds directly on the signed-in session from US1/US2.

**Independent Test**: Can be fully tested by opening the settings from the corner chip, changing the username (or the password with the current password confirmed), and confirming the change is immediate — the chip shows the new initial, and the new credentials sign in.

**Acceptance Scenarios**:

1. **Given** my account settings, **When** I change my username to an available name, **Then** the change takes effect immediately and the corner chip shows the new initial.
2. **Given** a username change, **When** I sign out and sign in again, **Then** only the new username works.
3. **Given** my account settings, **When** I change my password and confirm the current one, **Then** the change takes effect immediately and the current password no longer works while the new one does.
4. **Given** a password change attempt, **When** I enter the wrong current password, **Then** the change is refused.
5. **Given** that I forget my password, **When** I ask for recovery, **Then** there is no self-service reset; only an admin can set a new password for me (US4).

---

### User Story 4 - Administer accounts (Priority: P2)

As an administrator, I want to see the accounts, create one, reset a forgotten password, and grant or revoke admin access, so that the console's membership and credentials stay manageable without email.

**Why this priority**: The "concept of admins" is an explicit part of the request; it depends on roles existing (US1 seeds the first admin) and is what makes credential recovery possible without email.

**Independent Test**: Can be fully tested by signing in as an admin, opening the account list, creating a second account, resetting its password, granting and revoking admin, and confirming a member cannot reach any of these actions.

**Acceptance Scenarios**:

1. **Given** a signed-in admin, **When** I open the account administration view, **Then** I see every account with its username, role, and status.
2. **Given** the account list, **When** I create an account with a username and password, **Then** it appears in the list as a member and can sign in immediately.
3. **Given** an account with a forgotten password, **When** I reset that password, **Then** the new password works for that account's next sign-in.
4. **Given** a member account, **When** I promote it to admin, **Then** it gains admin access; when I demote an admin to member, **Then** it loses admin access.
5. **Given** a signed-in user who is not an admin, **When** they try to open the account administration view, **Then** they are refused, and the entry point is not offered to them.
6. **Given** the only admin's own account, **When** I try to demote the last remaining admin (including myself), **Then** the action is refused so the system always keeps at least one admin.

---

### User Story 5 - Simplify the header navigation (Priority: P2)

As an analyst, I want the **Objectives** and **Heuristics** links gone from the top header on every page so the navigation reflects what the corner chip now carries and what the home page already provides.

**Why this priority**: Explicitly requested and simple to verify, but it belongs after the identity chip lands so the corner is the designed anchor for the right edge of the header.

**Independent Test**: Can be fully tested by viewing any console page and confirming only the brand mark, the **Datasets** link, and the corner identity chip are present, and that the removed links are absent everywhere.

**Acceptance Scenarios**:

1. **Given** any signed-in console page, **When** I look at the top header, **Then** it shows the brand mark, the **Datasets** link, and the corner identity chip — and no **Objectives** or **Heuristics** links.
2. **Given** the **Datasets** link, **When** I open it, **Then** it reaches the dataset inventory, and from there a dataset's objectives are reachable exactly as they are today (per the home-page flow).
3. **Given** a formerly bookmarked `/goals` or `/heuristics` address, **When** I visit it, **Then** the system handles it gracefully — it does not dead-end the user, consistent with how the current landing-flow aliasing works.

---

### Edge Cases

- What if a brand-new system has no accounts at all? — The sign-in screen is shown with registration as the way forward; the very first account created becomes an admin (US1.3), because no active admin exists.
- What if the console's data is ever reset or the sole admin account is removed outside normal flows (e.g., a redeploy or store wipe)? — The next registrant is seeded as admin automatically, since no active admin exists (FR-003); the system self-heals to always having an admin rather than locking on a dead first-user rule.
- What if a user forgets their password (no email)? — Only an admin can reset it (US4.3); there is no self-service recovery in this version.
- What if someone tries to register a username that is already taken, or violates the username format? — Registration is refused with a clear reason and no account is created.
- What if a password is too short or empty? — Registration (and password change) is refused; a minimum length is imposed (see Assumptions).
- What if a signed-in user's session is used while an admin deactivates/demotes them? — The session no longer authorizes the user: application content is refused on the next interaction and they are returned to sign-in.
- What if the last admin tries to demote or deactivate themselves? — Refused; the system always retains at least one active admin.
- What if a user changes their username? — The change is immediate and the corner chip reflects it; historical activity already recorded under the old identity is not rewritten (see Assumptions).
- What if a user is mid-view (e.g., a running goal stream) when they sign out? — Signing out ends the session and returns to the sign-in screen; no live view continues on their stale session.
- What if a signed-out visitor navigates directly to a console address? — Sign-in is required, so they are sent to the sign-in screen rather than seeing content.
- What if two people use the console in the same browser session? — There is one session per browser; signing in as one user ends the previous identity, and the corner chip always shows the current signed-in user.

## Requirements *(mandatory)*

### Functional Requirements

**Account creation & sign-in**

- **FR-001**: System MUST let a person create an account with a unique username and a password — no email address, no confirmation step — and that account MUST be usable for sign-in immediately.
- **FR-002**: System MUST validate registration input — no duplicate usernames, username shape rules, and a minimum password length (see Assumptions) — refusing creation with a clear reason otherwise.
- **FR-003**: System MUST treat the first self-registered account as an admin whenever no active admin exists — a fresh system, a wiped/reset system, or one whose sole admin was removed — and MUST treat every self-registered account created while an admin is active as a member; on the seeded admin's first sign-in, the system MUST acknowledge the grant with a lightweight, dismissible one-line notice noting the account is admin and that admin access can be delegated to others from account administration.
- **FR-004**: System MUST authenticate a sign-in attempt by username and password; on success it MUST establish a signed-in session and on failure it MUST return the same generic message regardless of whether the username exists.
- **FR-005**: System MUST require an active signed-in session to use the console: a visitor without one MUST be presented with the sign-in/registration screen, not application content, for every console address.

**Session & corner identity**

- **FR-006**: System MUST display, on every console page for a signed-in user, a corner chip showing the first letter of their username, and clicking it MUST open a small menu offering account settings and sign-out.
- **FR-007**: System MUST end the session when the user chooses sign-out and MUST return them to the sign-in screen.
- **FR-008**: System MUST record the signed-in user's identity on the actions they take in the console (replacing the current stub identity on activity it stamps).

**Account management**

- **FR-009**: System MUST let a signed-in user change their own username and their own password — the password change requires confirmation of the current password — from account settings reached via the corner chip, taking effect immediately.
- **FR-010**: System MUST NOT offer self-service recovery of a forgotten password (there is no email); recovery MUST be possible only through an admin resetting the password (FR-012).

**Admins & roles**

- **FR-011**: System MUST model each account with a role — admin or member — stored as a single extensible value so additional groups can be added later without re-architecting.
- **FR-012**: System MUST let an admin view the account list, create an account, reset an account's password, grant or revoke the admin role, and deactivate an account.
- **FR-013**: System MUST refuse an action that would leave the console with zero active admins — in particular an admin MUST NOT be able to demote or deactivate the last active admin.
- **FR-014**: System MUST restrict the account administration capabilities of FR-012 to admins; a member MUST NOT be able to reach or perform them.

**Header cleanup**

- **FR-015**: System MUST remove the **Objectives** and **Heuristics** links from the top header on every console page, leaving the brand mark and the **Datasets** link, with the corner identity chip anchoring the right edge, and MUST ensure every remaining navigation path reaches a working destination (the removed views remain reachable through the dataset inventory flow, not dead routes).

### Key Entities

- **User (Account)** — A person's identity on the console. Key attributes: unique username, a credential that is never stored or recoverable in plaintext, a role (admin or member, extensible to further groups), an active status, and created/updated timestamps.
- **Session** — The signed-in state that lets a user use the console; it is bound to the user, established on successful sign-in and ended on sign-out (or loss of authorization).
- **Role** — The single-valued privilege group attached to an account, beginning with admin and member; designed as an enum-like value so later groups extend it without restructuring.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A new user creates an account and signs in on a fresh system in under 2 minutes on their first attempt, with no email or confirmation step.
- **SC-002**: 100% of valid credential pairs sign in successfully and 100% of invalid pairs are rejected, and every rejection shows the identical generic message.
- **SC-003**: A signed-out visitor is shown the sign-in screen for every console address, and a signed-in user reaches application content, 100% of the time.
- **SC-004**: The corner chip displays the first letter of the signed-in username on every console page, and its menu exposes account settings and sign-out on every console page, 100% of the time.
- **SC-005**: A username or password change takes effect immediately — the new credentials sign in, the old ones do not, and the corner chip reflects a new initial without reload weirdness.
- **SC-006**: A password reset by an admin lets the affected user sign in immediately.
- **SC-007**: The console always retains at least one active admin; attempts to demote or deactivate the last admin are refused 100% of the time.
- **SC-008**: On every console page the header shows only the brand mark, the **Datasets** link, and the corner identity chip; **Objectives** and **Heuristics** links appear 0 times, and every remaining link works.
- **SC-009**: From an empty system, the first registrant reaches the signed-in admin state in under 2 minutes and in at most 2 screens, with a one-line dismissible acknowledgment of their admin grant (and of delegation) shown on first sign-in — 100% of the time.

## Assumptions

- The console requires sign-in: a signed-out visitor sees the sign-in/registration screen for every address, and application content is only served to an active session.
- Registration is open self-service. An account is seeded as admin whenever no active admin exists — the first registrant on a fresh system, and again after a store reset or if the sole admin is removed; a registration that happens while an admin is active creates a member. There is no email or confirmation of any kind.
- The console is a single shared workspace: accounts identify who is acting and govern account administration, not data visibility — all signed-in members see the same datasets, goals, and heuristics. Per-user data isolation is the previously backlogged tenancy work and is out of scope.
- A session lasts until the user signs out (or is deauthorized); there is no idle timeout in this version.
- There is no self-service forgotten-password recovery without an email address; an admin resets the password instead.
- Reasonable default input rules: username of 3–32 characters using letters, digits, `_` and `-`; password of at least 8 characters. The rules are uniform across registration, username change, and password change.
- Changing a username does not rewrite historical activity already attributed to the old identity; the change applies from that point forward.
- The removed **Objectives** and **Heuristics** header links do not delete any capability — those views remain reachable via the dataset-linked flow (consistent with the current landing-page aliasing behavior), and the system handles `Objectives`/`Heuristics` URL references gracefully.
- The existing activity/audit trail now carries the signed-in user's identity where a stub identity was previously used; an unauthenticated (signed-out) user performs no work in the console.
- Roles begin with admin and member as a single value per account; the model leaves room for additional groups later without a redesign.