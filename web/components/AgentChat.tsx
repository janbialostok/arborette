"use client";

import { useEffect, useRef, useState } from "react";
import {
  errorFrom,
  errorMessage,
  type ChatFrame,
  type ChatMessage,
} from "@/lib/orchestrator";
import {
  applyChatFrame,
  newChatTurn,
  turnOutcome,
  CHAT_FALLBACK,
  type ChatTurn,
  type ToolActivity,
  type TurnOutcome,
} from "@/lib/chatTurn";
import { consumeStream } from "@/lib/sse";
import { Button, Callout, cn, Panel, Spinner } from "@/components/ui";

// AgentChat is arborette's default agent surface: the bubbled transcript,
// inline tool activity, and composer that every agent it produces is presented
// with. It takes the endpoint rather than a goal id so any agent surface can
// mount it — the endpoint's only contract is the streamed chat frame.
//
// Chat is stateless server-side: this component holds the transcript and posts
// the whole of it each turn. The rules governing what may join that transcript
// live in lib/chatTurn.ts, which is where the reply is folded and judged.

// What the view shows when a turn does not commit — the same shape the fold
// hands back, so the two cannot drift apart.
type Notice = Extract<TurnOutcome, { kind: "notice" }>;

export function AgentChat({ endpoint }: { endpoint: string }) {
  const [turns, setTurns] = useState<ChatMessage[]>([]);
  // The turn in flight. Its presence is what "streaming" means, so there is no
  // second flag that could disagree with it.
  const [pending, setPending] = useState<ChatTurn | null>(null);
  const [draft, setDraft] = useState("");
  const [notice, setNotice] = useState<Notice | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const streaming = pending !== null;

  useEffect(() => () => abortRef.current?.abort(), []);

  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [turns, pending]);

  async function send() {
    const content = draft.trim();
    if (!content || streaming) return;
    const transcript: ChatMessage[] = [...turns, { role: "user", content }];
    setTurns(transcript);
    setDraft("");
    setNotice(null);

    const controller = new AbortController();
    abortRef.current = controller;
    let turn = newChatTurn();
    setPending(turn);

    try {
      const res = await fetch(endpoint, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          accept: "text/event-stream",
        },
        body: JSON.stringify({ messages: transcript }),
        signal: controller.signal,
      });
      // The 200 commits lazily on the first frame, so a fault before then is a
      // JSON body carrying the orchestrator's own analyst-safe message.
      if (!res.ok) throw await errorFrom(res);
      if (!res.body) throw new Error("chat response carried no body");
      await consumeStream<ChatFrame>(
        res.body,
        (ev) => {
          turn = applyChatFrame(turn, ev);
          setPending(turn);
        },
        controller.signal,
      );
    } catch (err) {
      turn = { ...turn, failure: errorMessage(err, CHAT_FALLBACK) };
    }

    if (controller.signal.aborted) return;
    abortRef.current = null;
    setPending(null);
    const outcome = turnOutcome(turn);
    if (outcome.kind === "commit") {
      setTurns((t) => [...t, { role: "assistant", content: outcome.content }]);
      return;
    }
    setNotice(outcome);
  }

  return (
    <div className="flex flex-col gap-4 pt-4">
      <Panel className="flex flex-col overflow-hidden">
        <div
          ref={scrollRef}
          className="flex max-h-[60vh] min-h-[22rem] flex-col gap-3 overflow-y-auto p-5"
        >
          {turns.length === 0 && !pending && <EmptyHint />}
          {turns.map((m, i) => (
            <Bubble key={i} role={m.role}>
              {m.content}
            </Bubble>
          ))}
          {pending && <PendingBubble turn={pending} />}
        </div>

        <form
          onSubmit={(e) => {
            e.preventDefault();
            void send();
          }}
          className="flex items-end gap-2 border-t border-line bg-surface/60 p-3"
        >
          <textarea
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                void send();
              }
            }}
            rows={1}
            disabled={streaming}
            placeholder={
              streaming ? "Waiting for the reply…" : "Ask about this run…"
            }
            className="max-h-32 w-full resize-y rounded-lg border border-line bg-surface px-4 py-2.5 text-sm leading-relaxed text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20 disabled:opacity-60"
          />
          <Button
            type="submit"
            loading={streaming}
            disabled={!draft.trim()}
            className="shrink-0"
          >
            Send
          </Button>
        </form>
      </Panel>

      {notice && <Callout tone={notice.tone}>{notice.message}</Callout>}
    </div>
  );
}

function Bubble({
  role,
  children,
}: {
  role: ChatMessage["role"];
  children: React.ReactNode;
}) {
  const mine = role === "user";
  return (
    <div className={cn("flex", mine ? "justify-end" : "justify-start")}>
      <div
        className={cn(
          "max-w-[82%] whitespace-pre-wrap break-words rounded-2xl px-4 py-2.5 text-sm leading-relaxed",
          mine
            ? "rounded-br-md bg-signal text-signal-ink"
            : "rounded-bl-md bg-surface-2 text-fg",
        )}
      >
        {children}
      </div>
    </div>
  );
}

function PendingBubble({ turn }: { turn: ChatTurn }) {
  return (
    <div className="flex flex-col items-start gap-2">
      {turn.activity.length > 0 && (
        <div className="flex flex-col gap-1 pl-1">
          {turn.activity.map((a, i) => (
            <ActivityRow key={i} activity={a} />
          ))}
        </div>
      )}
      <Bubble role="assistant">{turn.text || <TypingDots />}</Bubble>
    </div>
  );
}

function ActivityRow({ activity }: { activity: ToolActivity }) {
  return (
    <div className="flex items-center gap-2 font-mono text-[11px] text-faint">
      {activity.state === "running" ? (
        <Spinner className="h-2.5 w-2.5" />
      ) : (
        <span
          className={cn(
            "h-1.5 w-1.5 rounded-full",
            activity.state === "failed" ? "bg-neg" : "bg-signal",
          )}
        />
      )}
      <span>{activity.tool}</span>
      {activity.state === "failed" && <span className="text-neg">failed</span>}
    </div>
  );
}

function TypingDots() {
  return (
    <span className="flex items-center gap-1 py-1" aria-label="The agent is replying">
      {[0, 1, 2].map((i) => (
        <span
          key={i}
          className="pulse-live h-1.5 w-1.5 rounded-full bg-muted"
          style={{ animationDelay: `${i * 180}ms` }}
        />
      ))}
    </span>
  );
}

function EmptyHint() {
  return (
    <p className="m-auto max-w-sm text-center text-sm leading-relaxed text-faint">
      Ask what this run has learned. The agent answers only from the insights it
      can look up and the evidence behind them — it will say so plainly when
      nothing relevant has accumulated yet.
    </p>
  );
}
