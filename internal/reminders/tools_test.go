package reminders_test

// tools_test.go — TDD tests for Module.Tools() (slice 5: AI tool adapters).
//
// Acceptance criteria (each independently verified):
//  1. Tool-vs-REST equivalence: create via tool and via REST produce the same row, same reminder.created event,
//     and same fire-job EnqueueRequest.
//  2. update/complete/delete route to Service methods and emit the same events.
//  3. Risk tiers: create/update/complete = RiskWrite, delete = RiskDestructive.
//  4. Validation errors (bad dueAt/rrule/tz) surface as *capability.ToolError.
//  5. Five tools are registered on the capability registry (four write/destructive from slice 5 + list_reminders
//     RiskRead from slice 6).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/reminders"
	"github.com/qovira/qovira/internal/store"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// toolByName returns the named tool from the slice or fails the test.
func toolByName(t *testing.T, tools []capability.Tool, name string) capability.Tool {
	t.Helper()
	for _, tt := range tools {
		if tt.Name == name {
			return tt
		}
	}
	t.Fatalf("tool %q not found in Tools() slice", name)
	return capability.Tool{}
}

// newToolScope builds a store.Scope for the given user ID.
func newToolScope(userID string) store.Scope {
	return store.UserScope(store.Principal{UserID: userID, Role: "member"})
}

// encodeArgs marshals v to JSON and fails the test on error.
func encodeArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encodeArgs: %v", err)
	}
	return b
}

// ── AC5: Tools() returns five tools registered on the registry ───────────────

// TestTools_ReturnsFiveTools verifies that Module.Tools() returns exactly five tools: the four write/destructive tools
// and the new list_reminders read tool.
func TestTools_ReturnsFiveTools(t *testing.T) {
	t.Parallel()

	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-tools-01"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	tools := m.Tools()
	if len(tools) != 5 {
		t.Fatalf("Tools() returned %d tools, want 5", len(tools))
	}

	wantNames := []string{
		"create_reminder", "update_reminder", "complete_reminder",
		"delete_reminder", "list_reminders",
	}
	got := make(map[string]bool, len(tools))
	for _, tt := range tools {
		got[tt.Name] = true
	}
	for _, name := range wantNames {
		if !got[name] {
			t.Errorf("tool %q missing from Tools()", name)
		}
	}
}

// TestTools_RegisteredOnCapabilityRegistry verifies that all five tools appear in the capability registry after
// reg.Add(module).
func TestTools_RegisteredOnCapabilityRegistry(t *testing.T) {
	t.Parallel()

	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-reg-01"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	reg := capability.NewRegistry()
	if err := reg.Add(m); err != nil {
		t.Fatalf("reg.Add(remindersModule): %v", err)
	}

	catalog := reg.Catalog()
	if len(catalog) != 5 {
		t.Fatalf("registry catalog has %d tools, want 5", len(catalog))
	}

	wantNames := []string{
		"create_reminder", "update_reminder", "complete_reminder",
		"delete_reminder", "list_reminders",
	}
	got := make(map[string]bool, len(catalog))
	for _, tt := range catalog {
		got[tt.Name] = true
	}
	for _, name := range wantNames {
		if !got[name] {
			t.Errorf("tool %q missing from registry catalog", name)
		}
	}
}

// ── AC3: Risk tiers ───────────────────────────────────────────────────────────

// TestTools_RiskTiers verifies each tool declares its correct risk tier.
func TestTools_RiskTiers(t *testing.T) {
	t.Parallel()

	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-risk-01"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	tools := m.Tools()

	cases := []struct {
		name string
		want capability.RiskTier
	}{
		{"create_reminder", capability.RiskWrite},
		{"update_reminder", capability.RiskWrite},
		{"complete_reminder", capability.RiskWrite},
		{"delete_reminder", capability.RiskDestructive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tt := toolByName(t, tools, tc.name)
			if tt.Risk != tc.want {
				t.Errorf("%s: Risk = %d, want %d", tc.name, tt.Risk, tc.want)
			}
		})
	}
}

