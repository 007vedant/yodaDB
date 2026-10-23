GO ?= go

.PHONY: all build fmt fmt-check vet test test-race check model-test-extended bench

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

model-test-extended:
	YODADB_EXTENDED_MODEL=1 $(GO) test ./storage/btree -run '^TestBTreeModelExtended$$' -count=1 -timeout=30m

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/bench
