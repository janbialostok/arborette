import { forward } from "@/lib/proxy";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// The dispatch answers 202 as soon as the Verifier accepts the job — the run
// itself reports through the goal's stream — so this needs no submit cap.
export async function POST(
  req: Request,
  { params }: { params: Promise<{ id: string; interventionID: string }> },
): Promise<Response> {
  const { id, interventionID } = await params;
  return forward(
    req,
    `/goals/${encodeURIComponent(id)}/findings/${encodeURIComponent(interventionID)}/verify`,
  );
}