// TestTools_HaveSchemas verifies that each tool has a non-empty JSON Schema.
func TestTools_HaveSchemas(t *testing.T) {
	t.Parallel()

	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-schema-01"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	for _, tt := range m.Tools() {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()
			if len(tt.Schema) == 0 {
				t.Errorf("tool %q has empty Schema", tt.Name)
			}
			// Must be valid JSON.
			var schema map[string]any
			if err := json.Unmarshal(tt.Schema, &schema); err != nil {
				t.Errorf("tool %q schema is not valid JSON: %v", tt.Name, err)
			}
		})
	}
}

// ── AC1: create_reminder tool-vs-REST equivalence ────────────────────────────

// TestTool_CreateReminder_SameAsRESTCreate verifies that calling the create_reminder tool produces the same persisted
// row, same reminder.created event type, and same fire-job EnqueueRequest shape as POST /api/v1/reminders.
//
// "Same shape" means: identical field values on the persisted reminder (title, notes, dueAt, tz, autoComplete,
// status), same event Type, same EnqueueRequest Kind/Key/RunAt/Recurrence. The IDs naturally differ between the two
// calls.
func TestTool_CreateReminder_SameAsRESTCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-create-01"
	seedUser(t, st, userID, "America/New_York")
	scope := newToolScope(userID)

	// ── REST path ────────────────────────────────────────────────────────────

	restProd := &fakeProducer{returnID: "job-rest-equiv-01"}
	restBus := &fakePublisher{}
	restMod, _ := newTestModule(t, st, restProd, restBus)
	restSvc := restMod.Service()

	dueAt := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	dueAtStr := dueAt.Format(time.RFC3339)

	restResult, err := restSvc.Create(ctx, scope, reminders.CreateInput{
		Title:        "Grocery run",
		Notes:        "Milk, eggs, bread",
		DueAt:        dueAt,
		AutoComplete: ptrTrue,
	})
	if err != nil {
		t.Fatalf("Service.Create (REST path): %v", err)
	}

	restEvents := restBus.allEvents()
	restEnqueued := restProd.allEnqueued()
	if len(restEvents) == 0 || len(restEnqueued) == 0 {
		t.Fatal("REST path: expected event and enqueue call")
	}

	// ── Tool path ────────────────────────────────────────────────────────────

	toolProd := &fakeProducer{returnID: "job-tool-equiv-01"}
	toolBus := &fakePublisher{}
	toolMod, _ := newTestModule(t, st, toolProd, toolBus)

	createTool := toolByName(t, toolMod.Tools(), "create_reminder")

	args := encodeArgs(t, map[string]any{
		"title":        "Grocery run",
		"notes":        "Milk, eggs, bread",
		"dueAt":        dueAtStr,
		"autoComplete": true,
	})

	toolRes, err := createTool.Execute(ctx, scope, args)
	if err != nil {
		t.Fatalf("create_reminder tool Execute: %v", err)
	}

	toolReminder, ok := toolRes.(reminders.Reminder)
	if !ok {
		// Try JSON round-trip in case the tool returns a concrete type that needs marshalling.
		b, _ := json.Marshal(toolRes)
		if unmarshalErr := json.Unmarshal(b, &toolReminder); unmarshalErr != nil {
			t.Fatalf("create_reminder result not a Reminder; got %T: %v", toolRes, toolRes)
		}
	}

	toolEvents := toolBus.allEvents()
	toolEnqueued := toolProd.allEnqueued()
	if len(toolEvents) == 0 || len(toolEnqueued) == 0 {
		t.Fatal("tool path: expected event and enqueue call")
	}

	// ── Assert equivalence ───────────────────────────────────────────────────

	// Same field values (IDs differ by design).
	if restResult.Title != toolReminder.Title {
		t.Errorf("Title: REST=%q tool=%q", restResult.Title, toolReminder.Title)
	}
	if restResult.Notes != toolReminder.Notes {
		t.Errorf("Notes: REST=%q tool=%q", restResult.Notes, toolReminder.Notes)
	}
	if restResult.DueAt != toolReminder.DueAt {
		t.Errorf("DueAt: REST=%q tool=%q", restResult.DueAt, toolReminder.DueAt)
	}
	if restResult.Status != toolReminder.Status {
		t.Errorf("Status: REST=%q tool=%q", restResult.Status, toolReminder.Status)
	}
	if restResult.AutoComplete != toolReminder.AutoComplete {
		t.Errorf("AutoComplete: REST=%v tool=%v", restResult.AutoComplete, toolReminder.AutoComplete)
	}
	// Both default tz from profile (America/New_York) when no explicit tz passed.
	if restResult.Tz != toolReminder.Tz {
		t.Errorf("Tz: REST=%q tool=%q", restResult.Tz, toolReminder.Tz)
	}

	// Same event type.
	if restEvents[0].event.Type != toolEvents[0].event.Type {
		t.Errorf("event Type: REST=%q tool=%q", restEvents[0].event.Type, toolEvents[0].event.Type)
	}
	if restEvents[0].event.Type != "reminder.created" {
		t.Errorf("event Type = %q, want \"reminder.created\"", restEvents[0].event.Type)
	}

	// Same EnqueueRequest shape (Kind, Key prefix, RunAt, Recurrence).
	re := restEnqueued[0]
	te := toolEnqueued[0]
	if re.Kind != te.Kind {
		t.Errorf("EnqueueRequest.Kind: REST=%q tool=%q", re.Kind, te.Kind)
	}
	if re.Kind != "reminder.fire" {
		t.Errorf("EnqueueRequest.Kind = %q, want \"reminder.fire\"", re.Kind)
	}
	if !re.RunAt.Equal(te.RunAt) {
		t.Errorf("EnqueueRequest.RunAt: REST=%v tool=%v", re.RunAt, te.RunAt)
	}
	if (re.Recurrence == nil) != (te.Recurrence == nil) {
		t.Errorf("EnqueueRequest.Recurrence nil mismatch: REST=%v tool=%v", re.Recurrence, te.Recurrence)
	}
}

