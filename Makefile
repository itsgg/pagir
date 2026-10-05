# Everything CI runs, in one command, before you push.
.PHONY: check build test vet fmt install e2e clean

BIN := $(HOME)/.local/bin/pagir

check: fmt vet test
	@echo "all checks passed"

build:
	go build -o pagir .

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt wants: $$out"; exit 1; fi

install:
	go build -o $(BIN) .

# Live shares through this machine's tailscaled; never in CI.
e2e: build
	scripts/e2e.sh ./pagir

clean:
	rm -f pagir
