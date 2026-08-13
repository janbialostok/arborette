"use client";

import { useEffect, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import {
  errorMessage,
  listDatasets,
  submitGoal,
  type DatasetSummary,
} from "@/lib/orchestrator";
import { Button, Callout, cn, SectionLabel } from "@/components/ui";

type Source = "file" | "path";

export function GoalForm() {
  const router = useRouter();
  const [goal, setGoal] = useState("");
  const [source, setSource] = useState<Source>("file");
  const [file, setFile] = useState<File | null>(null);
  const [importPath, setImportPath] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // The dataset picker: empty means "new data source" (the legacy upload/path
  // ingest, which mints an implicit dataset). Selecting a dataset binds the goal
  // to its already-ingested source and sends no upload.
  const [datasets, setDatasets] = useState<DatasetSummary[]>([]);
  const [datasetID, setDatasetID] = useState("");

  useEffect(() => {
    let cancelled = false;
    listDatasets()
      .then((rows) => {
        // Only active datasets can accept new objectives; archived ones are
        // gate-409ed by the backend, so the picker omits them up front.
        if (!cancelled) setDatasets(rows.filter((d) => d.status === "active"));
      })
      .catch(() => {
        // A failed inventory load falls back to upload-only submission; the
        // upload path needs no dataset list.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const bound = datasetID !== "";
  const canSubmit =
    goal.trim().length > 0 &&
    (bound || (source === "file" ? file != null : importPath.trim().length > 0));

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);

    if (!goal.trim()) {
      setError("Describe the goal before submitting.");
      return;
    }
    // Enforce exactly-one-source client-side: a bound goal sends dataset_id and
    // nothing else; an unbound one sends exactly one of the file/path fields.
    const form = new FormData();
    form.set("goal", goal.trim());
    if (bound) {
      form.set("dataset_id", datasetID);
    } else if (source === "file") {
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
      // Submitting a goal does not start the hypothesis loop — that's the
      // separate "Run Phase 1" action on the live view — so the goal is left
      // un-triggered. Its live view resolves to the neutral "not yet started"
      // state prompting Phase 1, rather than waiting for a run that isn't going.
      router.push(`/goals/${optimization_function_id}`);
    } catch (err) {
      // The orchestrator's {error} body is analyst-safe — surface it verbatim.
      setError(
        errorMessage(err, "Something went wrong submitting the goal. Please retry."),
      );
      setPending(false);
    }
  }

  return (
    <form onSubmit={onSubmit} className="flex flex-col gap-7">
      <div className="flex flex-col gap-2.5">
        <label htmlFor="goal" className="flex items-center justify-between">
          <SectionLabel>Optimization goal</SectionLabel>
          <span className="font-mono text-[11px] text-faint">
            natural language
          </span>
        </label>
        <textarea
          id="goal"
          value={goal}
          onChange={(e) => setGoal(e.target.value)}
          rows={4}
          placeholder="e.g. Maximize average order value for repeat customers in the Northeast region."
          className="w-full resize-y rounded-lg border border-line bg-surface px-4 py-3 text-[15px] leading-relaxed text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
        />
      </div>

      <div className="flex flex-col gap-3">
        <SectionLabel>Data source</SectionLabel>
        <select
          value={datasetID}
          onChange={(e) => setDatasetID(e.target.value)}
          aria-label="Data source dataset"
          className="w-full rounded-lg border border-line bg-surface px-4 py-3 text-sm text-fg outline-hidden transition-colors focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
        >
          <option value="">
            New data source — upload a file or import a path
          </option>
          {datasets.map((d) => (
            <option key={d.id} value={d.id}>
              {d.name}
            </option>
          ))}
        </select>

        {bound ? (
          <div className="flex flex-col gap-2 rounded-lg border border-signal/25 bg-surface px-4 py-3">
            <p className="text-sm text-fg">
              Binding this goal to the selected dataset&apos;s data source.
            </p>
            <p className="text-xs leading-relaxed text-muted">
              No upload needed — the objective fits against the dataset&apos;s
              already-ingested data. Pick a new data source to upload instead.
            </p>
          </div>
        ) : (
          <>
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
          </>
        )}
      </div>

      {error && <Callout tone="error">{error}</Callout>}

      <div className="flex items-center gap-4">
        <Button type="submit" loading={pending} disabled={!canSubmit}>
          {pending ? "Registering goal…" : "Register goal"}
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
