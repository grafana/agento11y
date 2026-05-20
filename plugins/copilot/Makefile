.PHONY: build test lint install

build:
	go build -o sigil-copilot ./cmd/sigil-copilot

test:
	go test ./...

lint:
	golangci-lint run ./...

install:
	go install ./cmd/sigil-copilot
