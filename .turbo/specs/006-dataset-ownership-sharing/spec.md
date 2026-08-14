# Feature Specification: Dataset Ownership & Sharing

**Feature Branch**: `006-dataset-ownership-sharing`

**Created**: 2026-08-14

**Status**: Draft

**Input**: User description: "Current state is that users can see all datasets and their children objects in the system, whether they created them or not. Datasets, and all of their children objects must be owned by their creators, and not visible to other users unless explicitly shared to another user. DO NOT COMMIT TO GIT BEFORE YOU ARE INSTRUCTED TO DO SO. Use the AGENTS.md file as your constitution."

## Overview

The console is today a single shared workspace: every signed-in user sees every dataset and everything beneath it — goals (objectives), their runs, verifications, heuristics and embeddings, and the causal/outcome views — whether they created any of it or not. Nothing user-facing records who owns a dataset, and no view, list, search, or direct link is filtered by the signed-in user. The audit trail records *who did what*, but it is not readable by runtime roles and is not a visibility mechanism.

This feature introduces ownership: **a dataset, and every child object beneath it, is owned by the user who created it, and is not visible to any other user unless the owner explicitly shares the dataset with that user.** The dataset is the ownership root — a child object (a goal, its runs, its verifications, its heuristics, its graph) inherits the accessibility of the dataset it hangs from, so access is decided in one place. Visibility stops at the account boundary: users cannot see, search, stream, or deep-link to a dataset (or anything under it) they do not own or have not been shared.

