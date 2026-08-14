"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import {
  listUsers,
  createUser,
  updateUser,
  errorMessage,
  type UserAdminDto,
} from "@/lib/orchestrator";
import { Badge, Button, Callout, Panel, SectionLabel, cn } from "@/components/ui";

// UsersPage is the admin account roster: every account with its role and active
// state, a member-create form, and per-row promote/demote, reset-password, and
// deactivate/reactivate actions. The roster entry is hidden for members by the
// chip, but a direct visit still surfaces the server's 403 verbatim (FR-014).
export default function UsersPage() {
  const router = useRouter();
  const [users, setUsers] = useState<UserAdminDto[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [newUsername, setNewUsername] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    listUsers()
      .then((rows) => {
        if (!cancelled) setUsers(rows);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        const msg = errorMessage(err, "Could not load the roster.");
        setError(msg);
        // A member (or unsigned visitor) hitting this surface directly is
        // redirected to the sign-in/identity flow.
        if (err instanceof Error && "status" in err && (err as { status: number }).status === 403) {
          router.replace("/");
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [router]);

  async function createMember() {
    setCreateError(null);
    setCreating(true);
    try {
      const created = await createUser(newUsername.trim(), newPassword);
      setUsers((prev) => (prev ? [...prev, created] : [created]));
      setNewUsername("");
      setNewPassword("");
    } catch (err) {
      setCreateError(errorMessage(err, "Could not create the account."));
    } finally {
      setCreating(false);
    }
  }

  async function apply(id: string, patch: Record<string, unknown>) {
    try {
      const updated = await updateUser(id, patch);
      setUsers((prev) => prev?.map((u) => (u.id === id ? updated : u)) ?? null);
    } catch (err) {
      setError(errorMessage(err, "Could not update the account."));
    }
  }

  if (loading) {
    return (
      <div className="flex flex-col gap-8 pt-4">
        <SectionLabel>Accounts</SectionLabel>
        <p className="text-sm text-muted">Loading the roster…</p>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-8 pt-4">
      <div className="flex flex-col gap-3 border-b border-line pb-6">
        <SectionLabel>Accounts</SectionLabel>
        <h1 className="text-2xl font-semibold tracking-tight">User roster</h1>
        <p className="max-w-xl text-sm leading-relaxed text-muted">
          Every account on this console. Admins create members, reset forgotten
          passwords, and delegate or revoke administration. The last active
          admin can never demote or deactivate themselves out — the server
          refuses it.
        </p>
      </div>

      {error && <Callout tone="error">{error}</Callout>}

      <Panel className="p-5">
        <form
          className="flex flex-wrap items-end gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            void createMember();
          }}
        >
          <div className="flex flex-1 flex-col gap-2">
            <SectionLabel>New member</SectionLabel>
            <input
              value={newUsername}
              onChange={(e) => setNewUsername(e.target.value)}
              placeholder="username (3–32 letters, digits, _ and -)"
              className="w-full rounded-lg border border-line bg-surface px-4 py-3 font-mono text-sm text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
            />
          </div>
          <div className="flex flex-1 flex-col gap-2">
            <input
              type="password"
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
              autoComplete="new-password"
              placeholder="password (at least 8 characters)"
              className="w-full rounded-lg border border-line bg-surface px-4 py-3 font-mono text-sm text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
            />
          </div>
          <Button type="submit" loading={creating} disabled={!newUsername.trim() || !newPassword}>
            {creating ? "Creating…" : "Create member"}
          </Button>
        </form>
        {createError && <Callout tone="error" className="mt-4">{createError}</Callout>}
      </Panel>

      <div className="flex flex-col gap-2">
        {users?.map((u) => (
          <UserRow key={u.id} user={u} onApply={apply} />
        ))}
      </div>
    </div>
  );
}

function UserRow({
  user,
  onApply,
}: {
  user: UserAdminDto;
  onApply: (id: string, patch: Record<string, unknown>) => Promise<void>;
}) {
  const [busy, setBusy] = useState<string | null>(null);

  async function act(patch: Record<string, unknown>) {
    setBusy(Object.keys(patch).join("+"));
    try {
      await onApply(user.id, patch);
    } finally {
      setBusy(null);
    }
  }

  const isAdmin = user.role === "admin";
  const rowBusy = busy !== null;

  return (
    <Panel className="flex items-center justify-between gap-4 px-4 py-3">
      <div className="flex min-w-0 flex-col gap-1">
        <span className="truncate font-mono text-sm text-fg">{user.username}</span>
        <div className="flex items-center gap-2">
          <Badge tone={isAdmin ? "positive" : "neutral"}>{isAdmin ? "admin" : "member"}</Badge>
          <Badge tone={user.active ? "neutral" : "warn"}>
            {user.active ? "active" : "deactivated"}
          </Badge>
        </div>
      </div>
      <div className={cn("flex shrink-0 items-center gap-1", rowBusy && "opacity-50")}>
        {isAdmin ? (
          <RowButton onClick={() => void act({ role: "member" })} disabled={rowBusy}>
            Demote
          </RowButton>
        ) : (
          <RowButton onClick={() => void act({ role: "admin" })} disabled={rowBusy}>
            Promote
          </RowButton>
        )}
        <RowButton onClick={() => void act({ active: !user.active })} disabled={rowBusy}>
          {user.active ? "Deactivate" : "Reactivate"}
        </RowButton>
        <form
          className="flex items-center gap-1"
          onSubmit={(e) => {
            e.preventDefault();
            const input = new FormData(e.currentTarget).get("reset-pass");
            if (typeof input === "string" && input.length > 0) {
              void act({ new_password: input });
              (e.currentTarget.elements.namedItem("reset-pass") as HTMLInputElement).value = "";
            }
          }}
        >
          <input
            name="reset-pass"
            type="password"
            autoComplete="off"
            placeholder="reset password…"
            disabled={rowBusy}
            className="w-40 rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-xs text-fg outline-hidden placeholder:text-faint focus:border-signal/60 disabled:opacity-45"
          />
          <RowButton type="submit" disabled={rowBusy}>
            Reset
          </RowButton>
        </form>
      </div>
    </Panel>
  );
}

function RowButton({
  onClick,
  disabled,
  type = "button",
  children,
}: {
  onClick?: () => void;
  disabled?: boolean;
  type?: "button" | "submit";
  children: React.ReactNode;
}) {
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      className="rounded-md border border-line-strong px-3 py-1.5 text-xs text-muted transition-colors hover:border-signal/50 hover:text-signal disabled:cursor-not-allowed disabled:opacity-45"
    >
      {children}
    </button>
  );
}