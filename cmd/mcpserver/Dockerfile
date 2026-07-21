# Multi-stage build for the mcpserver binary. One Dockerfile per cmd/<service> keeps
# the services independently deployable images despite sharing one Go module.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/mcpserver ./cmd/mcpserver

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/mcpserver /mcpserver
ENTRYPOINT ["/mcpserver"]
