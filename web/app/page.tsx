import { GoalForm } from "@/components/GoalForm";
import { SectionLabel } from "@/components/ui";

export default function SubmitDatasetPage() {
  return (
    <div className="grid gap-14 pt-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)] lg:gap-16 lg:pt-16">
      <section className="flex flex-col justify-center">
        <SectionLabel>New dataset</SectionLabel>
        <h1 className="mt-5 text-balance text-4xl font-semibold leading-[1.05] tracking-tight md:text-5xl">
          Ask a question about{" "}
          <span className="text-signal">your data.</span>
        </h1>
        <p className="mt-6 max-w-md text-pretty text-base leading-relaxed text-muted">
          Give your dataset a name, describe what you want to learn, and point
          the system at a data source. It will explore candidate segments, measure
          their effects, and surface the insights that survive scrutiny.
        </p>
        <ol className="mt-10 flex flex-col gap-4 border-l border-line pl-5">
          {[
            "Name your dataset and ask a question",
            "Watch the hypothesis loop stream live effects",
            "Browse the insights it discovers",
          ].map((step, i) => (
            <li key={step} className="flex items-baseline gap-3 text-sm">
              <span className="font-mono text-xs text-signal tabular">
                0{i + 1}
              </span>
              <span className="text-muted">{step}</span>
            </li>
          ))}
        </ol>
      </section>

      <section className="lg:pt-4">
        <GoalForm />
      </section>
    </div>
  );
}
