.PHONY: build test lint install

build:
	GOWORK=off go build -o sigil ./cmd/sigil

test:
	GOWORK=off go test ./...

lint:
	golangci-lint run ./...

install:
	GOWORK=off go install ./cmd/sigil
