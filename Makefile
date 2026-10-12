BIN      := whyslow
PKG      := ./cmd/whyslow
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

export CGO_ENABLED ?= 0

.PHONY: all build test race lint integration ebpf-test run clean

all: lint test build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

test:
	go test ./...

race:
	CGO_ENABLED=1 go test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...
	staticcheck ./...

integration:
	go test -tags integration -count=1 -v ./test/integration/

# The eBPF test loads programs into the kernel, so it needs root.
# The test binary is built as your user and only executed with sudo.
ebpf-test:
	go test -c -tags ebpf -o netbpf.test ./internal/netbpf/
	sudo ./netbpf.test -test.v -test.count=1
	rm -f netbpf.test

run: build
	./$(BIN) run

clean:
	rm -f $(BIN) netbpf.test coverage.out
