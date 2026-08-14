import { forward, forwardSubmit } from "@/lib/proxy";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export function GET(req: Request): Promise<Response> {
  return forward(req, "/me");
}

export function PATCH(req: Request): Promise<Response> {
  return forwardSubmit(req, "/me");
}