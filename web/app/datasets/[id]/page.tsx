import { DatasetDetail } from "@/components/DatasetDetail";

export default async function DatasetDetailPage({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = await params;
  return <DatasetDetail id={id} />;
}