"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import {
  createDataset,
  errorMessage,
  listDatasets,
  type DatasetSummary,
} from "@/lib/orchestrator";
import { filterDatasets, isInUse, isSharedToMe } from "@/lib/datasets";
import { formatTimestamp, shortId } from "@/lib/format";
import {
  Badge,
  Button,
  Callout,
  cn,
  Panel,
  SectionLabel,
  Spinner,
  type BadgeTone,
} from "@/components/ui";

// DatasetsList is the inventory the header Datasets link lands on, plus the
// create form. The search box filters client-side (filterDatasets) so a
// keystroke never costs a round trip; creating a dataset lands on its detail.
export function DatasetsList() {
  const [datasets, setDatasets] = useState<DatasetSummary[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [q, setQ] = useState("");

  useEffect(() => {
    let cancelled = false;
    listDatasets()
      .then((rows) => {
        if (!cancelled) setDatasets(rows);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(errorMessage(err, "Could not load datasets. Please retry."));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const rows = datasets ? filterDatasets(datasets, q) : null;

  return (
    <div className="flex flex-col gap-8 pt-4">
      <div className="flex flex-col gap-3 border-b border-line pb-6">
        <SectionLabel>Datasets</SectionLabel>
        <h1 className="text-2xl font-semibold tracking-tight">
          Data source inventory
        </h1>
        <p className="max-w-xl text-sm leading-relaxed text-muted">
          The user-managed containers an objective&apos;s data source lives in.
          Create one around a file or import path, then bind goals to it. A
          dataset holding objectives cannot be removed until those objectives
          are gone.
        </p>
      </div>

      <DatasetCreateCard />

      {error && <Callout tone="error">{error}</Callout>}

      {!error && (
        <div className="flex flex-col gap-3">
          <div className="flex items-center justify-between gap-3">
            <SectionLabel>
              {rows && `${rows.length} dataset${rows.length === 1 ? "" : "s"}`}
            </SectionLabel>
            <input
              type="text"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="Filter by name…"
              aria-label="Filter datasets"
              className="w-56 rounded-lg border border-line bg-surface px-3 py-2 font-mono text-xs text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
            />
          </div>
          <List datasets={rows} loading={loading && !datasets} />
        </div>
      )}
    </div>
  );
}

function List({
  datasets,
  loading,
}: {
  datasets: DatasetSummary[] | null;
  loading: boolean;
}) {
  if (loading) {
    return (
      <Panel className="flex items-center justify-center px-6 py-16 text-sm text-muted">
        <Spinner className="mr-2" /> Loading datasets…
      </Panel>
    );
  }
  if (!datasets || datasets.length === 0) {
    return (
      <Panel className="px-6 py-16 text-center text-sm text-faint">
        No datasets yet. Create one above, or they are minted automatically when
        a goal is registered against a fresh data source.
      </Panel>
    );
  }
  return (
    <ul className="flex flex-col gap-2">
      {datasets.map((d) => (
        <li key={d.id}>
          <DatasetRow dataset={d} />
        </li>
      ))}
    </ul>
  );
}

function DatasetRow({ dataset }: { dataset: DatasetSummary }) {
  const tone: Record<string, BadgeTone> = {
    active: "positive",
    archived: "neutral",
  };
  return (
    <Link
      href={`/datasets/${dataset.id}`}
      className="flex flex-col gap-3 rounded-lg border border-line bg-surface/60 px-4 py-3.5 transition-colors hover:border-line-strong"
    >
      <div className="flex items-start justify-between gap-4">
        <div className="flex min-w-0 flex-col gap-1">
          <span className="truncate text-sm font-medium text-fg">
            {dataset.name}
          </span>
          <span className="truncate font-mono text-[11px] text-faint">
            {shortId(dataset.id)} · {dataset.data_source_ref}
          </span>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          {isSharedToMe(dataset) && (
            <Badge tone="positive">shared with you</Badge>
          )}
          {isInUse(dataset) && (
            <Badge tone="neutral">
              {dataset.objective_count} objective
              {dataset.objective_count === 1 ? "" : "s"}
            </Badge>
          )}
          <Badge tone={tone[dataset.status] ?? "neutral"}>{dataset.status}</Badge>
        </div>
      </div>
      <span className="font-mono text-[11px] text-faint">
        {formatTimestamp(dataset.created_at)}
      </span>
    </Link>
  );
}

type Source = "file" | "path";

// DatasetCreateCard is the inventory's create form, mirroring GoalForm's
// exactly-one-source ingest. Creates clear to the new dataset's detail.
function DatasetCreateCard() {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
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
      setError("Give the dataset a name before creating it.");
      return;
    }
    const form = new FormData();
    form.set("name", name.trim());
    if (description.trim()) form.set("description", description.trim());
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
      const created = await createDataset(form);
      router.push(`/datasets/${created.id}`);
    } catch (err) {
      setError(errorMessage(err, "Could not create the dataset. Please retry."));
      setPending(false);
    }
  }

  return (
    <Panel className="p-5">
      {open ? (
        <form onSubmit={onSubmit} className="flex flex-col gap-5">
          <div className="flex items-center justify-between">
            <SectionLabel>New dataset</SectionLabel>
            <button
              type="button"
              onClick={() => setOpen(false)}
              className="text-xs text-muted transition-colors hover:text-fg"
            >
              Cancel
            </button>
          </div>

          <div className="flex flex-col gap-2">
            <label htmlFor="dataset-name" className="text-sm text-muted">
              Name
            </label>
            <input
              id="dataset-name"
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. sales-2024"
              className="w-full rounded-lg border border-line bg-surface px-4 py-3 font-mono text-sm text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
            />
          </div>

          <div className="flex flex-col gap-2">
            <label htmlFor="dataset-description" className="text-sm text-muted">
              Description{" "}
              <span className="text-faint">(optional)</span>
            </label>
            <textarea
              id="dataset-description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={2}
              placeholder="What this data source is for"
              className="w-full resize-y rounded-lg border border-line bg-surface px-4 py-3 text-sm leading-relaxed text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
            />
          </div>

          <div className="flex flex-col gap-3">
            <SectionLabel>Data source</SectionLabel>
            <div className="flex gap-1 rounded-lg border border-line bg-surface p-1">
              <SourceTab active={source === "file"} onClick={() => setSource("file")}>
                File upload
              </SourceTab>
              <SourceTab active={source === "path"} onClick={() => setSource("path")}>
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
                  className="flex items-center justify-between rounded-lg border border-dashed border-line-strong bg-surface px-4 py-3 text-left text-sm transition-colors hover:border-signal/50"
                >
                  <span className={file ? "text-fg" : "text-faint"}>
                    {file ? file.name : "Choose a CSV or Parquet file…"}
                  </span>
                  <span className="font-mono text-[11px] uppercase tracking-wider text-faint">
                    browse
                  </span>
                </button>
              </div>
            ) : (
              <input
                type="text"
                value={importPath}
                onChange={(e) => setImportPath(e.target.value)}
                placeholder="contracts/acme-2024.csv"
                className="w-full rounded-lg border border-line bg-surface px-4 py-3.5 font-mono text-sm text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
              />
            )}
          </div>

          {error && <Callout tone="error">{error}</Callout>}

          <Button type="submit" loading={pending} disabled={!canSubmit}>
            {pending ? "Ingesting source…" : "Create dataset"}
          </Button>
        </form>
      ) : (
        <button
          type="button"
          onClick={() => setOpen(true)}
          className="flex w-full items-center justify-between text-left"
        >
          <span className="flex items-center gap-2 text-sm text-muted">
            <span className="font-mono text-signal">+</span> New dataset
          </span>
          <span className="font-mono text-[11px] uppercase tracking-wider text-faint">
            ingest a source
          </span>
        </button>
      )}
    </Panel>
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