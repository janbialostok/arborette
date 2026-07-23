# Multi-stage build for the web UI. The build context is ./web (self-contained,
# unlike the Go services which need the repo root for the shared go.mod), so COPY
# paths are conventional. output: 'standalone' (next.config.ts) emits a
# self-contained server.js run on a distroless Node runtime with only `node`.
FROM node:22-bookworm-slim AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

FROM gcr.io/distroless/nodejs22-debian12
WORKDIR /app
ENV NODE_ENV=production
# The standalone server binds to HOSTNAME; without 0.0.0.0 it can bind loopback
# and the published port mapping is unreachable from the host.
ENV HOSTNAME=0.0.0.0
ENV PORT=8083
# Standalone output omits static assets and public/, so copy them alongside it
# or every JS/CSS asset 404s.
COPY --from=build /app/.next/standalone ./
COPY --from=build /app/.next/static ./.next/static
COPY --from=build /app/public ./public
EXPOSE 8083
# The distroless nodejs image's entrypoint is `node`; standalone puts server.js
# at the workdir root (it omits the `next` CLI, so `next start` cannot run here).
CMD ["server.js"]
