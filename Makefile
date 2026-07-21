.PHONY: build test up down migrate-up migrate-down

# Host env for runs that talk to the compose stack from the host (not from
# inside the network): the published ports on localhost.
HOST_ENV = POSTGRES_HOST=localhost \
	NEO4J_URI=bolt://localhost:7687 \
	S3_ENDPOINT=http://localhost:9000 \
	OLLAMA_URL=http://localhost:11434

build:
	go build ./...

# Integration tests against a running `make up` stack. Sources .env for
# credentials and runs with -p 1 so packages do not race on the shared database.
test:
	set -a; . ./.env; set +a; \
	ARBORETTE_INTEGRATION=1 $(HOST_ENV) go test -p 1 ./...

up:
	docker compose up -d --build

down:
	docker compose down

migrate-up:
	set -a; . ./.env; set +a; $(HOST_ENV) go run ./cmd/migrate up

migrate-down:
	set -a; . ./.env; set +a; $(HOST_ENV) go run ./cmd/migrate down