// ── AC2: update_reminder routes to Service.Update ─────────────────────────────

// TestTool_UpdateReminder_EmitsUpdatedEvent verifies that update_reminder calls Service.Update and emits the same
// "reminder.updated" event as the PATCH path.
func TestTool_UpdateReminder_EmitsUpdatedEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-update-01"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{returnID: "job-upd-01"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	// Create a reminder first.
	created, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Original title",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Clear prior events.
	bus.mu.Lock()
	bus.events = nil
	bus.mu.Unlock()

	updateTool := toolByName(t, m.Tools(), "update_reminder")

	args := encodeArgs(t, map[string]any{
		"id":    created.ID,
		"title": "Updated title",
	})

	toolRes, err := updateTool.Execute(ctx, scope, args)
	if err != nil {
		t.Fatalf("update_reminder Execute: %v", err)
	}

	// Result must be the updated reminder.
	updated := reminderFromResult(t, toolRes)
	if updated.Title != "Updated title" {
		t.Errorf("updated.Title = %q, want \"Updated title\"", updated.Title)
	}
	if updated.ID != created.ID {
		t.Errorf("updated.ID = %q, want %q", updated.ID, created.ID)
	}

	// Must have emitted reminder.updated.
	evts := bus.allEvents()
	if len(evts) == 0 {
		t.Fatal("expected reminder.updated event, got none")
	}
	if evts[0].event.Type != "reminder.updated" {
		t.Errorf("event.Type = %q, want \"reminder.updated\"", evts[0].event.Type)
	}
}

// TestTool_UpdateReminder_AbsentFieldsPreserved verifies the merge semantics: fields not present in the tool args are
// left unchanged in the Service.
func TestTool_UpdateReminder_AbsentFieldsPreserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-update-02"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{returnID: "job-upd-02"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	dueAt := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)

	created, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title:        "Keep this title",
		Notes:        "Keep these notes",
		DueAt:        dueAt,
		AutoComplete: ptrFalse,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updateTool := toolByName(t, m.Tools(), "update_reminder")

	// Only update autoComplete — all other fields should be unchanged.
	args := encodeArgs(t, map[string]any{
		"id":           created.ID,
		"autoComplete": true,
	})

	toolRes, err := updateTool.Execute(ctx, scope, args)
	if err != nil {
		t.Fatalf("update_reminder Execute: %v", err)
	}

	updated := reminderFromResult(t, toolRes)
	if updated.Title != "Keep this title" {
		t.Errorf("Title changed unexpectedly: got %q, want \"Keep this title\"", updated.Title)
	}
	if updated.Notes != "Keep these notes" {
		t.Errorf("Notes changed unexpectedly: got %q, want \"Keep these notes\"", updated.Notes)
	}
	if updated.DueAt != dueAt.Format(time.RFC3339) {
		t.Errorf("DueAt changed unexpectedly: got %q", updated.DueAt)
	}
	if !updated.AutoComplete {
		t.Error("AutoComplete should be true after update")
	}
}

