import type { NextConfig } from "next";

// output: 'standalone' emits a self-contained .next/standalone/server.js so the
// Docker runtime needs only `node` — no `next` CLI. See web/Dockerfile.
const nextConfig: NextConfig = {
  output: "standalone",
  // Baseline security response headers. These govern framing, MIME sniffing,
  // and referrer leakage without constraining what the page may execute, so
  // they don't interfere with App Router hydration/streaming. DENY framing
  // closes clickjacking on the single-click, cost-incurring run/sleep-cycle
  // actions. A content CSP is deliberately omitted — a strict one needs
  // per-request nonce middleware (App Router injects inline bootstrap scripts).
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Frame-Options", value: "DENY" },
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "Referrer-Policy", value: "no-referrer" },
        ],
      },
    ];
  },
};

export default nextConfig;
