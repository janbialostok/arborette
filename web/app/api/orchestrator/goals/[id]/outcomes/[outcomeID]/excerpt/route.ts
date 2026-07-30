import { forward } from "@/lib/proxy";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function GET(
  req: Request,
  { params }: { params: Promise<{ id: string; outcomeID: string }> },
): Promise<Response> {
  const { id, outcomeID } = await params;
  return forward(
    req,
    `/goals/${encodeURIComponent(id)}/outcomes/${encodeURIComponent(outcomeID)}/excerpt`,
  );
}