// TestTool_CompleteReminder_EmitsCompletedEvent verifies that complete_reminder calls Service.Complete and emits
// "reminder.completed".
func TestTool_CompleteReminder_EmitsCompletedEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-complete-01"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{returnID: "job-comp-01"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	created, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Complete me",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Clear prior events.
	bus.mu.Lock()
	bus.events = nil
	bus.mu.Unlock()

	completeTool := toolByName(t, m.Tools(), "complete_reminder")
	args := encodeArgs(t, map[string]any{"id": created.ID})

	toolRes, err := completeTool.Execute(ctx, scope, args)
	if err != nil {
		t.Fatalf("complete_reminder Execute: %v", err)
	}

	completed := reminderFromResult(t, toolRes)
	if completed.Status != "completed" {
		t.Errorf("Status = %q, want \"completed\"", completed.Status)
	}
	if completed.CompletedAt == "" {
		t.Error("CompletedAt should be set after complete")
	}

	evts := bus.allEvents()
	if len(evts) == 0 {
		t.Fatal("expected reminder.completed event, got none")
	}
	if evts[0].event.Type != "reminder.completed" {
		t.Errorf("event.Type = %q, want \"reminder.completed\"", evts[0].event.Type)
	}
}

// TestTool_DeleteReminder_EmitsDeletedEvent verifies that delete_reminder calls Service.Delete and emits
// "reminder.deleted".
func TestTool_DeleteReminder_EmitsDeletedEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-delete-01"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{returnID: "job-del-01"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	created, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Delete me",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Clear prior events.
	bus.mu.Lock()
	bus.events = nil
	bus.mu.Unlock()

	deleteTool := toolByName(t, m.Tools(), "delete_reminder")
	args := encodeArgs(t, map[string]any{"id": created.ID})

	_, err = deleteTool.Execute(ctx, scope, args)
	if err != nil {
		t.Fatalf("delete_reminder Execute: %v", err)
	}

	evts := bus.allEvents()
	if len(evts) == 0 {
		t.Fatal("expected reminder.deleted event, got none")
	}
	if evts[0].event.Type != "reminder.deleted" {
		t.Errorf("event.Type = %q, want \"reminder.deleted\"", evts[0].event.Type)
	}
}

// ── AC4: ValidationError → *capability.ToolError ─────────────────────────────

// TestTool_CreateReminder_BadDueAt_ReturnsToolError verifies that a malformed dueAt from the model surfaces as
// *capability.ToolError (model-correctable).
func TestTool_CreateReminder_BadDueAt_ReturnsToolError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-val-01"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	createTool := toolByName(t, m.Tools(), "create_reminder")

	// Pass a non-RFC-3339 dueAt string — the model emitted natural-language date.
	args := encodeArgs(t, map[string]any{
		"title": "Bad date reminder",
		"dueAt": "Thursday 8am",
	})

	_, err := createTool.Execute(ctx, scope, args)
	if err == nil {
		t.Fatal("expected error for bad dueAt, got nil")
	}

	var toolErr *capability.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *capability.ToolError, got %T: %v", err, err)
	}
	if toolErr.Code == "" {
		t.Error("ToolError.Code is empty; want a stable slug")
	}
	if toolErr.Message == "" {
		t.Error("ToolError.Message is empty; want a model-readable description")
	}
}

// TestTool_CreateReminder_BadRrule_ReturnsToolError verifies that an invalid RRULE string surfaces as
// *capability.ToolError.
func TestTool_CreateReminder_BadRrule_ReturnsToolError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-val-02"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	createTool := toolByName(t, m.Tools(), "create_reminder")
	dueAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	args := encodeArgs(t, map[string]any{
		"title": "Recurring reminder",
		"dueAt": dueAt,
		"rrule": "FREQ=BOGUS", // invalid RFC 5545
	})

	_, err := createTool.Execute(ctx, scope, args)
	if err == nil {
		t.Fatal("expected error for bad rrule, got nil")
	}

	var toolErr *capability.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *capability.ToolError, got %T: %v", err, err)
	}
	if toolErr.Code == "" {
		t.Error("ToolError.Code is empty")
	}
}

