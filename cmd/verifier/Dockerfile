# Multi-stage build for the verifier binary. One Dockerfile per cmd/<service> keeps
# the services independently deployable images despite sharing one Go module. The
# verifier is CGO-free (consumes internal/sandboxclient, never internal/sandbox), so
# it builds static like the other non-sandbox services.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/verifier ./cmd/verifier

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/verifier /verifier
ENTRYPOINT ["/verifier"]
