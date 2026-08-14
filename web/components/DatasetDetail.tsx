"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import {
  deleteDataset,
  errorMessage,
  getDataset,
  getMe,
  listShares,
  revokeShare,
  shareDataset,
  updateDataset,
  type DatasetDetail,
  type DatasetSummary,
  type ObjectiveSummary,
  type ShareGrant,
} from "@/lib/orchestrator";
import { canAdminister, isInUse } from "@/lib/datasets";
import { formatTimestamp, shortId } from "@/lib/format";
import {
  Badge,
  Button,
  Callout,
  Panel,
  SectionLabel,
  Spinner,
  type BadgeTone,
} from "@/components/ui";
import { GoalForm } from "@/components/GoalForm";

// DatasetDetail renders one dataset's metadata (editable — US4), its objectives
// (the children that block deletion until they are gone — US1/US6), and the
// delete control with the 409's named refusal surfaced.
export function DatasetDetail({ id }: { id: string }) {
  const router = useRouter();
  const [detail, setDetail] = useState<DatasetDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [actingUserID, setActingUserID] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    getDataset(id)
      .then((d) => {
        if (!cancelled) setDetail(d);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(errorMessage(err, "Could not load the dataset. Please retry."));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    // The acting user's identity decides which affordances render: only the
    // owner administers, a collaborator works. getMe is cheap and the detail
    // view is the one place ownership actually gates something visible.
    getMe()
      .then((me) => {
        if (!cancelled) setActingUserID(me.id);
      })
      .catch(() => {
        // Identity is an affordance input, not the page's content: a failed
        // read leaves every owner-only control hidden, never broken.
      });
    return () => {
      cancelled = true;
    };
  }, [id]);

  if (loading && !detail) {
    return (
      <Panel className="flex items-center justify-center px-6 py-16 text-sm text-muted">
        <Spinner className="mr-2" /> Loading dataset…
      </Panel>
    );
  }
  if (error || !detail) {
    return (
      <div className="flex flex-col gap-4 pt-4">
        <Callout tone="error">{error ?? "Dataset not found."}</Callout>
        <Link href="/datasets" className="text-sm text-signal hover:underline">
          ← Back to the inventory
        </Link>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-8 pt-4">
      <div className="flex items-start justify-between gap-4 border-b border-line pb-6">
        <div className="flex flex-col gap-2">
          <SectionLabel>Dataset</SectionLabel>
          <h1 className="text-2xl font-semibold tracking-tight">{detail.name}</h1>
          <p className="max-w-xl text-sm leading-relaxed text-muted">
            {detail.description || "No description."}
          </p>
        </div>
        <StatusBadge status={detail.status} usage={detail.usage} />
      </div>

      <div className="grid gap-6 lg:grid-cols-[minmax(0,0.9fr)_minmax(0,1.1fr)]">
        <MetadataPanel
          detail={detail}
          onSaved={(updated) =>
            setDetail((prev) =>
              prev ? { ...updated, objectives: prev.objectives } : prev,
            )
          }
          onError={setSaveError}
        />
        <div className="flex flex-col gap-6">
          <RegistrationPanel detail={detail} />
          <ObjectivesPanel objectives={detail.objectives} />
          {actingUserID &&
            canAdminister(detail, actingUserID) && (
              <SharingPanel id={detail.id} ownerID={detail.owner_id} />
            )}
          <DangerZone
            detail={detail}
            deleting={deleting}
            onDelete={async () => {
              setDeleting(true);
              setSaveError(null);
              try {
                await deleteDataset(id);
                router.push("/datasets");
              } catch (err: unknown) {
                setSaveError(
                  errorMessage(
                    err,
                    "Could not delete the dataset. Please retry.",
                  ),
                );
                setDeleting(false);
              }
            }}
          />
          {saveError && <Callout tone="error">{saveError}</Callout>}
        </div>
      </div>
    </div>
  );
}

function StatusBadge({
  status,
  usage,
}: {
  status: string;
  usage: string;
}) {
  const tone: Record<string, BadgeTone> = {
    active: "positive",
    archived: "neutral",
    in_use: "warn",
    empty: "neutral",
  };
  return (
    <div className="flex shrink-0 items-center gap-2">
      <Badge tone={tone[usage] ?? "neutral"}>{usage.replace("_", " ")}</Badge>
      <Badge tone={tone[status] ?? "neutral"}>{status}</Badge>
    </div>
  );
}

// MetadataPanel is the editable metadata form (US4): name, description, and the
// lifecycle status. Metadata only — the data source binding is immutable.
function MetadataPanel({
  detail,
  onSaved,
  onError,
}: {
  detail: DatasetDetail;
  onSaved: (d: DatasetSummary) => void;
  onError: (msg: string | null) => void;
}) {
  const [name, setName] = useState(detail.name);
  const [description, setDescription] = useState(detail.description);
  const [status, setStatus] = useState<"active" | "archived">(detail.status);
  const [saving, setSaving] = useState(false);

  const dirty =
    name !== detail.name ||
    description !== detail.description ||
    status !== detail.status;

  async function onSave() {
    if (!name.trim()) {
      onError("Name is required.");
      return;
    }
    if (!dirty) return;
    setSaving(true);
    onError(null);
    try {
      // A patch with already-applied values is fine, but only send what changed;
      // the status column rejects nothing, while an empty name would.
      const patch = {
        ...(name !== detail.name ? { name: name.trim() } : {}),
        ...(description !== detail.description ? { description } : {}),
        ...(status !== detail.status ? { status } : {}),
      };
      const updated = await updateDataset(detail.id, patch);
      onSaved(updated);
      setName(updated.name);
      setDescription(updated.description);
      setStatus(updated.status);
    } catch (err: unknown) {
      onError(errorMessage(err, "Could not save the dataset metadata."));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Panel className="flex flex-col gap-5 p-5">
      <SectionLabel>Metadata</SectionLabel>
      <div className="flex flex-col gap-2">
        <label htmlFor="edit-name" className="text-sm text-muted">
          Name
        </label>
        <input
          id="edit-name"
          type="text"
          value={name}
          onChange={(e) => setName(e.target.value)}
          className="w-full rounded-lg border border-line bg-surface px-4 py-3 font-mono text-sm text-fg outline-hidden transition-colors focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
        />
      </div>
      <div className="flex flex-col gap-2">
        <label htmlFor="edit-description" className="text-sm text-muted">
          Description
        </label>
        <textarea
          id="edit-description"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          rows={3}
          className="w-full resize-y rounded-lg border border-line bg-surface px-4 py-3 text-sm leading-relaxed text-fg outline-hidden transition-colors focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
        />
      </div>
      <div className="flex flex-col gap-2">
        <span className="text-sm text-muted">Status</span>
        <div className="flex gap-1 rounded-lg border border-line bg-surface p-1">
          {(["active", "archived"] as const).map((s) => (
            <button
              key={s}
              type="button"
              aria-pressed={status === s}
              onClick={() => setStatus(s)}
              className={`flex-1 rounded-md px-3 py-2 text-sm font-medium transition-colors ${
                status === s
                  ? "bg-surface-2 text-signal shadow-[0_0_0_1px_rgba(123,241,168,0.25)]"
                  : "text-muted hover:text-fg"
              }`}
            >
              {s}
            </button>
          ))}
        </div>
        <p className="text-xs text-faint">
          Archiving a dataset keeps it and its objectives read-only upstream: it
          cannot accept new goals.
        </p>
      </div>
      <div className="flex items-center gap-3">
        <Button onClick={onSave} loading={saving} disabled={!dirty}>
          Save metadata
        </Button>
        {!dirty && <span className="text-xs text-faint">No changes</span>}
      </div>
    </Panel>
  );
}

// RegistrationPanel hosts the objective form, bound to this dataset (US4).
// Registration moved here from the root URL: the primary surface to register a
// new goal is the dataset its data lands in. An archived dataset cannot accept
// new objectives — the backend 409s a bound goal — so the form is replaced by
// the reason, which also points at the un-archive control in Metadata.
function RegistrationPanel({ detail }: { detail: DatasetDetail }) {
  return (
    <Panel className="flex flex-col gap-5 p-5">
      <SectionLabel>Register an objective</SectionLabel>
      {detail.status === "active" ? (
        <GoalForm initialDatasetID={detail.id} />
      ) : (
        <p className="py-2 text-sm leading-relaxed text-muted">
          This dataset is archived and read-only, so it cannot accept new
          objectives. Switch it back to <span className="text-fg">active</span>{" "}
          in Metadata first.
        </p>
      )}
    </Panel>
  );
}

function ObjectivesPanel({ objectives }: { objectives: ObjectiveSummary[] }) {
  return (
    <Panel className="flex flex-col gap-3 p-5">
      <SectionLabel>
        Objectives · {objectives.length}
      </SectionLabel>
      {objectives.length === 0 ? (
        <p className="py-6 text-center text-sm text-faint">
          No objectives registered against this dataset yet. Register one above
          to optimize against its data.
        </p>
      ) : (
        <ul className="flex flex-col gap-2">
          {objectives.map((o) => (
            <li key={o.optimization_function_id}>
              <ObjectivesRow objective={o} />
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

function ObjectivesRow({ objective }: { objective: ObjectiveSummary }) {
  const tone: Record<string, BadgeTone> = {
    running: "positive",
    completed: "positive",
    failed: "negative",
    "no run": "neutral",
  };
  return (
    <Link
      href={`/goals/${objective.optimization_function_id}`}
      className="flex items-center justify-between gap-4 rounded-lg border border-line bg-surface/60 px-4 py-3 transition-colors hover:border-line-strong"
    >
      <span className="truncate text-sm leading-relaxed text-fg">
        {objective.goal_text}
      </span>
      <div className="flex shrink-0 items-center gap-2">
        <Badge tone={tone[objective.status] ?? "neutral"}>
          {objective.status || "unknown"}
        </Badge>
        <span className="font-mono text-[11px] text-faint">
          {shortId(objective.optimization_function_id)}
        </span>
      </div>
    </Link>
  );
}

// SharingPanel is the owner-only collaborator management surface (US4): the
// recipient list, add-by-username, and revoke-with-confirm. It renders only
// when the acting user administers the dataset, so a collaborator never sees
// the very grants the backend refuses to serve them. The backend's verbatim
// refusal bodies -- an unknown username (404), a duplicate grant (409), a
// self-share (400) -- surface exactly as the {error} the orchestrator returns.
function SharingPanel({ id, ownerID }: { id: string; ownerID: string | null }) {
  const [shares, setShares] = useState<ShareGrant[] | null>(null);
  const [username, setUsername] = useState("");
  const [sharingError, setSharingError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const refresh = useCallback(() => {
    setSharingError(null);
    listShares(id)
      .then(setShares)
      .catch((err: unknown) => {
        setShares([]);
        setSharingError(errorMessage(err, "Could not load collaborators."));
      });
  }, [id]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  async function onAdd() {
    const target = username.trim();
    if (!target) return;
    setBusy(true);
    setSharingError(null);
    try {
      await shareDataset(id, target);
      setUsername("");
      refresh();
    } catch (err: unknown) {
      setSharingError(errorMessage(err, "Could not share the dataset."));
    } finally {
      setBusy(false);
    }
  }

  async function onRevoke(grant: ShareGrant) {
    if (!window.confirm(`Stop sharing “${id}” with ${grant.username}?`)) return;
    setBusy(true);
    setSharingError(null);
    try {
      await revokeShare(id, grant.username);
      refresh();
    } catch (err: unknown) {
      setSharingError(errorMessage(err, "Could not revoke access."));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Panel className="flex flex-col gap-4 p-5">
      <SectionLabel>Collaborators</SectionLabel>
      {ownerID === null ? (
        <p className="py-2 text-sm leading-relaxed text-muted">
          This dataset has no attributed owner yet, so sharing is unavailable.
        </p>
      ) : (
        <>
          <ul className="flex flex-col gap-2">
            {shares === null ? (
              <li className="flex items-center gap-2 py-2 text-sm text-faint">
                <Spinner className="h-3.5 w-3.5" /> Loading collaborators…
              </li>
            ) : shares.length === 0 && !sharingError ? (
              <li className="py-2 text-sm text-faint">
                No collaborators yet. Share with an account by username below.
              </li>
            ) : (
              shares.map((g) => (
                <li
                  key={g.user_id}
                  className="flex items-center justify-between gap-4 rounded-lg border border-line bg-surface/60 px-4 py-3"
                >
                  <div className="flex min-w-0 flex-col">
                    <span className="truncate text-sm font-medium text-fg">
                      {g.username}
                    </span>
                    <span className="text-xs text-faint">
                      since {formatTimestamp(g.created_at)}
                    </span>
                  </div>
                  <Button
                    variant="ghost"
                    loading={busy}
                    onClick={() => void onRevoke(g)}
                    className="border-neg/40 text-neg hover:border-neg/70 hover:text-neg"
                  >
                    Revoke
                  </Button>
                </li>
              ))
            )}
          </ul>
          <form
            className="flex items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              void onAdd();
            }}
          >
            <div className="flex min-w-0 flex-1 flex-col gap-2">
              <label htmlFor="share-username" className="text-sm text-muted">
                Username
              </label>
              <input
                id="share-username"
                type="text"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder="account to grant access"
                className="w-full rounded-lg border border-line bg-surface px-4 py-3 font-mono text-sm text-fg outline-hidden transition-colors focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
              />
            </div>
            <Button type="submit" loading={busy} disabled={!username.trim()}>
              Share
            </Button>
          </form>
        </>
      )}
      {sharingError && <Callout tone="error">{sharingError}</Callout>}
    </Panel>
  );
}

// DangerZone is the delete control. In use, the orchestrator refuses with 409
// and lists the blocking objectives — the UI surfaces that named refusal and
// does not optimistically clear. Empty, a confirm dialog precedes the request.
function DangerZone({
  detail,
  deleting,
  onDelete,
}: {
  detail: DatasetDetail;
  deleting: boolean;
  onDelete: () => Promise<void>;
}) {
  const inUse = isInUse(detail);
  return (
    <Panel className="flex flex-col gap-3 border-neg/25 p-5">
      <SectionLabel className="text-neg">Delete dataset</SectionLabel>
      {inUse ? (
        <>
          <p className="text-sm leading-relaxed text-muted">
            This dataset still holds{" "}
            <span className="text-fg">{detail.objective_count}</span> objective
            {detail.objective_count === 1 ? "" : "s"}. Delete those first — a
            dataset with objectives cannot be removed.
          </p>
          {detail.objectives.length > 0 && (
            <ul className="flex flex-col gap-1">
              {detail.objectives.map((o) => (
                <li key={o.optimization_function_id} className="font-mono text-xs text-faint">
                  {o.optimization_function_id} ·{" "}
                  <span className="text-muted">{o.goal_text}</span>
                </li>
              ))}
            </ul>
          )}
        </>
      ) : (
        <>
          <p className="text-sm leading-relaxed text-muted">
            Removes the dataset, its data-source object, and its registry row.
            This cannot be undone.
          </p>
          <Button
            variant="ghost"
            loading={deleting}
            disabled={deleting}
            onClick={() => {
              if (window.confirm(`Delete dataset “${detail.name}”?`)) {
                void onDelete();
              }
            }}
            className="self-start border-neg/40 text-neg hover:border-neg/70 hover:text-neg"
          >
            {deleting ? "Deleting…" : "Delete dataset"}
          </Button>
        </>
      )}
    </Panel>
  );
}