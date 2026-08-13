import { forward, forwardSubmit } from "@/lib/proxy";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export function GET(req: Request): Promise<Response> {
  return forward(req, "/datasets");
}

export function POST(req: Request): Promise<Response> {
  return forwardSubmit(req, "/datasets");
}
