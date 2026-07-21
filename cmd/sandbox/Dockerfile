# Multi-stage build for the sandbox binary. One Dockerfile per cmd/<service> keeps
# the services independently deployable images despite sharing one Go module.
# Unlike the other services this one is a CGO build: the DuckDB driver bundles a
# static C++ archive, so the linked binary dynamically needs libstdc++/libgcc_s
# (plus glibc) at runtime -- the distroless static/base images lack the C++
# runtime and fail at startup, so the runtime base is cc-debian12. The builder is
# pinned to -bookworm to match cc-debian12's glibc: the default golang:1.24 tag is
# Debian trixie, whose newer glibc links symbols bookworm cannot resolve, so the
# binary would fail at startup with a GLIBC_2.3x-not-found loader error.
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o /out/sandbox ./cmd/sandbox

FROM gcr.io/distroless/cc-debian12
COPY --from=build /out/sandbox /sandbox
ENTRYPOINT ["/sandbox"]
