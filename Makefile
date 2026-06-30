GO ?= go
BIN := bin/envcore

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
	$(GO) build -o $(BIN) ./cmd/envcore

run:
	$(GO) run ./cmd/envcore demo

clean:
	rm -rf bin

# what CI runs
ci: fmtcheck vet test build
