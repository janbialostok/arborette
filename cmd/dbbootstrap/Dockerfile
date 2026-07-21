# Multi-stage build for the dbbootstrap binary. One Dockerfile per cmd/<service> keeps
# the services independently deployable images despite sharing one Go module.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/dbbootstrap ./cmd/dbbootstrap

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/dbbootstrap /dbbootstrap
ENTRYPOINT ["/dbbootstrap"]
