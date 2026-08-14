"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { getMe, updateMe, errorMessage, type MeDto } from "@/lib/orchestrator";
import { Button, Callout, Panel, SectionLabel } from "@/components/ui";

// SettingsPage is the self-service account surface: rename the username and
// change the password (current password required). Each form is independent and
// reflects the orchestrator's {error} verbatim -- validation reasons, the 409
// conflict, and the 403 wrong-current-password are analyst-safe. A successful
// change refreshes the identity so the chip's username/role re-reads.
export default function SettingsPage() {
  const router = useRouter();
  const [me, setMe] = useState<MeDto | null>(null);

  const [newUsername, setNewUsername] = useState("");
  const [usernamePending, setUsernamePending] = useState(false);
  const [usernameMsg, setUsernameMsg] = useState<{ ok?: string; err?: string }>({});

  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [passwordPending, setPasswordPending] = useState(false);
  const [passwordMsg, setPasswordMsg] = useState<{ ok?: string; err?: string }>({});

  useEffect(() => {
    let cancelled = false;
    getMe()
      .then((m) => {
        if (!cancelled) {
          setMe(m);
          setNewUsername(m.username);
        }
      })
      .catch(() => {
        if (!cancelled) router.replace("/login");
      });
    return () => {
      cancelled = true;
    };
  }, [router]);

  async function rename() {
    setUsernameMsg({});
    setUsernamePending(true);
    try {
      setMe(await updateMe({ username: newUsername.trim() }));
      setUsernameMsg({ ok: "Username updated." });
      router.refresh();
    } catch (err) {
      setUsernameMsg({ err: errorMessage(err, "Could not update the username.") });
    } finally {
      setUsernamePending(false);
    }
  }

  async function changePassword() {
    setPasswordMsg({});
    setPasswordPending(true);
    try {
      await updateMe({
        password: newPassword,
        current_password: currentPassword,
      });
      setCurrentPassword("");
      setNewPassword("");
      setPasswordMsg({ ok: "Password updated." });
    } catch (err) {
      setPasswordMsg({ err: errorMessage(err, "Could not change the password.") });
    } finally {
      setPasswordPending(false);
    }
  }

  return (
    <div className="flex flex-col gap-8 pt-4">
      <div className="flex flex-col gap-3 border-b border-line pb-6">
        <SectionLabel>Settings</SectionLabel>
        <h1 className="text-2xl font-semibold tracking-tight">Your account</h1>
        <p className="max-w-xl text-sm leading-relaxed text-muted">
          Signed in as <span className="font-mono text-fg">{me?.username}</span>.
          Rename your username or change your password. Neither ends your current
          session, but the next sign-in uses the new value.
        </p>
      </div>

      <div className="grid max-w-3xl gap-6">
        <Panel className="p-5">
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              void rename();
            }}
          >
            <div className="flex flex-col gap-2">
              <SectionLabel>Username</SectionLabel>
            </div>
            <label className="flex flex-col gap-2">
              <span className="text-sm text-muted">Rename (3–32 letters, digits, _ and -)</span>
              <input
                value={newUsername}
                onChange={(e) => setNewUsername(e.target.value)}
                autoComplete="username"
                className="w-full rounded-lg border border-line bg-surface px-4 py-3 font-mono text-sm text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
              />
            </label>
            {usernameMsg.ok && <Callout tone="info">{usernameMsg.ok}</Callout>}
            {usernameMsg.err && <Callout tone="error">{usernameMsg.err}</Callout>}
            <div>
              <Button type="submit" loading={usernamePending}>
                Update username
              </Button>
            </div>
          </form>
        </Panel>

        <Panel className="p-5">
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              void changePassword();
            }}
          >
            <div className="flex flex-col gap-2">
              <SectionLabel>Password</SectionLabel>
            </div>
            <label className="flex flex-col gap-2">
              <span className="text-sm text-muted">Current password</span>
              <input
                type="password"
                value={currentPassword}
                onChange={(e) => setCurrentPassword(e.target.value)}
                autoComplete="current-password"
                className="w-full rounded-lg border border-line bg-surface px-4 py-3 font-mono text-sm text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
              />
            </label>
            <label className="flex flex-col gap-2">
              <span className="text-sm text-muted">New password (at least 8 characters)</span>
              <input
                type="password"
                value={newPassword}
                onChange={(e) => setNewPassword(e.target.value)}
                autoComplete="new-password"
                className="w-full rounded-lg border border-line bg-surface px-4 py-3 font-mono text-sm text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
              />
            </label>
            {passwordMsg.ok && <Callout tone="info">{passwordMsg.ok}</Callout>}
            {passwordMsg.err && <Callout tone="error">{passwordMsg.err}</Callout>}
            <div>
              <Button
                type="submit"
                loading={passwordPending}
                disabled={!currentPassword || !newPassword}
              >
                Change password
              </Button>
            </div>
          </form>
        </Panel>
      </div>
    </div>
  );
}