import { forward } from "@/lib/proxy";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Grant and revoke one collaborator by username. The orchestrator's verbatim
// refusal bodies (404 unknown user, 400 self-share, 409 duplicate, 403
// non-owner) relay through the proxy untouched.
export async function PUT(
  req: Request,
  { params }: { params: Promise<{ id: string; username: string }> },
): Promise<Response> {
  const { id, username } = await params;
  return forward(
    req,
    `/datasets/${encodeURIComponent(id)}/shares/${encodeURIComponent(username)}`,
  );
}

export async function DELETE(
  req: Request,
  { params }: { params: Promise<{ id: string; username: string }> },
): Promise<Response> {
  const { id, username } = await params;
  return forward(
    req,
    `/datasets/${encodeURIComponent(id)}/shares/${encodeURIComponent(username)}`,
  );
}