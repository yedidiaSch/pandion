GO ?= go
BIN := bin/pandion

.PHONY: all test vet fmt fmtcheck build run clean ci

all: ci

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

fmtcheck:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi

build:
	$(GO) build -o $(BIN) ./cmd/pandion

run:
	$(GO) run ./cmd/pandion demo

clean:
	rm -rf bin

# what CI runs
ci: fmtcheck vet test build
