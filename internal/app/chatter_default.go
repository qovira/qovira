//go:build !e2e

package app

// chatter_default.go — Chatter selection for the default (production) build.
//
// In the default binary the Chatter is always a *gateway.Gateway.  This file
// is compiled when the e2e build tag is absent; chatter_e2e.go (//go:build e2e)
// provides the alternative implementation for E2E test builds.

import (
	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/store"
)

// newChatter returns the production Chatter: a *gateway.Gateway backed by the
// store's settings store.  The settings store is the live, read-through source
// for model gateway configuration.
func newChatter(ss *store.SettingsStore) harness.Chatter {
	return gateway.New(ss)
}
