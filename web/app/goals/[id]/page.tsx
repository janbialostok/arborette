import { LiveRun } from "@/components/LiveRun";

export default async function GoalRunPage({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = await params;
  return <LiveRun id={id} />;
}
