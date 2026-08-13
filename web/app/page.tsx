import { GoalForm } from "@/components/GoalForm";
import { SectionLabel } from "@/components/ui";

export default function SubmitGoalPage() {
  return (
    <div className="grid gap-14 pt-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)] lg:gap-16 lg:pt-16">
      <section className="flex flex-col justify-center">
        <SectionLabel>Active hypothesis loop</SectionLabel>
        <h1 className="mt-5 text-balance text-4xl font-semibold leading-[1.05] tracking-tight md:text-5xl">
          Turn an analyst goal into{" "}
          <span className="text-signal">causal knowledge.</span>
        </h1>
        <p className="mt-6 max-w-md text-pretty text-base leading-relaxed text-muted">
          Register a goal against a data source. Arborette translates it into an
          evaluation matrix, then explores a tree of interventions — measuring
          the effect of each and recording it as a causal triplet.
        </p>
        <ol className="mt-10 flex flex-col gap-4 border-l border-line pl-5">
          {[
            "Register a goal and its data source",
            "Watch the hypothesis loop stream live effects",
            "Browse the heuristics it distills",
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
