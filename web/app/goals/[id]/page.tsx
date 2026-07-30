import { GoalTabs } from "@/components/GoalTabs";

export default async function GoalRunPage({
  params,
  searchParams,
}: {
  params: Promise<{ id: string }>;
  searchParams: Promise<{ [key: string]: string | string[] | undefined }>;
}) {
  const { id } = await params;
  const { tab } = await searchParams;
  return <GoalTabs id={id} initialTab={Array.isArray(tab) ? tab[0] : tab} />;
}
