// Package api — generate.go holds the go:generate directive that regenerates the committed OpenAPI 3.1
// spec (openapi.yaml at the repository root). The directive lives here because the api package's
// operation registrations are the canonical source of truth for the spec: running the generator reuses
// api.New to produce a byte-identical artifact on every machine.
//
// Run via:
//
//	go generate ./...
//	# or: make generate
package api

//go:generate go run github.com/qovira/qovira/cmd/gen-openapi
