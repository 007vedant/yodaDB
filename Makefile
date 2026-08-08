GO ?= go

.PHONY: all build fmt fmt-check vet test test-race check bench

all: check

build:
	$(GO) build ./...

fmt:
	gofmt -w $$(find . -type f -name '*.go' -not -path './vendor/*')

fmt-check:
	@unformatted="$$(gofmt -l $$(find . -type f -name '*.go' -not -path './vendor/*'))"; \
	test -z "$$unformatted" || { \
		echo "Go files were not formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	}

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

check: fmt-check vet test test-race

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/bench
