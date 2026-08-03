.PHONY: build test up down sleep-cycle discover migrate-up migrate-down web-dev web-build

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

# One-shot Sleep Cycle (the AWS Batch seam), compose profile "jobs" so `make up`
# does not start it -- `make up` instead runs the sleepcycle-serve service, which
# the orchestrator's HTTP launcher drives per goal. This target is for exercising
# the Batch path directly; --build keeps a stale image from running previous code:
#   make sleep-cycle GOAL=<optimization_function_id>
sleep-cycle:
	docker compose run --rm --build sleepcycle -goal $(GOAL)

# One-shot causal discovery (the AWS Batch seam), compose profile "jobs" so `make
# up` does not start it -- `make up` instead runs the verifier-serve service. This
# target exercises the Batch path directly:
#   make discover GOAL=<optimization_function_id> DATASOURCE=<data_source_ref>
discover:
	docker compose run --rm --build verifier -goal $(GOAL) -datasource $(DATASOURCE)

migrate-up:
	set -a; . ./.env; set +a; $(HOST_ENV) go run ./cmd/migrate up

migrate-down:
	set -a; . ./.env; set +a; $(HOST_ENV) go run ./cmd/migrate down

# Web UI (Node/Next.js, in ./web). web-dev runs the dev server against a running
# orchestrator; ORCHESTRATOR_URL defaults to the compose orchestrator's host port.
web-dev:
	cd web && ORCHESTRATOR_URL=$${ORCHESTRATOR_URL:-http://localhost:8080} npm run dev

web-build:
	cd web && npm ci && npm run build
