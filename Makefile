VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/Goutam-11/repath/internal/cli.version=$(VERSION)

.PHONY: build test vet fmt check clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/repath ./cmd/repath

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check:
	test -z "$$(gofmt -l .)" || (echo "unformatted files above; run 'make fmt'"; exit 1)
	go vet ./...
	go test ./...

clean:
	rm -rf bin
