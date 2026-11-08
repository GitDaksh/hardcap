# Hardcap's everyday commands. Run `make` on its own to list them.
#
# They expect Postgres on localhost:5432 and Redis on localhost:6379, the way
# Homebrew runs them (see the README). Any variable below can be changed on
# the command line, e.g. `make run ADMIN_TOKEN=a-longer-secret`.

DATABASE_URL      ?= postgres://localhost:5432/hardcap?sslmode=disable
TEST_DATABASE_URL ?= postgres://localhost:5432/hardcap_test?sslmode=disable
REDIS_URL         ?= redis://localhost:6379/0
TEST_REDIS_URL    ?= redis://localhost:6379/1
ADMIN_TOKEN       ?= dev-admin-token

.DEFAULT_GOAL := help
.PHONY: help setup run mock-backup stop-mock stampede test bench check build clean

help: ## List these commands
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  make %-12s %s\n", $$1, $$2}'

setup: ## Create the two local databases (safe to run again)
	@pg_isready -q || { \
		echo "Can't reach Postgres on this computer. On a Mac, start it with:"; \
		echo "  brew services start postgresql@18"; \
		echo "then wait a few seconds and run make setup again."; \
		exit 1; }
	@for db in hardcap hardcap_test; do \
		if psql -lqt | cut -d'|' -f1 | grep -qw $$db; then echo "database $$db: already there"; \
		else createdb $$db && echo "database $$db: created"; fi; \
	done

build: ## Build the binaries into bin/
	go build -o bin/ ./cmd/...

run: build ## Start the fake AI provider and Hardcap, with the dashboard on
	@echo
	@echo "  Dashboard:   http://localhost:8080/dashboard/"
	@echo "  Admin token: $(ADMIN_TOKEN)"
	@echo "  Stop with Ctrl+C"
	@echo
	@./bin/mockllm & MOCK=$$!; \
	trap 'kill $$MOCK 2>/dev/null' EXIT INT TERM; \
	HARDCAP_DATABASE_URL='$(DATABASE_URL)' HARDCAP_REDIS_URL='$(REDIS_URL)' \
	HARDCAP_ADMIN_TOKEN='$(ADMIN_TOKEN)' HARDCAP_DEMO=1 ./bin/hardcap

mock-backup: build ## Start a second fake provider on port 8091 (for the fallback demo)
	MOCK_ADDR=localhost:8091 ./bin/mockllm

stop-mock: ## Stop the main fake provider on port 8090, so requests fall back
	@pid=$$(lsof -ti tcp:8090 -sTCP:LISTEN); \
	if [ -n "$$pid" ]; then kill $$pid && echo "stopped the mock on port 8090"; else echo "nothing is listening on port 8090"; fi

stampede: build ## Send 200 clients at a naive and a Hardcap project, then audit both
	./bin/stampede -token '$(ADMIN_TOKEN)'

test: ## Run every test with the race detector (needs Postgres and Redis)
	HARDCAP_TEST_DATABASE_URL='$(TEST_DATABASE_URL)' HARDCAP_TEST_REDIS_URL='$(TEST_REDIS_URL)' \
	HARDCAP_REQUIRE_SERVICES=1 go test -race -count=1 ./...

bench: ## Measure the budget ledger: one project, 1 to 32 clients at once
	HARDCAP_TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test ./internal/ledger -run '^$$' -bench ReserveSettle -benchtime 3s

check: ## Check formatting and run go vet, as CI does
	@test -z "$$(gofmt -l .)" || { echo "These files need gofmt -w:"; gofmt -l .; exit 1; }
	go vet ./...

clean: ## Delete the built binaries
	rm -rf bin