Three design decisions materially shape this feature and were resolved during specification (see [Clarifications](#clarifications)): pre-existing and system-created data is owned by an administrator caretaker; administrators are held to the same ownership and sharing rules as every other user; and sharing conveys working access without dataset administration.

## Users

- **Dataset owner (primary)** — a signed-in user who creates a dataset. Sees and manages their own datasets and everything under them, shares a dataset with other accounts, and revokes access. Owns the dataset's metadata and lifecycle.
- **Collaborator** — a user a dataset is explicitly shared with. Gains working access to that dataset and its children (view, contribute, manage their own created children) while dataset administration stays with the owner.
- **Unrelated user** — a signed-in user who neither created nor was shared a dataset. Must not see it anywhere: not in lists, searches, streams, or direct links.
- **Administrator** — an account admin (as in the existing account-role model) who is held to the same ownership and sharing rules as every other user: an admin sees a dataset only if they own it or it is shared with them, and pre-existing/system-created data is owned by an admin caretaker.
- **System processes** — background work (reconciliation, the sleep-cycle run, verifier auto-promotion) that creates or touches objects without a signed-in user; such objects are attributed to an admin caretaker owner and governed by the same ownership rules as user-created data, never surfaced to arbitrary users.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The inventory shows only what I can access (Priority: P1)

As a signed-in user, I want the dataset inventory (the home page and `/datasets`) to show only the datasets I own or have been shared with me, so that other people's work never appears in my console by default.

**Why this priority**: This is the entire point of the request — the current behavior shows every dataset to everyone. Until lists are filtered, no visibility boundary exists at all.

**Independent Test**: Can be fully tested by signing in as two different users, having user B create a dataset, and confirming that user A's inventory (home and `/datasets`) shows nothing of B's — and that B's own inventory shows it — with no empty placeholder, error, or trace of the other user's data.

**Acceptance Scenarios**:

1. **Given** a signed-in user, **When** I open the home page or `/datasets`, **Then** I see only the datasets I own and the datasets shared with me — nothing else.
2. **Given** another user's dataset that has not been shared with me, **When** I browse the inventory, **Then** it does not appear, in any form (no row, no placeholder for it, no count that hints at it).
3. **Given** a dataset shared with me, **When** I open the inventory, **Then** it appears alongside my own, and I can tell it is shared rather than mine.

---

### User Story 2 - Children inherit the dataset's visibility (Priority: P1)

As a signed-in user, I want every child object — goals, runs, verifications, heuristics, outcomes, causal views — to be reachable exactly when its dataset is accessible to me, so a hidden dataset leaks nothing beneath it.

**Why this priority**: Children are where the data actually lives; if a user can reach a goal, run, or heuristic under a dataset they cannot access, the ownership boundary is meaningless.

**Independent Test**: Can be fully tested by creating a dataset with several goals and runs as user B, then as user A confirming no goal, run, verification, heuristic, or outcome under B's dataset is reachable through any of `/goals`, the heuristics browser, a goal detail URL, or a search — while a dataset B shares with A reveals its children normally.

**Acceptance Scenarios**:

1. **Given** a dataset I cannot access, **When** I open the goals list or heuristics browser, **Then** none of its goals or heuristics appear, and searches do not return them.
2. **Given** a goal URL under a dataset I cannot access, **When** I navigate to it directly, **Then** I am refused — no run, verification, outcome, or causal content is rendered.
3. **Given** a dataset shared with me, **When** I open its goals, a goal detail, its runs, its verifications, or its heuristics, **Then** they render exactly as they do for the owner.

---

### User Story 3 - Creating something makes me its owner (Priority: P1)

As a signed-in user, I want anything I create — a dataset, or a goal (and its runs and verifications) under a dataset — to be mine, so that from the moment of creation it is visible only to me and to whoever I share it with.

**Why this priority**: Ownership is the source of the whole boundary; without it there is nothing to enforce, and a fresh dataset is the first thing an owner shares or keeps private.

**Independent Test**: Can be fully tested by creating a dataset as one user and confirming it is owned by and only visible to that user immediately, and by creating a goal under a shared dataset and confirming the goal's creator is recorded and its accessibility follows the dataset.

**Acceptance Scenarios**:

1. **Given** a signed-in user, **When** I create a new dataset, **Then** I am its owner and no other user can see it by default.
2. **Given** a user working in a dataset shared with them, **When** they create a goal, **Then** the goal's creator is recorded, and anyone who can access the dataset can see and work with the goal.
3. **Given** a dataset minted implicitly from a data source reference during goal submission, **When** it comes into existence, **Then** the acting user becomes its owner.

---

### User Story 4 - Share a dataset with another user (Priority: P2)

As a dataset owner, I want to explicitly share a dataset with another account, so that a chosen colleague gains access to the dataset and everything under it.

**Why this priority**: Sharing is the only sanctioned way another user sees a dataset; the feature is not complete until an owner can let someone in (and, per US5, let them out).

**Independent Test**: Can be fully tested by sharing a dataset with a second account and confirming that account's inventory now lists it and its children are reachable, with working access (view and contribute) but no dataset administration.

**Acceptance Scenarios**:

1. **Given** a dataset I own, **When** I share it with another account, **Then** that account sees the dataset and its children on their next load and can view and contribute to them, but only I can administer the dataset.
2. **Given** a dataset shared with me, **When** I try to manage its sharing, **Then** I cannot — sharing is owner-only, and the affordance is not offered to me.
3. **Given** a dataset I own, **When** I share it with several accounts, **Then** each recipient's access is independent of the others'.

---

### User Story 5 - Revoke sharing and lose access immediately (Priority: P2)

As a dataset owner, I want to revoke a user's access to a dataset, so that access ends as soon as I decide, and the recipient cannot keep using what was shared.

**Why this priority**: Access must be revocable or sharing is irreversible; revocation is what makes the boundary trustworthy for an owner.

**Independent Test**: Can be fully tested by revoking a share and confirming the recipient's next load of any surface — inventory, direct link, stream, or search — no longer reaches the dataset or its children.

**Acceptance Scenarios**:

1. **Given** a dataset shared with a user, **When** I revoke the share, **Then** that user immediately loses access — the dataset disappears from their inventory and direct links to it (and its children) are refused on their next request.
2. **Given** a user whose access was just revoked, **When** they use a cached page or an open stream, **Then** the next action that reads data is refused rather than continuing to work.

---

### User Story 6 - Only the owner administers the dataset (Priority: P2)

As a dataset owner, I want to be the only person who can edit the dataset's metadata, delete it, or change its sharing, so that collaborators can work inside it without being able to destroy or re-purpose it.

**Why this priority**: Ownership is meaningless if a collaborator can rename or delete the dataset or invite others; the owner must hold the administrative edge.

**Independent Test**: Can be fully tested by sharing a dataset, then confirming the collaborator is refused on rename, delete, and share-management while the owner succeeds on all three.

**Acceptance Scenarios**:

1. **Given** a dataset shared with a collaborator, **When** the collaborator tries to edit its metadata or delete it, **Then** they are refused with a clear result, while the owner succeeds.
2. **Given** a collaborator working inside a shared dataset, **When** they create their own goal, **Then** they can edit and delete that goal, while they cannot delete a goal they did not create.

---

### User Story 7 - Administrators follow the same rules (Priority: P3)

As an administrator, I want to be held to the same ownership and sharing rules as every other user, so that no account — including an admin — sees a dataset that was not explicitly shared with it.

**Why this priority**: The requirement is absolute ("not visible to other users unless explicitly shared"), so the admin role grants no data-visibility exception; this story closes that loophole deliberately.

**Independent Test**: Can be fully tested by signing in as an admin and confirming that a dataset owned by another user (and not shared with the admin) is invisible to the admin on every surface, exactly as it is for any unrelated user.

**Acceptance Scenarios**:

1. **Given** a dataset owned by another user and not shared with me, **When** I open the inventory, a direct link, or any child surface as an admin, **Then** I see and reach nothing of it — the same outcome as any unrelated user.
2. **Given** a dataset shared with me, **When** I use it as an admin, **Then** I have the same working access as any other collaborator, and no more.

---

### Edge Cases

- **Direct link to a hidden dataset/goal**: A signed-in user who guesses or bookmarks a URL to a dataset, goal, run, or heuristic under a dataset they cannot access must be refused, never shown a partial or empty frame that leaks the object's existence.
- **Revoked access with an open view**: A collaborator mid-view (an open goal stream, a chat, a search) when access is revoked must lose access on the next data action, not continue on a stale authorization.
- **Deep link after revocation**: Bookmarks and browser history must not restore a revoked user's view; the next load re-checks access and is refused.
- **Pre-existing data**: Datasets and children created before ownership existed have no recorded creator; they are owned by an admin caretaker and follow the same rules as any dataset — visible only to that owner and whomever they share with. This attribution must be applied consistently.
- **System-created objects**: Datasets minted during reconciliation, heuristics published by a system run, and other objects created without a signed-in user are owned by an admin caretaker and never surface to arbitrary users.
- **Implicit dataset minting**: Submitting a goal for a data source reference that has no dataset yet must not leak the new dataset — the acting user owns it immediately (US3.3).
- **Deactivated or removed accounts**: Sharing to an account that is later deactivated must not leak data; the grant dies with the account. If the account is removed, the share is removed with it.
- **Last-owner edge**: A dataset always retains an owner; deleting the owning account must not orphan the dataset into invisibility — its ownership is reassigned (e.g., to another admin caretaker) rather than lost.
- **Username changes**: Ownership follows the account, not the username — renaming one's username (an existing self-service flow) must not change who owns what or break shares.
- **Cross-goal searches and the heuristics corpus**: Similarity search and the heuristics browser default to cross-goal retrieval; no surface may return rows whose dataset the user cannot access, including rows whose goal link is legacy or absent.
- **Shared-dataset deletions**: Deleting a dataset a collaborator was working in removes their access with the dataset; the collaborator is not left holding goals with no home.
- **Concurrent sharing changes**: If an owner grants and revokes while a collaborator is loading, the outcome is a single consistent state — the collaborator either has access or does not; no partial or cached state lingers.

## Requirements *(mandatory)*

### Functional Requirements

**Ownership model**

- **FR-001**: System MUST record the owner of every dataset as the signed-in user who created it, and MUST make the dataset visible only to its owner by default — no other user sees it in any list, URL, search, stream, or count.
- **FR-002**: System MUST record the creator of every child object (goal, run, verification, heuristic, outcome) created under a dataset; when the creating user is not the dataset owner, the object MUST inherit the dataset's accessibility so it is visible to exactly the set of users who can access the dataset.
- **FR-003**: System MUST attribute pre-existing datasets and children (created before ownership existed) and objects created by background/system processes (reconciliation-minted datasets, system-published heuristics) to an administrator caretaker owner; those objects follow the same ownership and sharing rules as user-created data — visible only to the owning admin and to whomever that admin shares with — and MUST NOT surface to arbitrary users.
- **FR-004**: System MUST NOT leak ownership through historical attribution: the existing audit trail continues to record actors, but it is not a visibility mechanism and nothing about it grants or implies access.

**Visibility**

- **FR-005**: System MUST filter every dataset-list surface — the home inventory and `/datasets` — to datasets the signed-in user owns or has shared with them; no inaccessible dataset appears in any form.
- **FR-006**: System MUST filter every child surface to the user's accessible datasets: the goals list, the heuristics browser, goal detail, runs, verifications, outcomes, causal views, and all searches (including similarity/cross-goal retrieval) MUST return only objects whose dataset the user can access.
- **FR-007**: System MUST refuse direct navigation to a dataset or child object the user cannot access — the request MUST NOT render data, a partial frame, or any content that reveals the object exists; the user-facing result is a clear refusal.
- **FR-008**: System MUST re-check access on every data action, so a user whose access is revoked (or whose account is deactivated) loses access on their next read or action, including over open streams and cached pages.

**Sharing**

- **FR-009**: System MUST let a dataset owner explicitly share a dataset with another account, granting that account working access — the ability to view the dataset and its children, create goals within it, run them, respond to verifications, and manage the children they themselves created — while dataset administration (see FR-013) stays with the owner; the grant MUST take effect immediately.
- **FR-010**: System MUST let a dataset owner revoke a share, removing the recipient's access immediately across all surfaces, including direct links and open streams.
- **FR-011**: System MUST restrict sharing management (grant, revoke, and viewing the sharing list) to the dataset owner; a non-owner collaborator MUST NOT be able to perform or see it.
- **FR-012**: System MUST expire a share when the recipient's account is deactivated or removed, and MUST NOT deliver the dataset's data to that account thereafter.

**Dataset administration**

- **FR-013**: System MUST restrict dataset administration — editing metadata, deleting the dataset, and managing sharing — to the dataset owner; a collaborator MUST NOT be able to rename or delete a dataset they do not own, regardless of sharing rights.
- **FR-014**: System MUST let the creator of a child object, and the dataset owner, edit and delete that child; a collaborator who did not create a given child MUST NOT be able to delete it.

**Administrators**

- **FR-015**: System MUST hold administrators to the same ownership and sharing rules as every other user, applied consistently across every surface: an admin can reach a dataset and its children only when they own it or it is shared with them, whether browsing the inventory, following a direct link, or using a child view.

### Key Entities

- **User (Account)** — the person identity established by the user-management feature; every dataset and child object is attributed to a user (or, for pre-existing/system-created data, an admin caretaker) as its owner/creator. Ownership follows the account id, so a username change does not affect it.
- **Dataset** — the top-level owned object and the ownership root. Key attributes: its owner, its shared-with set, and its metadata. Every dataset has exactly one owner; a user may hold access by ownership or by an explicit share.
- **Child objects** — goals (objectives) and everything beneath them: runs, verifications, heuristics and embeddings, and graph/outcome data. Each records its creator; accessibility is inherited from the dataset, never decided independently.
- **Share (access grant)** — the dataset→account grant created by an owner. Revocable, per-account, and the only sanctioned way a non-owner sees a dataset. Conveys working access without dataset administration.
- **Admin caretaker** — an administrator account that owns pre-existing data (created before ownership existed) and objects created by background processes. Such objects follow the same ownership and sharing rules as user-created data; they never surface to arbitrary users.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of signed-in users see, on every list surface, exactly the datasets they own or have shared with them — and nothing else, in any form (no rows, no placeholders, no counts hinting at hidden datasets).
- **SC-002**: 100% of child objects (goals, runs, verifications, heuristics, outcomes, causal views) are reachable if and only if the user can access their dataset; zero child objects under an inaccessible dataset are reachable through any surface, including direct URLs and searches.
- **SC-003**: A newly created dataset is owned by and visible only to its creator immediately, with no other user able to reach it by default, 100% of the time.
- **SC-004**: Sharing and revocation take effect immediately: a recipient sees a shared dataset and its children on the next load, and a revoked user loses access on the next data action, with zero stale or partial access persisting.
- **SC-005**: 100% of attempts by a non-owner to rename, delete, or re-share a dataset are refused, while the owner succeeds, every time.
- **SC-006**: 100% of attempts by a collaborator to delete a child object they did not create are refused, while the creator (or dataset owner) succeeds.
- **SC-007**: An owner's existing workflow is unchanged: create, work within, share, and revoke without new friction or false refusals against their own data.
- **SC-008**: Administrators are held to the same ownership and sharing rules as every other user on every surface: an admin never sees a dataset they neither own nor are shared, with no route behaving differently.
- **SC-009**: Pre-existing datasets/children and system-created objects are attributed to an admin caretaker owner and never surface to unrelated users, 100% of the time, on every surface.

## Assumptions

- The dataset is the ownership root: child objects inherit the accessibility of the dataset they hang from, so a user can reach a child exactly when they can reach its dataset. Access is decided once, at the dataset level.
- Sign-in is required (the existing gate stands): an unsigned-in visitor has no access to any dataset or child object whatsoever.
- Sharing is per-account and granted by the dataset owner; sharing to groups, roles, or organization-wide grants is out of scope for this feature.
- Sharing and its revocation take effect immediately on the next data read; there is no "pending" or staged sharing state.
- The existing audit trail continues to record actors on activity but remains a system concern, not a visibility or access mechanism.
- Ownership follows the account: renaming a username never changes ownership, and shares survive a rename.
- Pre-existing datasets and children, and objects created by background processes, are owned by an administrator caretaker and follow the same ownership and sharing rules as user-created data — visible only to that owner and to whomever they share with.
- Administrators are held to the same ownership and sharing rules as every other user; the admin role grants no data-visibility exception.
- Sharing conveys working access without dataset administration: the recipient can view the dataset and its children, create and run goals, respond to verifications, and manage the children they created; only the owner edits/deletes the dataset and manages sharing.
- The raw data-source reference ledger is a system-level store used by ingestion and by the execution/verification services, not a user-facing owned object; this feature governs the user-facing dataset tree, while those services continue to honor the acting user's access when executing work.
- The scope boundary of a "dataset" is its full child tree (goals, runs, verifications, heuristics/embeddings, graph and outcome data); no child surface is exempt from the visibility rule.

## Clarifications

### Design decisions (2026-08-14)

Resolved during specification; each is recorded in Assumptions so planning can revisit.

- Q: Pre-existing datasets and system-created objects have no recorded signed-in creator. Who owns them? → A: An **administrator caretaker** owns them. Historical activity was stamped with a stub identity, so the true creator is unknowable; attributing such data to an admin owner keeps it out of ordinary users' consoles while keeping a real account responsible. These objects follow the same ownership and sharing rules as any user-created dataset — visible only to the owning admin and whomever that admin shares with.
- Q: Can an admin see all data regardless of ownership? → A: **No — admins follow the same rules as every other user.** The requirement is absolute ("not visible to other users unless explicitly shared"), so the admin role grants no data-visibility exception. An admin reaches a dataset only when they own it or it is shared with them. The account-administration surface (`/users`) is unchanged; it manages accounts, not data access.
- Q: What does sharing convey? → A: **Working access without dataset administration.** A recipient views the dataset and its children, creates and runs goals, responds to verifications, and manages the children they themselves created. Only the dataset owner edits or deletes the dataset, its metadata, or its sharing.
