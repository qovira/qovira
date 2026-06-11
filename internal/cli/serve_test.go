// serve_test.go: openAndMigrate tests have moved to internal/app/app_test.go
// now that app.New owns the open+migrate step. This file is intentionally
// empty; the serve command surface (flags, RunE wiring) is exercised by the
// integration test in cli_test.go.
package cli