// TestTool_CreateReminder_BadRrule_MessageIsClean verifies that the *capability.ToolError message produced for an
// invalid RRULE does not contain raw library error text and does include a helpful FREQ= example so the model can
// self-correct.
func TestTool_CreateReminder_BadRrule_MessageIsClean(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-rrule-clean-01"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	createTool := toolByName(t, m.Tools(), "create_reminder")
	dueAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	args := encodeArgs(t, map[string]any{
		"title": "Recurring reminder",
		"dueAt": dueAt,
		"rrule": "FREQ=BOGUS",
	})

	_, err := createTool.Execute(ctx, scope, args)
	if err == nil {
		t.Fatal("expected error for bad rrule, got nil")
	}

	var toolErr *capability.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *capability.ToolError, got %T: %v", err, err)
	}
	// Must not leak library error text.
	if strings.Contains(toolErr.Message, "string: ") {
		t.Errorf("ToolError.Message leaks library error text (contains 'string: …'): %q", toolErr.Message)
	}
	// Must include a concrete FREQ= example so the model can self-correct.
	if !strings.Contains(toolErr.Message, "FREQ=") {
		t.Errorf("ToolError.Message lacks a FREQ= example; got: %q", toolErr.Message)
	}
}

// TestTool_CreateReminder_BadTz_ReturnsToolError verifies that an invalid IANA timezone string surfaces as
// *capability.ToolError.
func TestTool_CreateReminder_BadTz_ReturnsToolError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-val-03"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	createTool := toolByName(t, m.Tools(), "create_reminder")
	dueAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	args := encodeArgs(t, map[string]any{
		"title": "Bad tz reminder",
		"dueAt": dueAt,
		"tz":    "Not/A/Timezone",
	})

	_, err := createTool.Execute(ctx, scope, args)
	if err == nil {
		t.Fatal("expected error for bad tz, got nil")
	}

	var toolErr *capability.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *capability.ToolError, got %T: %v", err, err)
	}
	if toolErr.Code == "" {
		t.Error("ToolError.Code is empty")
	}
}

// TestTool_UpdateReminder_BadDueAt_ReturnsToolError verifies that update_reminder with a malformed dueAt surfaces as
// *capability.ToolError.
func TestTool_UpdateReminder_BadDueAt_ReturnsToolError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-val-04"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{returnID: "job-val-04"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	created, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Update me",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updateTool := toolByName(t, m.Tools(), "update_reminder")
	args := encodeArgs(t, map[string]any{
		"id":    created.ID,
		"dueAt": "next Tuesday", // natural-language — model mistake
	})

	_, err = updateTool.Execute(ctx, scope, args)
	if err == nil {
		t.Fatal("expected error for bad dueAt in update, got nil")
	}

	var toolErr *capability.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *capability.ToolError, got %T: %v", err, err)
	}
}

// TestTool_CompleteReminder_NotFound_ReturnsToolError verifies that complete_reminder with a non-existent or
// other-user's id surfaces as *capability.ToolError (not a plain infrastructure error).
func TestTool_CompleteReminder_NotFound_ReturnsToolError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-nf-01"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	completeTool := toolByName(t, m.Tools(), "complete_reminder")
	args := encodeArgs(t, map[string]any{"id": "nonexistent-id-xyz"})

	_, err := completeTool.Execute(ctx, scope, args)
	if err == nil {
		t.Fatal("expected error for nonexistent id, got nil")
	}

	var toolErr *capability.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *capability.ToolError for not-found, got %T: %v", err, err)
	}
}

// TestTool_DeleteReminder_NotFound_ReturnsToolError verifies that delete_reminder with a non-existent id surfaces as
// *capability.ToolError.
func TestTool_DeleteReminder_NotFound_ReturnsToolError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-nf-02"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	deleteTool := toolByName(t, m.Tools(), "delete_reminder")
	args := encodeArgs(t, map[string]any{"id": "nonexistent-id-xyz"})

	_, err := deleteTool.Execute(ctx, scope, args)
	if err == nil {
		t.Fatal("expected error for nonexistent id, got nil")
	}

	var toolErr *capability.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *capability.ToolError for not-found, got %T: %v", err, err)
	}
}

