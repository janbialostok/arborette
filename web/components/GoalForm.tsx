"use client";

import { useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { errorMessage, submitGoal } from "@/lib/orchestrator";
import { Button, Callout, cn, SectionLabel } from "@/components/ui";

type Source = "file" | "path";

export function GoalForm() {
  const router = useRouter();
  const [name, setName] = useState("");
  const [goal, setGoal] = useState("");
  const [source, setSource] = useState<Source>("file");
  const [file, setFile] = useState<File | null>(null);
  const [importPath, setImportPath] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const canSubmit =
    name.trim().length > 0 &&
    (source === "file" ? file != null : importPath.trim().length > 0);

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);

    if (!name.trim()) {
      setError("Enter a name for the dataset.");
      return;
    }
    if (!goal.trim()) {
      setError("Describe the question before submitting.");
      return;
    }
    // Enforce exactly-one-source client-side to avoid the both/neither 400s.
    const form = new FormData();
    // The backend stores `goal` form field as goal_text (the dataset title).
    // Send the name as the primary identifier; include the question as well
    // so Claude can generate the evaluation matrix from both.
    form.set("goal", `${name.trim()}\n\n${goal.trim()}`);
    if (source === "file") {
      if (!file) {
        setError("Choose a file to upload, or switch to an on-disk path.");
        return;
      }
      form.set("file", file);
    } else {
      if (!importPath.trim()) {
        setError("Enter a relative path under the import mount.");
        return;
      }
      form.set("import_path", importPath.trim());
    }

    setPending(true);
    try {
      const { optimization_function_id } = await submitGoal(form);
      router.push(`/datasets/${optimization_function_id}`);
    } catch (err) {
      // The orchestrator's {error} body is analyst-safe — surface it verbatim.
      setError(
        errorMessage(err, "Something went wrong submitting the dataset. Please retry."),
      );
      setPending(false);
    }
  }

  return (
    <form onSubmit={onSubmit} className="flex flex-col gap-7">
      <div className="flex flex-col gap-2.5">
        <label htmlFor="name" className="flex items-center justify-between">
          <SectionLabel>Dataset name</SectionLabel>
          <span className="font-mono text-[11px] text-faint">
            short label
          </span>
        </label>
        <input
          id="name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. Transaction Fraud — July 2026"
          className="w-full rounded-lg border border-line bg-surface px-4 py-3 text-[15px] text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
        />
      </div>
      <div className="flex flex-col gap-2.5">
        <label htmlFor="goal" className="flex items-center justify-between">
          <SectionLabel>Question</SectionLabel>
          <span className="font-mono text-[11px] text-faint">
            natural language
          </span>
        </label>
        <textarea
          id="goal"
          value={goal}
          onChange={(e) => setGoal(e.target.value)}
          rows={4}
          placeholder="e.g. Which segments of repeat customers in the Northeast region have the highest average order value?"
          className="w-full resize-y rounded-lg border border-line bg-surface px-4 py-3 text-[15px] leading-relaxed text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
        />
        <p className="text-xs text-faint">
          What would you like to learn from this dataset?
        </p>
      </div>

      <div className="flex flex-col gap-3">
        <SectionLabel>Data source</SectionLabel>
        <div className="flex gap-1 rounded-lg border border-line bg-surface p-1">
          <SourceTab
            active={source === "file"}
            onClick={() => setSource("file")}
          >
            File upload
          </SourceTab>
          <SourceTab
            active={source === "path"}
            onClick={() => setSource("path")}
          >
            On-disk path
          </SourceTab>
        </div>

        {source === "file" ? (
          <div className="flex flex-col gap-2">
            <input
              ref={fileInputRef}
              type="file"
              className="hidden"
              onChange={(e) => setFile(e.target.files?.[0] ?? null)}
            />
            <button
              type="button"
              onClick={() => fileInputRef.current?.click()}
              className="flex items-center justify-between rounded-lg border border-dashed border-line-strong bg-surface px-4 py-3.5 text-left text-sm transition-colors hover:border-signal/50"
            >
              <span className={file ? "text-fg" : "text-faint"}>
                {file ? file.name : "Choose a CSV or Parquet file…"}
              </span>
              <span className="font-mono text-[11px] uppercase tracking-wider text-faint">
                {file ? formatBytes(file.size) : "browse"}
              </span>
            </button>
            <p className="text-xs text-faint">
              Uploaded and streamed to the object store. Max 512 MiB.
            </p>
          </div>
        ) : (
          <div className="flex flex-col gap-2">
            <input
              type="text"
              value={importPath}
              onChange={(e) => setImportPath(e.target.value)}
              placeholder="contracts/acme-2024.csv"
              className="w-full rounded-lg border border-line bg-surface px-4 py-3.5 font-mono text-sm text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
            />
            <p className="text-xs text-faint">
              Relative path resolved under the server&apos;s read-only import
              mount. Absolute paths and <span className="font-mono">..</span>{" "}
              escapes are rejected.
            </p>
          </div>
        )}
      </div>

      {error && <Callout tone="error">{error}</Callout>}

      <div className="flex items-center gap-4">
        <Button type="submit" loading={pending} disabled={!canSubmit}>
          {pending ? "Registering dataset…" : "Register dataset"}
        </Button>
        {pending && (
          <span className="text-xs text-faint">
            Generating the evaluation matrix — this calls Claude and can take a
            moment.
          </span>
        )}
      </div>
    </form>
  );
}

function SourceTab({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={cn(
        "flex-1 rounded-md px-3 py-2 text-sm font-medium transition-colors",
        active
          ? "bg-surface-2 text-signal shadow-[0_0_0_1px_rgba(123,241,168,0.25)]"
          : "text-muted hover:text-fg",
      )}
    >
      {children}
    </button>
  );
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(value < 10 ? 1 : 0)} ${units[unit]}`;
}
