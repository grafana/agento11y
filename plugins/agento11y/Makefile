.PHONY: build test lint install

build:
	GOWORK=off go build -o agento11y ./cmd/agento11y

test:
	GOWORK=off go test ./...

lint:
	golangci-lint run ./...

install:
	GOWORK=off go install ./cmd/agento11y
