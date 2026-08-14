import { forward } from "@/lib/proxy";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Collection of the dataset's collaborator grants. The orchestrator answers
// 403 to a non-owner, so the list is owner-only end to end.
export async function GET(
  req: Request,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  const { id } = await params;
  return forward(req, `/datasets/${encodeURIComponent(id)}/shares`);
}