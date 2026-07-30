import { forwardSubmit } from "@/lib/proxy";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// A resolution re-reads the source document through the sandbox to relocate a
// corrected value, so it is given the generous submit cap.
export async function POST(
  req: Request,
  { params }: { params: Promise<{ id: string; outcomeID: string }> },
): Promise<Response> {
  const { id, outcomeID } = await params;
  return forwardSubmit(
    req,
    `/goals/${encodeURIComponent(id)}/verifications/${encodeURIComponent(outcomeID)}`,
  );
}
