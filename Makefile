CGO_ENABLED ?= 1
BINARY      := qovira
GOFLAGS     := -trimpath

.PHONY: build test race lint clean

build:
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -o $(BINARY) ./cmd/qovira

test:
	CGO_ENABLED=$(CGO_ENABLED) go test ./...

race:
	CGO_ENABLED=$(CGO_ENABLED) go test -race ./...

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY)
