import { forwardSubmit } from "@/lib/proxy";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function PATCH(
  req: Request,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  const { id } = await params;
  return forwardSubmit(req, `/users/${encodeURIComponent(id)}`);
}