// TestTool_UpdateReminder_NotFound_ReturnsToolError verifies that update_reminder with a non-existent id surfaces as
// *capability.ToolError.
func TestTool_UpdateReminder_NotFound_ReturnsToolError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-nf-03"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	updateTool := toolByName(t, m.Tools(), "update_reminder")
	args := encodeArgs(t, map[string]any{
		"id":    "nonexistent-id-xyz",
		"title": "New title",
	})

	_, err := updateTool.Execute(ctx, scope, args)
	if err == nil {
		t.Fatal("expected error for nonexistent id, got nil")
	}

	var toolErr *capability.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *capability.ToolError for not-found, got %T: %v", err, err)
	}
}

// ── AC2: side-by-side event equivalence (update/complete/delete) ──────────────

// TestTool_UpdateReminder_SameEventAsREST asserts that the tool path and the Service-direct path emit the same event
// type for a plain field update.
func TestTool_UpdateReminder_SameEventAsREST(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-evteq-01"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	// REST (Service.Update direct).
	restProd := &fakeProducer{returnID: "job-evteq-rest-01"}
	restBus := &fakePublisher{}
	restMod, _ := newTestModule(t, st, restProd, restBus)
	restSvc := restMod.Service()

	r1, err := restSvc.Create(ctx, scope, reminders.CreateInput{
		Title: "REST update target",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("REST Create: %v", err)
	}

	newTitle := "REST updated"
	restBus.mu.Lock()
	restBus.events = nil
	restBus.mu.Unlock()

	_, err = restSvc.Update(ctx, scope, r1.ID, reminders.UpdateInput{
		Title: &newTitle,
	})
	if err != nil {
		t.Fatalf("REST Update: %v", err)
	}
	restEvts := restBus.allEvents()

	// Tool path.
	toolProd := &fakeProducer{returnID: "job-evteq-tool-01"}
	toolBus := &fakePublisher{}
	toolMod, _ := newTestModule(t, st, toolProd, toolBus)
	toolSvc := toolMod.Service()

	r2, err := toolSvc.Create(ctx, scope, reminders.CreateInput{
		Title: "Tool update target",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Tool Create: %v", err)
	}

	toolBus.mu.Lock()
	toolBus.events = nil
	toolBus.mu.Unlock()

	updateTool := toolByName(t, toolMod.Tools(), "update_reminder")
	toolUpdTitle := "Tool updated"
	_, err = updateTool.Execute(ctx, scope, encodeArgs(t, map[string]any{
		"id":    r2.ID,
		"title": toolUpdTitle,
	}))
	if err != nil {
		t.Fatalf("Tool Update: %v", err)
	}
	toolEvts := toolBus.allEvents()

	if len(restEvts) == 0 || len(toolEvts) == 0 {
		t.Fatal("expected events from both paths")
	}
	if restEvts[0].event.Type != toolEvts[0].event.Type {
		t.Errorf("event type mismatch: REST=%q tool=%q",
			restEvts[0].event.Type, toolEvts[0].event.Type)
	}
}

// TestTool_CompleteReminder_SameEventAsREST asserts that the tool emits the same "reminder.completed" event as
// Service.Complete.
func TestTool_CompleteReminder_SameEventAsREST(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-evteq-02"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	// REST (Service.Complete direct).
	restProd := &fakeProducer{returnID: "job-evteq-rest-02"}
	restBus := &fakePublisher{}
	restMod, _ := newTestModule(t, st, restProd, restBus)
	restSvc := restMod.Service()

	r1, err := restSvc.Create(ctx, scope, reminders.CreateInput{
		Title: "REST complete target",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("REST Create: %v", err)
	}

	restBus.mu.Lock()
	restBus.events = nil
	restBus.mu.Unlock()

	_, err = restSvc.Complete(ctx, scope, r1.ID)
	if err != nil {
		t.Fatalf("REST Complete: %v", err)
	}
	restEvts := restBus.allEvents()

	// Tool path.
	toolProd := &fakeProducer{returnID: "job-evteq-tool-02"}
	toolBus := &fakePublisher{}
	toolMod, _ := newTestModule(t, st, toolProd, toolBus)
	toolSvc := toolMod.Service()

	r2, err := toolSvc.Create(ctx, scope, reminders.CreateInput{
		Title: "Tool complete target",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Tool Create: %v", err)
	}

	toolBus.mu.Lock()
	toolBus.events = nil
	toolBus.mu.Unlock()

	completeTool := toolByName(t, toolMod.Tools(), "complete_reminder")
	_, err = completeTool.Execute(ctx, scope, encodeArgs(t, map[string]any{"id": r2.ID}))
	if err != nil {
		t.Fatalf("Tool Complete: %v", err)
	}
	toolEvts := toolBus.allEvents()

	if len(restEvts) == 0 || len(toolEvts) == 0 {
		t.Fatal("expected events from both paths")
	}
	if restEvts[0].event.Type != toolEvts[0].event.Type {
		t.Errorf("event type mismatch: REST=%q tool=%q",
			restEvts[0].event.Type, toolEvts[0].event.Type)
	}
	if restEvts[0].event.Type != "reminder.completed" {
		t.Errorf("event type = %q, want \"reminder.completed\"", restEvts[0].event.Type)
	}
}

// TestTool_DeleteReminder_SameEventAsREST asserts that the tool emits the same "reminder.deleted" event as
// Service.Delete.
func TestTool_DeleteReminder_SameEventAsREST(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openMigratedStore(t)
	const userID = "user-tool-evteq-03"
	seedUser(t, st, userID, "UTC")
	scope := newToolScope(userID)

	// REST (Service.Delete direct).
	restProd := &fakeProducer{returnID: "job-evteq-rest-03"}
	restBus := &fakePublisher{}
	restMod, _ := newTestModule(t, st, restProd, restBus)
	restSvc := restMod.Service()

	r1, err := restSvc.Create(ctx, scope, reminders.CreateInput{
		Title: "REST delete target",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("REST Create: %v", err)
	}

	restBus.mu.Lock()
	restBus.events = nil
	restBus.mu.Unlock()

	if err := restSvc.Delete(ctx, scope, r1.ID); err != nil {
		t.Fatalf("REST Delete: %v", err)
	}
	restEvts := restBus.allEvents()

	// Tool path.
	toolProd := &fakeProducer{returnID: "job-evteq-tool-03"}
	toolBus := &fakePublisher{}
	toolMod, _ := newTestModule(t, st, toolProd, toolBus)
	toolSvc := toolMod.Service()

	r2, err := toolSvc.Create(ctx, scope, reminders.CreateInput{
		Title: "Tool delete target",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Tool Create: %v", err)
	}

	toolBus.mu.Lock()
	toolBus.events = nil
	toolBus.mu.Unlock()

	deleteTool := toolByName(t, toolMod.Tools(), "delete_reminder")
	_, err = deleteTool.Execute(ctx, scope, encodeArgs(t, map[string]any{"id": r2.ID}))
	if err != nil {
		t.Fatalf("Tool Delete: %v", err)
	}
	toolEvts := toolBus.allEvents()

	if len(restEvts) == 0 || len(toolEvts) == 0 {
		t.Fatal("expected events from both paths")
	}
	if restEvts[0].event.Type != toolEvts[0].event.Type {
		t.Errorf("event type mismatch: REST=%q tool=%q",
			restEvts[0].event.Type, toolEvts[0].event.Type)
	}
	if restEvts[0].event.Type != "reminder.deleted" {
		t.Errorf("event type = %q, want \"reminder.deleted\"", restEvts[0].event.Type)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// reminderFromResult extracts a reminders.Reminder from a capability.Result. It handles both direct type assertion
// and JSON round-trip for typed returns.
func reminderFromResult(t *testing.T, res capability.Result) reminders.Reminder {
	t.Helper()
	if r, ok := res.(reminders.Reminder); ok {
		return r
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("reminderFromResult: marshal: %v", err)
	}
	var r reminders.Reminder
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("reminderFromResult: unmarshal: %v", err)
	}
	return r
}
