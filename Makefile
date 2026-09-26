.PHONY: run test lint demo docker clean

run: ## Start the API on :8080 (data in ./data)
	go run ./cmd/server

test: ## Unit, golden and end-to-end HTTP tests with the race detector
	go test -race -count=1 ./...

lint: ## gofmt + go vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...

demo: ## Exercise every endpoint against a running server (needs curl + jq)
	./scripts/demo.sh

docker: ## Build and run in Docker on :8080
	docker build -t receipt-autoitemize . && docker run --rm -p 8080:8080 receipt-autoitemize

clean:
	rm -rf data
