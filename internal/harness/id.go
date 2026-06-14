package harness

import "github.com/qovira/qovira/internal/id"

// generateID returns a new monotonic ULID string using the shared package-level generator.
func generateID() string { return id.New() }
