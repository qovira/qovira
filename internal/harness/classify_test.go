package harness

// classify_test.go — pure table-driven unit tests for the classify function. These are in package harness (not
// harness_test) to access the unexported classify function and fault enum. AC-3 from the error-classification issue.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/gateway"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	toolErr := &capability.ToolError{Code: "validation_failed", Message: "bad argument"}
	wrappedToolErr := fmt.Errorf("tool call failed: %w", toolErr)
	genericInfra := errors.New("database connection refused")
	panicWrapped := errors.New("recovered panic: something exploded")

	tests := []struct {
		name      string
		err       error
		wantFault turnFault
	}{
		{
			name:      "capability ToolError",
			err:       toolErr,
			wantFault: faultToolError,
		},
		{
			name:      "wrapped capability ToolError",
			err:       wrappedToolErr,
			wantFault: faultToolError,
		},
		{
			name:      "gateway ErrContextLength",
			err:       gateway.ErrContextLength,
			wantFault: faultContextLength,
		},
		{
			name:      "gateway ErrAuth",
			err:       gateway.ErrAuth,
			wantFault: faultInfrastructure,
		},
		{
			name:      "gateway ErrModelNotFound",
			err:       gateway.ErrModelNotFound,
			wantFault: faultInfrastructure,
		},
		{
			name:      "gateway ErrRateLimited",
			err:       gateway.ErrRateLimited,
			wantFault: faultInfrastructure,
		},
		{
			name:      "gateway RateLimitedError (wraps ErrRateLimited)",
			err:       &gateway.RateLimitedError{},
			wantFault: faultInfrastructure,
		},
		{
			name:      "gateway ErrUpstream",
			err:       gateway.ErrUpstream,
			wantFault: faultInfrastructure,
		},
		{
			name:      "gateway ErrTimeout",
			err:       gateway.ErrTimeout,
			wantFault: faultInfrastructure,
		},
		{
			name:      "gateway ErrUpstreamProtocol",
			err:       gateway.ErrUpstreamProtocol,
			wantFault: faultInfrastructure,
		},
		{
			name:      "gateway ErrGatewayNotConfigured",
			err:       gateway.ErrGatewayNotConfigured,
			wantFault: faultInfrastructure,
		},
		{
			name:      "generic infra error",
			err:       genericInfra,
			wantFault: faultInfrastructure,
		},
		{
			name:      "recovered panic (wrapped as error)",
			err:       panicWrapped,
			wantFault: faultInfrastructure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classify(tt.err)
			if got != tt.wantFault {
				t.Errorf("classify(%v) = %v, want %v", tt.err, got, tt.wantFault)
			}
		})
	}
}
