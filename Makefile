.PHONY: run run-memory run-async test lint demo docker clean

run: ## Start the API on :8080 (data in ./data)
	go run ./cmd/server

run-memory: ## Same API on the in-memory repository (no database file)
	STORE=memory go run ./cmd/server

run-async: ## Same, but process returns 202 and a goroutine worker pool runs OCR
	PROCESS_MODE=async go run ./cmd/server

test: ## Unit, golden, repository contract and HTTP tests (-race); HTTP suite on both backends
	go test -race -count=1 ./...
	TEST_STORE=memory go test -race -count=1 ./internal/api/

lint: ## gofmt + go vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...

demo: ## Exercise every endpoint against a running server (needs curl + jq)
	./scripts/demo.sh

docker: ## Build and run in Docker on :8080
	docker build -t receipt-autoitemize . && docker run --rm -p 8080:8080 receipt-autoitemize

clean:
	rm -rf data
