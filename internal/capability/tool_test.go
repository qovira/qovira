package capability_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/store"
)

// ── RiskTier ─────────────────────────────────────────────────────────────────

// TestRiskTier_Order verifies that the four risk-tier constants are declared in the correct iota order.
func TestRiskTier_Order(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  capability.RiskTier
		want capability.RiskTier
	}{
		{"RiskRead is 0", capability.RiskRead, 0},
		{"RiskWrite is 1", capability.RiskWrite, 1},
		{"RiskExternal is 2", capability.RiskExternal, 2},
		{"RiskDestructive is 3", capability.RiskDestructive, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Errorf("got %d, want %d", tc.got, tc.want)
			}
		})
	}
}

// ── ToolError ─────────────────────────────────────────────────────────────────

// TestToolError_SatisfiesError verifies that *ToolError satisfies the error interface and that Error() returns a
// non-empty string.
func TestToolError_SatisfiesError(t *testing.T) {
	t.Parallel()

	var err error = &capability.ToolError{Code: "validation_failed", Message: "bad args"}
	if err.Error() == "" {
		t.Error("ToolError.Error() returned empty string")
	}
}

// TestToolError_ErrorsAs verifies that a *ToolError wrapped in fmt.Errorf can be matched with errors.As.
func TestToolError_ErrorsAs(t *testing.T) {
	t.Parallel()

	orig := &capability.ToolError{Code: "validation_failed", Message: "bad args"}
	wrapped := fmt.Errorf("wrapping: %w", orig)

	var target *capability.ToolError
	if !errors.As(wrapped, &target) {
		t.Error("errors.As did not match *ToolError through wrapping")
	}
	if target.Code != "validation_failed" {
		t.Errorf("Code = %q, want \"validation_failed\"", target.Code)
	}
}

// ── NewTool ───────────────────────────────────────────────────────────────────

// argsType is a sample typed argument struct used in NewTool tests.
type argsType struct {
	Value string `json:"value"`
}

// TestNewTool_MalformedArgs_ReturnsToolError verifies that when NewTool's Execute is called with JSON that cannot
// decode into Args, it returns a *ToolError (matchable via errors.As) and never panics or returns a raw decode error.
func TestNewTool_MalformedArgs_ReturnsToolError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args json.RawMessage
	}{
		{"not json", json.RawMessage(`not-valid-json`)},
		{"wrong type", json.RawMessage(`{"value": 42}`)},
		{"empty bytes", json.RawMessage(``)},
		{"array instead of object", json.RawMessage(`[1,2,3]`)},
	}

	tool := capability.NewTool(
		"test_tool",
		"a test tool",
		json.RawMessage(`{}`),
		capability.RiskRead,
		func(_ context.Context, _ store.Scope, _ argsType) (capability.Result, error) {
			t.Error("handler must not be called for malformed args")
			return struct{}{}, nil
		},
	)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scope := store.UserScope(store.Principal{UserID: "u1", Role: "user"})
			_, err := tool.Execute(context.Background(), scope, tc.args)
			if err == nil {
				t.Fatal("Execute returned nil error for malformed args; want *ToolError")
			}

			var toolErr *capability.ToolError
			if !errors.As(err, &toolErr) {
				t.Fatalf("errors.As(*ToolError) = false; err = %v (%T)", err, err)
			}
			if toolErr.Code == "" {
				t.Error("ToolError.Code is empty; want a stable slug")
			}
			if toolErr.Message == "" {
				t.Error("ToolError.Message is empty; want a model-safe message")
			}
		})
	}
}

// TestNewTool_WellFormedArgs_InvokesHandler verifies that when Execute is called with valid JSON that decodes into
// Args, the typed handler is called with the decoded value and its return values are propagated.
func TestNewTool_WellFormedArgs_InvokesHandler(t *testing.T) {
	t.Parallel()

	type result struct {
		ID string `json:"id"`
	}

	called := false
	var gotArgs argsType

	tool := capability.NewTool(
		"test_tool",
		"a test tool",
		json.RawMessage(`{}`),
		capability.RiskWrite,
		func(_ context.Context, _ store.Scope, args argsType) (capability.Result, error) {
			called = true
			gotArgs = args
			return result{ID: "123"}, nil
		},
	)

	scope := store.UserScope(store.Principal{UserID: "u1", Role: "user"})
	res, err := tool.Execute(context.Background(), scope, json.RawMessage(`{"value":"hello"}`))
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	if !called {
		t.Error("handler was not called")
	}
	if gotArgs.Value != "hello" {
		t.Errorf("decoded args.Value = %q, want \"hello\"", gotArgs.Value)
	}
	got, ok := res.(result)
	if !ok {
		t.Fatalf("result type = %T, want result", res)
	}
	if got.ID != "123" {
		t.Errorf("result.ID = %q, want \"123\"", got.ID)
	}
}

// TestNewTool_WellFormedArgs_HandlerError_Propagated verifies that a non-nil error returned by the handler is passed
// through as-is (not wrapped in *ToolError).
func TestNewTool_WellFormedArgs_HandlerError_Propagated(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("domain error")

	tool := capability.NewTool(
		"test_tool",
		"a test tool",
		json.RawMessage(`{}`),
		capability.RiskDestructive,
		func(_ context.Context, _ store.Scope, _ argsType) (capability.Result, error) {
			return nil, sentinel
		},
	)

	scope := store.UserScope(store.Principal{UserID: "u1", Role: "user"})
	_, err := tool.Execute(context.Background(), scope, json.RawMessage(`{"value":"x"}`))
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(sentinel) = false; err = %v", err)
	}
}

// TestNewTool_Fields_SetCorrectly verifies that NewTool wires the tool's metadata fields (Name, Description, Schema,
// Risk) from the constructor args.
func TestNewTool_Fields_SetCorrectly(t *testing.T) {
	t.Parallel()

	schema := json.RawMessage(`{"type":"object"}`)

	tool := capability.NewTool(
		"create_reminder",
		"Creates a reminder.",
		schema,
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ argsType) (capability.Result, error) {
			return struct{}{}, nil
		},
	)

	if tool.Name != "create_reminder" {
		t.Errorf("Name = %q, want \"create_reminder\"", tool.Name)
	}
	if tool.Description != "Creates a reminder." {
		t.Errorf("Description = %q, want \"Creates a reminder.\"", tool.Description)
	}
	if string(tool.Schema) != string(schema) {
		t.Errorf("Schema = %s, want %s", tool.Schema, schema)
	}
	if tool.Risk != capability.RiskExternal {
		t.Errorf("Risk = %d, want RiskExternal (%d)", tool.Risk, capability.RiskExternal)
	}
}

// TestNewTool_ScopePassedThrough verifies that the store.Scope received by Execute is forwarded unchanged to the
// handler.
func TestNewTool_ScopePassedThrough(t *testing.T) {
	t.Parallel()

	scope := store.UserScope(store.Principal{UserID: "u-xyz", Role: "user"})

	var gotScope store.Scope
	tool := capability.NewTool(
		"test_tool",
		"a test tool",
		json.RawMessage(`{}`),
		capability.RiskRead,
		func(_ context.Context, s store.Scope, _ argsType) (capability.Result, error) {
			gotScope = s
			return struct{}{}, nil
		},
	)

	_, err := tool.Execute(context.Background(), scope, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	if gotScope.UserID() != "u-xyz" {
		t.Errorf("handler received scope.UserID() = %q, want \"u-xyz\"", gotScope.UserID())
	}
}
