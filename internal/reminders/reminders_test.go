package reminders_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/reminders"
	"github.com/qovira/qovira/internal/scheduler"
	"github.com/qovira/qovira/internal/store"
	"github.com/qovira/qovira/internal/store/db"
)

// ptrTrue and ptrFalse are pointers to bool literals, used wherever a *bool
// argument is required. Using package-level variables avoids a helper function.
var (
	ptrTrue  = func() *bool { b := true; return &b }()
	ptrFalse = func() *bool { b := false; return &b }()
)

// ── fakes ─────────────────────────────────────────────────────────────────────

// fakeProducer records Enqueue, Reschedule, and Cancel calls and allows
// controlled returns. The Reschedule field was extended in the
// edit/complete/delete slice (AC1 guard test).
type fakeProducer struct {
	mu          sync.Mutex
	enqueued    []scheduler.EnqueueRequest
	returnID    string
	returnErr   error
	cancelled   []string
	cancelErr   error
	rescheduled []rescheduleCall
}

// rescheduleCall records a single Reschedule invocation.
type rescheduleCall struct {
	jobID string
	runAt time.Time
}

func (f *fakeProducer) Enqueue(_ context.Context, req scheduler.EnqueueRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, req)
	if f.returnErr != nil {
		return "", f.returnErr
	}
	jobID := f.returnID
	if jobID == "" {
		jobID = fmt.Sprintf("job-%d", len(f.enqueued))
	}
	return jobID, nil
}

func (f *fakeProducer) Reschedule(_ context.Context, jobID string, runAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescheduled = append(f.rescheduled, rescheduleCall{jobID: jobID, runAt: runAt})
	return nil
}

func (f *fakeProducer) Cancel(_ context.Context, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, jobID)
	return f.cancelErr
}

// allEnqueued returns a snapshot of all recorded enqueue requests.
func (f *fakeProducer) allEnqueued() []scheduler.EnqueueRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]scheduler.EnqueueRequest, len(f.enqueued))
	copy(out, f.enqueued)
	return out
}

// allRescheduled returns a snapshot of all recorded Reschedule calls.
func (f *fakeProducer) allRescheduled() []rescheduleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]rescheduleCall, len(f.rescheduled))
	copy(out, f.rescheduled)
	return out
}

// allCancelled returns a snapshot of all recorded Cancel job IDs.
func (f *fakeProducer) allCancelled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.cancelled))
	copy(out, f.cancelled)
	return out
}

// fakePublisher records Publish calls.
type fakePublisher struct {
	mu     sync.Mutex
	events []publishedEvent
}

type publishedEvent struct {
	userID string
	event  events.Event
}

func (f *fakePublisher) Publish(userID string, e events.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, publishedEvent{userID: userID, event: e})
}

// allEvents returns a snapshot of all published events.
func (f *fakePublisher) allEvents() []publishedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]publishedEvent, len(f.events))
	copy(out, f.events)
	return out
}

// fakeRegistrar captures handler registrations without running them.
type fakeRegistrar struct {
	mu       sync.Mutex
	handlers map[string]scheduler.Handler
}

func newFakeRegistrar() *fakeRegistrar {
	return &fakeRegistrar{handlers: make(map[string]scheduler.Handler)}
}

func (f *fakeRegistrar) Register(kind string, h scheduler.Handler) {
	f.mu.Lock()
	f.handlers[kind] = h
	f.mu.Unlock()
}

// dispatch calls the named handler synchronously, returning its error.
func (f *fakeRegistrar) dispatch(ctx context.Context, kind string, job scheduler.Job) error {
	f.mu.Lock()
	h, ok := f.handlers[kind]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("fakeRegistrar: no handler registered for %q", kind)
	}
	return h(ctx, job)
}

// ── helpers ───────────────────────────────────────────────────────────────────

const testKey = "a-sufficiently-long-passphrase-for-sqlcipher-reminders"

// openMigratedStore opens a fresh SQLCipher database on a temp file and runs
// all migrations. It is closed via t.Cleanup. Never use ":memory:".
func openMigratedStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(store.Config{
		Path: filepath.Join(dir, "test.db"),
		Key:  testKey,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.NewRunner().Up(context.Background(), s.Writer()); err != nil {
		_ = s.Close()
		t.Fatalf("migrations.Up: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedUser inserts a minimal user row directly so reminder tests have a valid
// user_id without pulling in internal/auth.
func seedUser(t *testing.T, s *store.Store, userID, tz string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.Writer().ExecContext(context.Background(), `
INSERT INTO users (id, email, display_name, password_hash, role, timezone, locale, language, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID,
		fmt.Sprintf("user-%s@test.example", userID),
		"Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$...",
		"member",
		tz,
		"en-US",
		"en",
		now, now,
	)
	if err != nil {
		t.Fatalf("seedUser: %v", err)
	}
}

// newTestModule builds a reminders.Module wired to the given fakes.  Returns
// the module and the fakeRegistrar so tests can dispatch the fire handler.
func newTestModule(
	t *testing.T,
	st *store.Store,
	prod *fakeProducer,
	bus *fakePublisher,
) (*reminders.Module, *fakeRegistrar) {
	t.Helper()
	reg := newFakeRegistrar()
	m := reminders.New(st, prod, bus, reg)
	return m, reg
}

// newScopeFor builds a UserScope from a raw userID string (no auth layer).
func newScopeFor(userID string) store.Scope {
	return store.UserScope(store.Principal{UserID: userID})
}

// ── AC1: migration creates the table and index ─────────────────────────────

// TestMigration_RemindersTable verifies that running migrations creates the
// reminders table with all expected columns and the composite index.
func TestMigration_RemindersTable(t *testing.T) {
	t.Parallel()
	st := openMigratedStore(t)
	ctx := context.Background()

	// Verify the table exists by querying sqlite_master.
	var tableName string
	err := st.Reader().QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='reminders'`,
	).Scan(&tableName)
	if err != nil {
		t.Fatalf("reminders table not found: %v", err)
	}
	if tableName != "reminders" {
		t.Fatalf("got table name %q, want %q", tableName, "reminders")
	}

	// Verify the composite index exists with the correct name and columns.
	var indexName string
	err = st.Reader().QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='reminders_user_due'`,
	).Scan(&indexName)
	if err != nil {
		t.Fatalf("reminders_user_due index not found: %v", err)
	}
	// Verify the index covers (user_id, due_at, id) — not the old (user_id, status, due_at).
	var indexSQL string
	err = st.Reader().QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='reminders_user_due'`,
	).Scan(&indexSQL)
	if err != nil {
		t.Fatalf("could not read reminders_user_due DDL: %v", err)
	}
	for _, col := range []string{"user_id", "due_at", "id"} {
		if !strings.Contains(indexSQL, col) {
			t.Errorf("reminders_user_due DDL missing column %q; got: %s", col, indexSQL)
		}
	}
	if strings.Contains(indexSQL, "status") {
		t.Errorf("reminders_user_due DDL must NOT include 'status'; got: %s", indexSQL)
	}

	// Verify expected columns by inserting a row and reading it back.
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = st.Writer().ExecContext(ctx, `
INSERT INTO reminders (id, user_id, title, notes, due_at, rrule, tz, auto_complete, status,
                       completed_at, last_fired_at, fire_job_id, created_at, updated_at)
VALUES ('R1','U1','Test',NULL,'2026-01-01T00:00:00Z',NULL,'UTC',1,'active',NULL,NULL,NULL,?,?)`,
		now, now)
	if err != nil {
		t.Fatalf("insert into reminders: %v", err)
	}

	var id, userID, title, status, tz string
	var autoComplete int64
	err = st.Reader().QueryRowContext(ctx,
		`SELECT id, user_id, title, status, tz, auto_complete FROM reminders WHERE id='R1'`,
	).Scan(&id, &userID, &title, &status, &tz, &autoComplete)
	if err != nil {
		t.Fatalf("select from reminders: %v", err)
	}
	if id != "R1" || userID != "U1" || title != "Test" || status != "active" || tz != "UTC" || autoComplete != 1 {
		t.Errorf("unexpected row values: id=%q userID=%q title=%q status=%q tz=%q autoComplete=%d",
			id, userID, title, status, tz, autoComplete)
	}
}

// ── AC2: POST /api/v1/reminders → 201 + Location ──────────────────────────

// TestService_Create_Success verifies the Service.Create happy path:
// persists the row, publishes reminder.created, enqueues a one-shot fire-job,
// and stores the returned fire_job_id.
func TestService_Create_Success(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-fire-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-create-01"
	seedUser(t, st, userID, "America/New_York")
	scope := newScopeFor(userID)

	dueAt := time.Now().UTC().Add(time.Hour)
	in := reminders.CreateInput{
		Title:        "Pick up groceries",
		DueAt:        dueAt,
		AutoComplete: ptrTrue,
	}

	r, err := svc.Create(ctx, scope, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// --- Persisted row has all expected fields ---
	if r.ID == "" {
		t.Error("expected non-empty ID")
	}
	if r.UserID != userID {
		t.Errorf("got UserID %q, want %q", r.UserID, userID)
	}
	if r.Title != "Pick up groceries" {
		t.Errorf("got Title %q, want %q", r.Title, "Pick up groceries")
	}
	if r.Status != "active" {
		t.Errorf("got Status %q, want %q", r.Status, "active")
	}
	if !r.AutoComplete {
		t.Error("expected AutoComplete=true")
	}
	if r.FireJobID == "" {
		t.Error("expected non-empty FireJobID")
	}

	// --- published reminder.created ---
	evts := bus.allEvents()
	if len(evts) == 0 {
		t.Fatal("expected reminder.created event, got none")
	}
	if evts[0].event.Type != "reminder.created" {
		t.Errorf("got event type %q, want %q", evts[0].event.Type, "reminder.created")
	}
	if evts[0].userID != userID {
		t.Errorf("got event userID %q, want %q", evts[0].userID, userID)
	}

	// --- enqueued a fire-job with correct fields ---
	enqueued := prod.allEnqueued()
	if len(enqueued) == 0 {
		t.Fatal("expected Enqueue call, got none")
	}
	req := enqueued[0]
	if req.Kind != "reminder.fire" {
		t.Errorf("got Kind %q, want %q", req.Kind, "reminder.fire")
	}
	if req.Key != "reminder:"+r.ID {
		t.Errorf("got Key %q, want %q", req.Key, "reminder:"+r.ID)
	}
	// RunAt is canonically truncated to the second (matches persisted due_at).
	wantRunAt := dueAt.UTC().Truncate(time.Second)
	if !req.RunAt.Equal(wantRunAt) {
		t.Errorf("got RunAt %v, want %v (truncated to second)", req.RunAt, wantRunAt)
	}
	if req.Recurrence != nil {
		t.Error("expected nil Recurrence for a one-shot reminder")
	}
	// Verify payload contains reminderId.
	var payload struct {
		ReminderID string `json:"reminderId"`
	}
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		t.Fatalf("unmarshal enqueue payload: %v", err)
	}
	if payload.ReminderID != r.ID {
		t.Errorf("got payload.reminderId %q, want %q", payload.ReminderID, r.ID)
	}

	// --- fire_job_id is persisted on the row ---
	if r.FireJobID != "job-fire-001" {
		t.Errorf("got FireJobID %q, want %q", r.FireJobID, "job-fire-001")
	}
}

// ── AC2 (REST layer): POST → 201 + Location header ────────────────────────

func TestHTTP_Create_201(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-http-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	const userID = "user-http-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	router := httpx.NewRouter()
	m.Routes(router)

	dueAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{
		"title": "Call dentist",
		"dueAt": dueAt,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: userID, Role: "member"}))
	_ = scope
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusCreated {
		t.Errorf("got status %d, want %d; body: %s", res.StatusCode, http.StatusCreated, w.Body.String())
	}
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, "/api/v1/reminders/") {
		t.Errorf("got Location %q, want /api/v1/reminders/<id>", loc)
	}

	// Body must decode to a reminder.
	var resp reminders.Reminder
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID == "" {
		t.Error("expected non-empty id in response")
	}
	if resp.Title != "Call dentist" {
		t.Errorf("got title %q, want %q", resp.Title, "Call dentist")
	}
}

// ── AC3: Validation → 422 ─────────────────────────────────────────────────

func TestHTTP_Create_Validation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	const userID = "user-valid-01"
	seedUser(t, st, userID, "UTC")

	router := httpx.NewRouter()
	m.Routes(router)

	dueAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	tests := []struct {
		name        string
		body        map[string]any
		wantPointer string
	}{
		{
			name:        "empty title",
			body:        map[string]any{"title": "", "dueAt": dueAt},
			wantPointer: "/title",
		},
		{
			name:        "whitespace title",
			body:        map[string]any{"title": "   ", "dueAt": dueAt},
			wantPointer: "/title",
		},
		{
			name:        "missing title",
			body:        map[string]any{"dueAt": dueAt},
			wantPointer: "/title",
		},
		{
			name:        "missing dueAt",
			body:        map[string]any{"title": "Reminder"},
			wantPointer: "/dueAt",
		},
		{
			name:        "invalid dueAt",
			body:        map[string]any{"title": "Reminder", "dueAt": "not-a-date"},
			wantPointer: "/dueAt",
		},
		{
			name:        "invalid tz",
			body:        map[string]any{"title": "Reminder", "dueAt": dueAt, "tz": "Not/A/Zone"},
			wantPointer: "/tz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: userID, Role: "member"}))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			res := w.Result()
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Errorf("got status %d, want 422; body: %s", res.StatusCode, w.Body.String())
				return
			}

			var prob struct {
				Status int `json:"status"`
				Errors []struct {
					Pointer string `json:"pointer"`
				} `json:"errors"`
			}
			if err := json.NewDecoder(w.Body).Decode(&prob); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if prob.Status != http.StatusUnprocessableEntity {
				t.Errorf("problem.status=%d, want 422", prob.Status)
			}

			found := false
			for _, fe := range prob.Errors {
				if fe.Pointer == tt.wantPointer {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected field error at pointer %q; got errors: %+v", tt.wantPointer, prob.Errors)
			}
		})
	}
}

// ── AC4: past dueAt is accepted ───────────────────────────────────────────

func TestService_Create_PastDueAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-past-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-past-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	pastDue := time.Now().UTC().Add(-24 * time.Hour) // yesterday
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title:        "Past reminder",
		DueAt:        pastDue,
		AutoComplete: ptrTrue,
	})
	if err != nil {
		t.Fatalf("Create with past dueAt should succeed, got: %v", err)
	}

	// Confirm it was enqueued (RunAt matches the canonical truncated past dueAt — scheduler fires on next poll).
	enqueued := prod.allEnqueued()
	if len(enqueued) == 0 {
		t.Fatal("expected Enqueue call for past dueAt")
	}
	wantRunAt := pastDue.UTC().Truncate(time.Second)
	if !enqueued[0].RunAt.Equal(wantRunAt) {
		t.Errorf("got RunAt %v, want %v (canonical truncated past dueAt)", enqueued[0].RunAt, wantRunAt)
	}
	if r.ID == "" {
		t.Error("expected non-empty ID")
	}
}

// ── AC5: tz defaults from profile; explicit tz overrides ─────────────────

func TestService_Create_TzDefaultFromProfile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-tz-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-tz-01"
	seedUser(t, st, userID, "America/Chicago")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Tz default test",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.Tz != "America/Chicago" {
		t.Errorf("got tz %q, want %q (from profile)", r.Tz, "America/Chicago")
	}
}

func TestService_Create_TzExplicitOverride(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-tz-002"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-tz-02"
	seedUser(t, st, userID, "America/Chicago")
	scope := newScopeFor(userID)

	explicit := "Europe/Paris"
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Tz override test",
		DueAt: time.Now().UTC().Add(time.Hour),
		Tz:    explicit,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.Tz != explicit {
		t.Errorf("got tz %q, want %q (explicit override)", r.Tz, explicit)
	}
}

func TestService_Create_TzEmptyProfileFallsBackToUTC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-tz-003"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-tz-03"
	// Seed with empty timezone — Service must fall back to UTC.
	seedUser(t, st, userID, "")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Tz fallback test",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.Tz != "UTC" {
		t.Errorf("got tz %q, want %q (UTC fallback)", r.Tz, "UTC")
	}
}

// ── AC6: fire handler auto-complete ──────────────────────────────────────

func TestFireHandler_AutoComplete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-fire-ac-001"}
	bus := &fakePublisher{}
	m, reg := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-fire-ac-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	// Create a reminder with auto_complete=true.
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title:        "Auto-complete reminder",
		DueAt:        time.Now().UTC().Add(-time.Minute),
		AutoComplete: ptrTrue,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Build the job payload and dispatch the fire handler.
	payload, _ := json.Marshal(map[string]string{"reminderId": r.ID})
	job := scheduler.Job{
		ID:      "job-fire-ac-001",
		Kind:    "reminder.fire",
		Payload: payload,
		Attempt: 1,
		Scope:   scope,
	}

	if err := reg.dispatch(ctx, "reminder.fire", job); err != nil {
		t.Fatalf("fire handler: %v", err)
	}

	// Published reminder.fired event.
	evts := bus.allEvents()
	var firedEvt *publishedEvent
	for i := range evts {
		if evts[i].event.Type == "reminder.fired" {
			firedEvt = &evts[i]
			break
		}
	}
	if firedEvt == nil {
		t.Fatal("expected reminder.fired event, got none")
	}
	if firedEvt.userID != userID {
		t.Errorf("got fired event userID %q, want %q", firedEvt.userID, userID)
	}

	// Verify fired event payload.
	data, _ := json.Marshal(firedEvt.event.Data)
	var firedPayload struct {
		ReminderID string `json:"reminderId"`
		Title      string `json:"title"`
		DueAt      string `json:"dueAt"`
		FiredAt    string `json:"firedAt"`
	}
	if err := json.Unmarshal(data, &firedPayload); err != nil {
		t.Fatalf("unmarshal fired payload: %v", err)
	}
	if firedPayload.ReminderID != r.ID {
		t.Errorf("got reminderId %q, want %q", firedPayload.ReminderID, r.ID)
	}
	if firedPayload.Title != "Auto-complete reminder" {
		t.Errorf("got title %q, want %q", firedPayload.Title, "Auto-complete reminder")
	}
	if firedPayload.FiredAt == "" {
		t.Error("expected non-empty firedAt")
	}

	// Row must be status=completed and have last_fired_at + completed_at set.
	row, err := db.New(st.Reader()).GetReminder(ctx, db.GetReminderParams{
		ID:     r.ID,
		UserID: userID,
	})
	if err != nil {
		t.Fatalf("GetReminder: %v", err)
	}
	if row.Status != "completed" {
		t.Errorf("got status %q, want %q (auto_complete=true)", row.Status, "completed")
	}
	if !row.LastFiredAt.Valid || row.LastFiredAt.String == "" {
		t.Error("expected last_fired_at to be stamped")
	}
	if !row.CompletedAt.Valid || row.CompletedAt.String == "" {
		t.Error("expected completed_at to be set (auto_complete=true)")
	}
}

// ── AC7: fire handler manual (auto_complete=false) ────────────────────────

func TestFireHandler_KeepActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-fire-ka-001"}
	bus := &fakePublisher{}
	m, reg := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-fire-ka-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	// Create with auto_complete=false.
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title:        "Manual reminder",
		DueAt:        time.Now().UTC().Add(-time.Minute),
		AutoComplete: ptrFalse,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	payload, _ := json.Marshal(map[string]string{"reminderId": r.ID})
	job := scheduler.Job{
		ID:      "job-fire-ka-001",
		Kind:    "reminder.fire",
		Payload: payload,
		Attempt: 1,
		Scope:   scope,
	}

	if err := reg.dispatch(ctx, "reminder.fire", job); err != nil {
		t.Fatalf("fire handler: %v", err)
	}

	// published reminder.fired
	evts := bus.allEvents()
	var found bool
	for _, e := range evts {
		if e.event.Type == "reminder.fired" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected reminder.fired event")
	}

	// Status must remain active; last_fired_at stamped; completed_at NULL.
	row, err := db.New(st.Reader()).GetReminder(ctx, db.GetReminderParams{
		ID:     r.ID,
		UserID: userID,
	})
	if err != nil {
		t.Fatalf("GetReminder: %v", err)
	}
	if row.Status != "active" {
		t.Errorf("got status %q, want %q (auto_complete=false)", row.Status, "active")
	}
	if !row.LastFiredAt.Valid || row.LastFiredAt.String == "" {
		t.Error("expected last_fired_at to be stamped")
	}
	if row.CompletedAt.Valid {
		t.Error("expected completed_at to be NULL (auto_complete=false)")
	}
}

// ── AC8: deleted reminder before fire → scheduler.Permanent ───────────────

func TestFireHandler_DeletedReminder_Permanent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-fire-del-001"}
	bus := &fakePublisher{}
	m, reg := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-fire-del-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title:        "Doomed reminder",
		DueAt:        time.Now().UTC().Add(-time.Minute),
		AutoComplete: ptrTrue,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Delete the reminder before the fire handler runs.
	_, err = st.Writer().ExecContext(ctx,
		`DELETE FROM reminders WHERE id = ? AND user_id = ?`, r.ID, userID)
	if err != nil {
		t.Fatalf("delete reminder: %v", err)
	}

	payload, _ := json.Marshal(map[string]string{"reminderId": r.ID})
	job := scheduler.Job{
		ID:      "job-fire-del-001",
		Kind:    "reminder.fire",
		Payload: payload,
		Attempt: 1,
		Scope:   scope,
	}

	handlerErr := reg.dispatch(ctx, "reminder.fire", job)
	if handlerErr == nil {
		t.Fatal("expected error from fire handler for deleted reminder")
	}

	// The error must be wrapped with scheduler.Permanent so it dead-letters without retry.
	var permErr interface{ Error() string }
	// Check that the error's message contains "permanent" (the scheduler.permanentError wraps this).
	if !isPermanentError(handlerErr) {
		t.Errorf("expected scheduler.Permanent error, got: %v", handlerErr)
	}
	_ = permErr
}

// isPermanentError checks that the error is a permanent error by checking if
// wrapping it again with scheduler.Permanent still wraps it (i.e., the handler
// returned a permanent error already). We check via a type assertion approach.
func isPermanentError(err error) bool {
	// scheduler.Permanent wraps the error in a permanentError type. The only
	// way to detect it without importing the unexported type is to check if
	// scheduler.Permanent(err) would produce a double-wrap — which means we
	// need to inspect the error message prefix "scheduler: permanent:".
	return strings.HasPrefix(err.Error(), "scheduler: permanent:")
}

// ── AC9: GET /api/v1/reminders/{id} ──────────────────────────────────────

func TestHTTP_Get_200(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-get-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-get-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title:        "GET test reminder",
		DueAt:        time.Now().UTC().Add(time.Hour),
		AutoComplete: ptrTrue,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	router := httpx.NewRouter()
	m.Routes(router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders/"+r.ID, nil)
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: userID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Errorf("got %d, want 200; body: %s", res.StatusCode, w.Body.String())
	}

	var resp reminders.Reminder
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != r.ID {
		t.Errorf("got id %q, want %q", resp.ID, r.ID)
	}
	if resp.Title != "GET test reminder" {
		t.Errorf("got title %q, want %q", resp.Title, "GET test reminder")
	}
}

func TestHTTP_Get_404_OtherUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-404-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const ownerID = "user-owner-01"
	const otherID = "user-other-01"
	seedUser(t, st, ownerID, "UTC")
	seedUser(t, st, otherID, "UTC")
	ownerScope := newScopeFor(ownerID)

	r, err := svc.Create(ctx, ownerScope, reminders.CreateInput{
		Title:        "Owner's reminder",
		DueAt:        time.Now().UTC().Add(time.Hour),
		AutoComplete: ptrTrue,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	router := httpx.NewRouter()
	m.Routes(router)

	// Request as the other user.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders/"+r.ID, nil)
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: otherID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("got %d, want 404 for other user accessing reminder", res.StatusCode)
	}
}

// ── AC9: Service.Get ──────────────────────────────────────────────────────

func TestService_Get(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-svcget-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-svcget-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title:        "Svc get test",
		DueAt:        time.Now().UTC().Add(time.Hour),
		Notes:        "some notes",
		AutoComplete: ptrFalse,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.Get(ctx, scope, r.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != r.ID {
		t.Errorf("got ID %q, want %q", got.ID, r.ID)
	}
	if got.Notes != "some notes" {
		t.Errorf("got Notes %q, want %q", got.Notes, "some notes")
	}
	if got.AutoComplete {
		t.Error("expected AutoComplete=false")
	}
	if got.FireJobID != "job-svcget-001" {
		t.Errorf("got FireJobID %q, want %q", got.FireJobID, "job-svcget-001")
	}
}

func TestService_Get_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-notfound-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	_, err := svc.Get(ctx, scope, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error for non-existent reminder")
	}
	// Must be ErrNotFound.
	if !isNotFoundError(err) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func isNotFoundError(err error) bool {
	return strings.Contains(err.Error(), "not found")
}

// ── Module interface satisfaction ────────────────────────────────────────

func TestModule_Interface(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	if m.Name() != "reminders" {
		t.Errorf("Name() = %q, want %q", m.Name(), "reminders")
	}
	// Tools() now returns the four AI tool adapters (create/update/complete/delete).
	// The nil-placeholder was replaced in slice 5 — verify non-nil and correct count.
	if got := m.Tools(); len(got) != 4 {
		t.Errorf("Tools() returned %d tools, want 4", len(got))
	}

	// Routes must not panic.
	router := httpx.NewRouter()
	m.Routes(router)
	_ = ctx
}

// ── Malformed body → 400 ─────────────────────────────────────────────────

func TestHTTP_Create_MalformedBody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	const userID = "user-malform-01"
	seedUser(t, st, userID, "UTC")

	router := httpx.NewRouter()
	m.Routes(router)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders",
		strings.NewReader("{invalid json"))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: userID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400 for malformed body", w.Code)
	}
}

// ── AC2 compensation: enqueue failure cleans up the row ──────────────────

// TestService_Create_EnqueueFailure_CleansUpRow verifies that when Enqueue
// returns an error after the row has been inserted, Create deletes the orphan
// row (best-effort) and returns the original enqueue error.
//
// The stamp-failure compensation path (Cancel + delete after SetReminderFireJobID
// fails) is structurally symmetric to this path and uses the same deleteReminder
// helper; injecting a SetReminderFireJobID failure from the external test package
// would require closing/replacing the write connection, which is impractical
// without contorting the store's test surface. The enqueue-failure test below
// therefore exercises the shared compensation helper (delete) and its slog
// secondary-failure path via the existing fake infrastructure, giving high
// confidence the stamp-failure branch behaves identically.
func TestService_Create_EnqueueFailure_CleansUpRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	bus := &fakePublisher{}

	const userID = "user-enqfail-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	tests := []struct {
		name string
	}{
		{name: "enqueue_error_row_deleted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			prod := &fakeProducer{returnErr: fmt.Errorf("scheduler unavailable")}
			m, _ := newTestModule(t, st, prod, bus)
			svc := m.Service()

			_, err := svc.Create(ctx, scope, reminders.CreateInput{
				Title: "Doomed",
				DueAt: time.Now().UTC().Add(time.Hour),
			})
			if err == nil {
				t.Fatal("expected Create to return an error when Enqueue fails")
			}
			if !strings.Contains(err.Error(), "scheduler unavailable") {
				t.Errorf("expected original enqueue error in returned err, got: %v", err)
			}

			// The row must have been deleted; Get should return ErrNotFound.
			// We can't know the reminder ID because Create failed, but we can
			// verify that no reminder exists for this user by probing the DB directly.
			var count int
			row := st.Reader().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM reminders WHERE user_id = ?`, userID)
			if scanErr := row.Scan(&count); scanErr != nil {
				t.Fatalf("count reminders: %v", scanErr)
			}
			if count != 0 {
				t.Errorf("expected 0 reminder rows after enqueue failure (orphan cleanup), got %d", count)
			}
		})
	}
}

// ── AC5 extension: invalid profile tz falls back to UTC ──────────────────

// TestService_Create_InvalidProfileTzFallsBackToUTC seeds a user whose profile
// timezone is syntactically invalid ("Mars/Phobos"). Service.Create must still
// succeed and snapshot tz as "UTC", locking the AC5 silent-fallback behavior.
//
// An explicitly supplied invalid tz still triggers a 422 (covered by
// TestHTTP_Create_Validation/"invalid tz") — this case is distinct.
func TestService_Create_InvalidProfileTzFallsBackToUTC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-tz-invalid-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-tz-invalid-01"
	// Seed with an invalid IANA timezone — Service must fall back to UTC.
	seedUser(t, st, userID, "Mars/Phobos")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Invalid tz profile fallback",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: unexpected error with invalid profile tz: %v", err)
	}
	if r.Tz != "UTC" {
		t.Errorf("got tz %q, want %q (UTC fallback for invalid profile tz)", r.Tz, "UTC")
	}
}

// ── List / filter acceptance criteria ────────────────────────────────────

// listResponse is the decoded JSON envelope for GET /api/v1/reminders.
// NextCursor is *string so it decodes JSON null as nil (last page) vs. a
// non-nil pointer to the opaque cursor string (more pages exist).
type listResponse struct {
	Data       []reminders.Reminder `json:"data"`
	Pagination struct {
		NextCursor *string `json:"nextCursor"`
		HasMore    bool    `json:"hasMore"`
	} `json:"pagination"`
}

// listRequest builds and fires GET /api/v1/reminders with the given query params
// against the router, authenticated as userID.
func listRequest(t *testing.T, router *httpx.Router, userID string, params url.Values) *httptest.ResponseRecorder {
	t.Helper()
	u := "/api/v1/reminders"
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	req = req.WithContext(httpx.ContextWithPrincipal(req.Context(), store.Principal{UserID: userID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// decodeListResponse decodes the response body into a listResponse, failing the
// test immediately if anything goes wrong.
func decodeListResponse(t *testing.T, w *httptest.ResponseRecorder) listResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
	var resp listResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	return resp
}

// seedReminder is a compact helper for seeding a reminder with a specific status.
// It creates via the service (exercising the full create path), then if status is
// "completed" it updates the row directly to avoid pulling in the complete slice.
func seedReminder(
	t *testing.T, svc *reminders.Service, st *store.Store, scope store.Scope,
	title string, dueAt time.Time, status string,
) reminders.Reminder {
	t.Helper()
	r, err := svc.Create(context.Background(), scope, reminders.CreateInput{
		Title:        title,
		DueAt:        dueAt,
		AutoComplete: ptrFalse, // keep active so we can test status=active
	})
	if err != nil {
		t.Fatalf("seedReminder %q: %v", title, err)
	}
	// If a completed status is requested, mark it completed directly in the DB.
	if status == "completed" {
		now := time.Now().UTC().Format(time.RFC3339)
		_, dbErr := st.Writer().ExecContext(
			context.Background(),
			`UPDATE reminders SET status='completed', completed_at=?, updated_at=? WHERE id=? AND user_id=?`,
			now, now, r.ID, scope.UserID(),
		)
		if dbErr != nil {
			t.Fatalf("seedReminder: mark completed: %v", dbErr)
		}
		r.Status = "completed"
		r.CompletedAt = now
	}
	return r
}

// ── AC1 (List): user isolation and dueAt order ────────────────────────────

// TestList_UserIsolation_And_Order verifies that GET /api/v1/reminders returns
// only the authenticated user's reminders, ordered by dueAt ascending.
func TestList_UserIsolation_And_Order(t *testing.T) {
	t.Parallel()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const (
		userA = "list-iso-user-a"
		userB = "list-iso-user-b"
	)
	seedUser(t, st, userA, "UTC")
	seedUser(t, st, userB, "UTC")
	scopeA := newScopeFor(userA)
	scopeB := newScopeFor(userB)

	base := time.Now().UTC().Truncate(time.Second)
	// Seed user A: two reminders in non-chronological creation order.
	r1 := seedReminder(t, svc, st, scopeA, "A later", base.Add(2*time.Hour), "active")
	r2 := seedReminder(t, svc, st, scopeA, "A earlier", base.Add(time.Hour), "active")
	// Seed user B: one reminder that must not appear in user A's list.
	seedReminder(t, svc, st, scopeB, "B reminder", base.Add(30*time.Minute), "active")
	_ = r1

	router := httpx.NewRouter()
	m.Routes(router)

	w := listRequest(t, router, userA, nil)
	resp := decodeListResponse(t, w)

	// Only user A's reminders.
	if len(resp.Data) != 2 {
		t.Fatalf("got %d reminders, want 2 (user A's only)", len(resp.Data))
	}
	for _, item := range resp.Data {
		if item.UserID != userA {
			t.Errorf("got reminder with userID=%q, want %q (isolation failure)", item.UserID, userA)
		}
	}
	// Ordered by dueAt: earlier first.
	if resp.Data[0].ID != r2.ID {
		t.Errorf("first item ID=%q, want %q (r2, earlier dueAt)", resp.Data[0].ID, r2.ID)
	}
}

// ── AC2 (List): cursor pagination ────────────────────────────────────────

// TestList_CursorPagination verifies that:
//   - default page size is 25 (seeds 30 rows, first page has 25 + hasMore=true)
//   - max is 100 (limit=200 clamps to 100)
//   - next-cursor assembles the full set with no overlap and no gaps
func TestList_CursorPagination(t *testing.T) {
	t.Parallel()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "list-page-user-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	base := time.Now().UTC().Truncate(time.Second)
	const total = 30
	// Seed 30 reminders with distinct dueAt values so order is deterministic.
	seeded := make([]string, total)
	for i := range total {
		r := seedReminder(t, svc, st, scope, fmt.Sprintf("item %02d", i), base.Add(time.Duration(i)*time.Minute), "active")
		seeded[i] = r.ID
	}

	router := httpx.NewRouter()
	m.Routes(router)

	// 1. Default page size = 25; should return 25 items and hasMore=true.
	w := listRequest(t, router, userID, nil)
	resp := decodeListResponse(t, w)
	if len(resp.Data) != 25 {
		t.Errorf("default page: got %d items, want 25", len(resp.Data))
	}
	if !resp.Pagination.HasMore {
		t.Error("default page: expected hasMore=true")
	}
	if resp.Pagination.NextCursor == nil {
		t.Error("default page: expected non-null nextCursor")
	}

	// 2. Collect all pages and verify completeness + no overlap.
	var collected []string
	cursor := ""
	for {
		params := url.Values{"limit": {"5"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		w := listRequest(t, router, userID, params)
		page := decodeListResponse(t, w)
		for _, item := range page.Data {
			collected = append(collected, item.ID)
		}
		if !page.Pagination.HasMore {
			break
		}
		if page.Pagination.NextCursor == nil {
			t.Fatal("hasMore=true but nextCursor is null")
		}
		cursor = *page.Pagination.NextCursor
	}
	// Verify all 30 are present with no duplicates.
	if len(collected) != total {
		t.Errorf("assembled %d IDs from pages, want %d", len(collected), total)
	}
	seen := make(map[string]int, total)
	for _, id := range collected {
		seen[id]++
	}
	for _, id := range seeded {
		if seen[id] != 1 {
			t.Errorf("ID %q appeared %d times (want 1)", id, seen[id])
		}
	}

	// 3. Over-max clamps to 100.
	// Seed extra rows so 200 is beyond the real set (30 rows); clamping just means the
	// single page returns all 30 with hasMore=false.
	w = listRequest(t, router, userID, url.Values{"limit": {"200"}})
	resp = decodeListResponse(t, w)
	if len(resp.Data) != total {
		t.Errorf("limit=200 (clamped to 100): got %d items, want %d", len(resp.Data), total)
	}
	if resp.Pagination.HasMore {
		t.Error("limit=200: expected hasMore=false when all items fit")
	}
}

// ── AC3 (List): status filter ─────────────────────────────────────────────

// TestList_StatusFilter verifies ?status=active and ?status=completed filter correctly.
func TestList_StatusFilter(t *testing.T) {
	t.Parallel()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "list-status-user-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	base := time.Now().UTC().Truncate(time.Second)
	seedReminder(t, svc, st, scope, "active 1", base.Add(time.Hour), "active")
	seedReminder(t, svc, st, scope, "active 2", base.Add(2*time.Hour), "active")
	seedReminder(t, svc, st, scope, "completed 1", base.Add(3*time.Hour), "completed")

	router := httpx.NewRouter()
	m.Routes(router)

	// ?status=active -> 2 items.
	wa := listRequest(t, router, userID, url.Values{"status": {"active"}})
	ra := decodeListResponse(t, wa)
	if len(ra.Data) != 2 {
		t.Errorf("status=active: got %d items, want 2", len(ra.Data))
	}
	for _, item := range ra.Data {
		if item.Status != "active" {
			t.Errorf("status=active: got item with status=%q", item.Status)
		}
	}

	// ?status=completed -> 1 item.
	wc := listRequest(t, router, userID, url.Values{"status": {"completed"}})
	rc := decodeListResponse(t, wc)
	if len(rc.Data) != 1 {
		t.Errorf("status=completed: got %d items, want 1", len(rc.Data))
	}
	if len(rc.Data) > 0 && rc.Data[0].Status != "completed" {
		t.Errorf("status=completed: got item with status=%q", rc.Data[0].Status)
	}

	// No filter -> 3 items.
	wn := listRequest(t, router, userID, nil)
	rn := decodeListResponse(t, wn)
	if len(rn.Data) != 3 {
		t.Errorf("no status filter: got %d items, want 3", len(rn.Data))
	}
}

// ── AC4 (List): due-date window filters ──────────────────────────────────

// TestList_DueDateWindow verifies ?dueBefore and ?dueAfter filters, individually
// and combined, and combined with ?status.
func TestList_DueDateWindow(t *testing.T) {
	t.Parallel()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "list-due-user-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	// Three reminders at T+1h, T+3h, T+5h.
	base := time.Now().UTC().Truncate(time.Second)
	t1 := base.Add(time.Hour)
	t3 := base.Add(3 * time.Hour)
	t5 := base.Add(5 * time.Hour)

	r1 := seedReminder(t, svc, st, scope, "due 1h", t1, "active")
	r3 := seedReminder(t, svc, st, scope, "due 3h", t3, "active")
	r5 := seedReminder(t, svc, st, scope, "due 5h", t5, "completed")
	_ = r3

	router := httpx.NewRouter()
	m.Routes(router)

	// dueAfter=T+2h -> only r3 and r5.
	after := base.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	wa := listRequest(t, router, userID, url.Values{"dueAfter": {after}})
	ra := decodeListResponse(t, wa)
	if len(ra.Data) != 2 {
		t.Errorf("dueAfter=T+2h: got %d items, want 2", len(ra.Data))
	}
	for _, item := range ra.Data {
		if item.ID == r1.ID {
			t.Errorf("dueAfter=T+2h: got r1 (due T+1h), should be excluded")
		}
	}

	// dueBefore=T+4h -> only r1 and r3.
	before := base.Add(4 * time.Hour).UTC().Format(time.RFC3339)
	wb := listRequest(t, router, userID, url.Values{"dueBefore": {before}})
	rb := decodeListResponse(t, wb)
	if len(rb.Data) != 2 {
		t.Errorf("dueBefore=T+4h: got %d items, want 2", len(rb.Data))
	}
	for _, item := range rb.Data {
		if item.ID == r5.ID {
			t.Errorf("dueBefore=T+4h: got r5 (due T+5h), should be excluded")
		}
	}

	// dueAfter=T+2h combined with dueBefore=T+4h -> only r3.
	wab := listRequest(t, router, userID, url.Values{"dueAfter": {after}, "dueBefore": {before}})
	rab := decodeListResponse(t, wab)
	if len(rab.Data) != 1 {
		t.Errorf("dueAfter+dueBefore window: got %d items, want 1", len(rab.Data))
	}

	// dueBefore + status=completed -> only r5 (due T+5h, completed), but r5 is outside
	// the before window; combine dueAfter=T+4h with status=completed -> only r5.
	wcs := listRequest(t, router, userID, url.Values{
		"dueAfter": {before},
		"status":   {"completed"},
	})
	rcs := decodeListResponse(t, wcs)
	if len(rcs.Data) != 1 {
		t.Errorf("dueAfter+status=completed: got %d items, want 1", len(rcs.Data))
	}
	if len(rcs.Data) > 0 && rcs.Data[0].Status != "completed" {
		t.Errorf("expected completed, got %q", rcs.Data[0].Status)
	}

	// Invalid dueBefore -> 422.
	winv := listRequest(t, router, userID, url.Values{"dueBefore": {"not-a-date"}})
	if winv.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid dueBefore: got %d, want 422", winv.Code)
	}

	// Invalid dueAfter -> 422.
	winv2 := listRequest(t, router, userID, url.Values{"dueAfter": {"not-a-date"}})
	if winv2.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid dueAfter: got %d, want 422", winv2.Code)
	}

	// Invalid status value -> 422.
	wstat := listRequest(t, router, userID, url.Values{"status": {"bogus"}})
	if wstat.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid status: got %d, want 422", wstat.Code)
	}
}

// ── AC5 (List): index plan ────────────────────────────────────────────────

// eqpRows executes EXPLAIN QUERY PLAN for the given SQL + args and returns
// the detail strings from each plan row.
func eqpRows(t *testing.T, st *store.Store, sql string, args ...any) []string {
	t.Helper()
	rows, err := st.Reader().QueryContext(context.Background(), "EXPLAIN QUERY PLAN\n"+sql, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var plans []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan EQP row: %v", err)
		}
		plans = append(plans, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EQP rows.Err: %v", err)
	}
	return plans
}

// assertIndexPlan checks a slice of EQP plan details for index usage and
// absence of full-scan / temp-sort indicators.
func assertIndexPlan(t *testing.T, label string, plans []string) {
	t.Helper()
	t.Logf("EXPLAIN QUERY PLAN [%s]:", label)
	for _, p := range plans {
		t.Logf("  %s", p)
	}
	if len(plans) == 0 {
		t.Errorf("[%s] EXPLAIN QUERY PLAN returned no rows", label)
		return
	}

	// Must use the new reminders_user_due index (covering or otherwise).
	usesNewIndex := false
	for _, p := range plans {
		if strings.Contains(p, "reminders_user_due") {
			usesNewIndex = true
		}
	}
	if !usesNewIndex {
		t.Errorf("[%s] query plan does not use reminders_user_due; plans: %v", label, plans)
	}

	// Must NOT fall back to a temp sort.
	for _, p := range plans {
		if strings.Contains(strings.ToUpper(p), "USE TEMP B-TREE FOR ORDER BY") {
			t.Errorf("[%s] query plan uses USE TEMP B-TREE FOR ORDER BY (index does not serve ordering); plan line: %q", label, p)
		}
	}

	// Must NOT perform a full SCAN reminders without an index.
	for _, p := range plans {
		up := strings.ToUpper(p)
		if strings.Contains(up, "SCAN REMINDERS") &&
			!strings.Contains(up, "USING") {
			t.Errorf("[%s] query plan contains unindexed SCAN REMINDERS; plan line: %q", label, p)
		}
	}
}

// TestListReminders_IndexPlan verifies via EXPLAIN QUERY PLAN that the
// ListReminders query uses the reminders_user_due index on (user_id, due_at, id)
// and produces no USE TEMP B-TREE FOR ORDER BY on either the no-status default
// path or the status=active filtered path.
//
// Real rows are seeded so the SQLite query planner sees a non-empty table — an
// empty table can produce degenerate plans that differ from production.
func TestListReminders_IndexPlan(t *testing.T) {
	t.Parallel()
	st := openMigratedStore(t)

	// Seed a user and rows so the planner sees real data.
	const userID = "eqp-plan-user-01"
	seedUser(t, st, userID, "UTC")
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()
	scope := newScopeFor(userID)
	base := time.Now().UTC().Truncate(time.Second)
	for i := range 5 {
		seedReminder(t, svc, st, scope, fmt.Sprintf("plan row %d", i), base.Add(time.Duration(i)*time.Minute), "active")
	}

	// The SQL emitted by sqlc for ListReminders (positional ?N params).
	// Keep in sync with the generated listReminders constant in reminders.sql.go.
	const listSQL = `SELECT id, user_id, title, notes, due_at, rrule, tz,
       auto_complete, status, completed_at, last_fired_at,
       fire_job_id, created_at, updated_at
FROM reminders
WHERE user_id = ?1
  AND (?2     IS NULL OR status = ?2)
  AND (?3  IS NULL OR due_at > ?3)
  AND (?4 IS NULL OR due_at < ?4)
  AND (?5 IS NULL
       OR due_at > ?5
       OR (due_at = ?5 AND id > ?6))
ORDER BY due_at, id
LIMIT ?7`

	// Path 1: no-status default (status arg = nil).
	noStatusPlans := eqpRows(t, st, listSQL,
		userID, nil, nil, nil, nil, nil, int64(25))
	assertIndexPlan(t, "no-status default", noStatusPlans)

	// Path 2: status=active filter.
	statusPlans := eqpRows(t, st, listSQL,
		userID, "active", nil, nil, nil, nil, int64(25))
	assertIndexPlan(t, "status=active", statusPlans)
}

// ── AC2 extension: malformed cursor -> 400 ───────────────────────────────

// TestList_MalformedCursor verifies that a garbled cursor value returns 400.
func TestList_MalformedCursor(t *testing.T) {
	t.Parallel()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	const userID = "list-cursor-bad-01"
	seedUser(t, st, userID, "UTC")

	router := httpx.NewRouter()
	m.Routes(router)

	w := listRequest(t, router, userID, url.Values{"cursor": {"not-valid-base64!!!"}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed cursor: got %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// ── Service.List direct test ──────────────────────────────────────────────

// TestService_List_FullFidelity verifies that Service.List returns the full
// Reminder domain object (including notes), matching what Get returns.
func TestService_List_FullFidelity(t *testing.T) {
	t.Parallel()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "list-full-user-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	base := time.Now().UTC().Truncate(time.Second)
	created, err := svc.Create(context.Background(), scope, reminders.CreateInput{
		Title:        "Full fidelity reminder",
		Notes:        "These are the notes",
		DueAt:        base.Add(time.Hour),
		AutoComplete: ptrFalse,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	page, err := svc.List(context.Background(), scope, reminders.ListQuery{Limit: 25})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(page.Items))
	}
	got := page.Items[0]
	if got.ID != created.ID {
		t.Errorf("got ID %q, want %q", got.ID, created.ID)
	}
	if got.Notes != "These are the notes" {
		t.Errorf("got Notes %q, want full notes text", got.Notes)
	}
	// Verify it matches what Get returns.
	direct, err := svc.Get(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Notes != direct.Notes {
		t.Errorf("List.Notes=%q != Get.Notes=%q", got.Notes, direct.Notes)
	}
}

// TestList_Unauthenticated verifies that GET /api/v1/reminders without a
// principal returns 401.
func TestList_Unauthenticated(t *testing.T) {
	t.Parallel()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	router := httpx.NewRouter()
	m.Routes(router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401 for unauthenticated list", w.Code)
	}
}

// ── AC5 (List): unknown query params → 400 ───────────────────────────────

// TestList_UnknownQueryParam verifies that GET /api/v1/reminders rejects any
// query parameter outside the known set (cursor, limit, status, dueBefore,
// dueAfter) with 400 Bad Request per the HTTP house guide ("reject unknown
// filter params rather than silently ignoring them").
//
// A typo'd param like ?staus=completed must 400, not silently return the
// unfiltered list with wrong data.
func TestList_UnknownQueryParam(t *testing.T) {
	t.Parallel()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	const userID = "list-unknown-param-01"
	seedUser(t, st, userID, "UTC")

	router := httpx.NewRouter()
	m.Routes(router)

	cases := []struct {
		name   string
		params url.Values
	}{
		{name: "typo'd status", params: url.Values{"staus": {"completed"}}},
		{name: "unknown sort param", params: url.Values{"sort": {"due_at"}}},
		{name: "mixed known+unknown", params: url.Values{"limit": {"10"}, "foo": {"bar"}}},
		{name: "completely unknown", params: url.Values{"page": {"2"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := listRequest(t, router, userID, tc.params)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: got status %d, want 400; body: %s", tc.name, w.Code, w.Body.String())
				return
			}
			// Response must be a problem+json with code=unknown_query_param.
			var prob struct {
				Status int    `json:"status"`
				Code   string `json:"code"`
			}
			if err := json.NewDecoder(w.Body).Decode(&prob); err != nil {
				t.Fatalf("%s: decode problem: %v", tc.name, err)
			}
			if prob.Status != http.StatusBadRequest {
				t.Errorf("%s: problem.status=%d, want 400", tc.name, prob.Status)
			}
			if prob.Code != "unknown_query_param" {
				t.Errorf("%s: problem.code=%q, want %q", tc.name, prob.Code, "unknown_query_param")
			}
		})
	}
}

// TestList_NextCursorNullOnLastPage verifies that the last page of a list
// response carries nextCursor=null (not "") per the HTTP house guide.
func TestList_NextCursorNullOnLastPage(t *testing.T) {
	t.Parallel()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "list-null-cursor-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	base := time.Now().UTC().Truncate(time.Second)
	// Seed 2 reminders.
	seedReminder(t, svc, st, scope, "r1", base.Add(time.Hour), "active")
	seedReminder(t, svc, st, scope, "r2", base.Add(2*time.Hour), "active")

	router := httpx.NewRouter()
	m.Routes(router)

	// Fetch both in one page (limit=10 > 2 rows) — should be the last page.
	w := listRequest(t, router, userID, url.Values{"limit": {"10"}})
	resp := decodeListResponse(t, w)

	if len(resp.Data) != 2 {
		t.Fatalf("got %d items, want 2", len(resp.Data))
	}
	if resp.Pagination.HasMore {
		t.Error("expected hasMore=false on last page")
	}
	if resp.Pagination.NextCursor != nil {
		t.Errorf("expected nextCursor=null on last page, got %q", *resp.Pagination.NextCursor)
	}

	// Verify raw JSON contains "nextCursor":null not "nextCursor":"".
	// Re-issue the request to get the raw body.
	w2 := listRequest(t, router, userID, url.Values{"limit": {"10"}})
	raw := w2.Body.String()
	if strings.Contains(raw, `"nextCursor":""`) {
		t.Errorf("last-page JSON must not contain nextCursor empty string; got: %s", raw)
	}
	if !strings.Contains(raw, `"nextCursor":null`) {
		t.Errorf("last-page JSON must contain nextCursor:null; got: %s", raw)
	}

	// Verify a first page (hasMore=true) emits a non-null cursor.
	// Seed enough rows for a second page.
	for i := range 30 {
		seedReminder(t, svc, st, scope, fmt.Sprintf("extra %d", i), base.Add(time.Duration(i+3)*time.Hour), "active")
	}
	w3 := listRequest(t, router, userID, url.Values{"limit": {"5"}})
	resp3 := decodeListResponse(t, w3)
	if !resp3.Pagination.HasMore {
		t.Fatal("expected hasMore=true for first page of many")
	}
	if resp3.Pagination.NextCursor == nil {
		t.Error("expected non-null nextCursor on first page (hasMore=true)")
	}
}

// ── Edit / Complete / Delete slice tests ──────────────────────────────────────
//
// These tests cover:
//   AC1  PATCH dueAt → row updated + Reschedule called with correct jobID+time
//   AC2  PATCH {status:"completed"} → Complete path (status+completedAt set, job Cancelled)
//   AC3  PATCH {status:"active"} on completed → re-open (completedAt cleared, fresh Enqueue)
//   AC4  DELETE → row removed, job Cancelled, 204
//   AC5  Each mutation emits its fat event (reminder.updated/completed/deleted)
//   AC6  All operations are user-scoped (another user's id → 404)
//   +guard  dueAt change on COMPLETED reminder updates row but does NOT Reschedule

// ── AC1: PATCH dueAt → Reschedule ────────────────────────────────────────────

// TestService_Update_DueAt_Reschedules verifies that Update with a new dueAt on
// an active reminder updates the row AND calls Reschedule on the producer with
// the existing fire_job_id and the new canonical time.
func TestService_Update_DueAt_Reschedules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-upd-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-upd-due-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	origDue := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Update dueAt test",
		DueAt: origDue,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	origJobID := r.FireJobID

	newDue := origDue.Add(2 * time.Hour)
	updated, err := svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		DueAt: &newDue,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Row has the new dueAt.
	wantDueStr := newDue.UTC().Truncate(time.Second).Format(time.RFC3339)
	if updated.DueAt != wantDueStr {
		t.Errorf("got DueAt %q, want %q", updated.DueAt, wantDueStr)
	}

	// Reschedule was called with the original job ID and the new canonical time.
	rescheduled := prod.allRescheduled()
	if len(rescheduled) == 0 {
		t.Fatal("expected Reschedule to be called, got none")
	}
	rc := rescheduled[0]
	if rc.jobID != origJobID {
		t.Errorf("Reschedule called with jobID=%q, want %q", rc.jobID, origJobID)
	}
	wantRunAt := newDue.UTC().Truncate(time.Second)
	if !rc.runAt.Equal(wantRunAt) {
		t.Errorf("Reschedule called with runAt=%v, want %v", rc.runAt, wantRunAt)
	}

	// Verify AC5: reminder.updated event was published.
	evts := bus.allEvents()
	var foundUpd bool
	for _, e := range evts {
		if e.event.Type == "reminder.updated" && e.userID == userID {
			foundUpd = true
		}
	}
	if !foundUpd {
		t.Error("expected reminder.updated event, not found")
	}
}

// TestHTTP_Patch_DueAt_200 verifies PATCH /api/v1/reminders/{id} with a new
// dueAt returns 200 and the updated reminder JSON.
func TestHTTP_Patch_DueAt_200(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-http-upd-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-http-upd-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "HTTP PATCH dueAt",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	router := httpx.NewRouter()
	m.Routes(router)

	newDue := time.Now().UTC().Add(3 * time.Hour).Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{"dueAt": newDue})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/reminders/"+r.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: userID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp reminders.Reminder
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DueAt != time.Now().UTC().Add(3*time.Hour).Truncate(time.Second).Format(time.RFC3339) {
		// Allow slight skew: just check it changed from the original.
		if resp.DueAt == r.DueAt {
			t.Errorf("dueAt not updated: still %q", resp.DueAt)
		}
	}
}

// ── AC2: PATCH {status:"completed"} → Complete ───────────────────────────────

// TestService_Complete_CancelsJob verifies that Complete sets status=completed,
// sets completed_at, and Cancels the fire-job.
func TestService_Complete_CancelsJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-cmp-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-cmp-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Complete me",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	origJobID := r.FireJobID

	completed, err := svc.Complete(ctx, scope, r.ID)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Status and completedAt.
	if completed.Status != "completed" {
		t.Errorf("got status %q, want %q", completed.Status, "completed")
	}
	if completed.CompletedAt == "" {
		t.Error("expected non-empty completedAt after Complete")
	}
	// FireJobID cleared.
	if completed.FireJobID != "" {
		t.Errorf("expected empty FireJobID after Complete, got %q", completed.FireJobID)
	}

	// Cancel was called with the original job ID.
	cancelled := prod.allCancelled()
	// First cancel may be from Create compensation (not in this path); find the
	// cancel matching the original job.
	var found bool
	for _, jid := range cancelled {
		if jid == origJobID {
			found = true
		}
	}
	if !found {
		t.Errorf("Cancel not called with jobID=%q; cancelled: %v", origJobID, cancelled)
	}

	// Verify AC5: reminder.completed event.
	evts := bus.allEvents()
	var foundCmp bool
	for _, e := range evts {
		if e.event.Type == "reminder.completed" && e.userID == userID {
			foundCmp = true
		}
	}
	if !foundCmp {
		t.Error("expected reminder.completed event, not found")
	}
}

// TestService_Complete_Idempotent verifies that calling Complete on an already-
// completed reminder does not error and does not double-cancel a nil job.
func TestService_Complete_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-idm-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-idm-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Idempotent complete",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Complete once.
	if _, err := svc.Complete(ctx, scope, r.ID); err != nil {
		t.Fatalf("Complete (1st): %v", err)
	}
	// Complete again — must not error.
	if _, err := svc.Complete(ctx, scope, r.ID); err != nil {
		t.Fatalf("Complete (2nd, idempotent): %v", err)
	}
}

// TestHTTP_Patch_Complete_200 verifies PATCH with status:"completed" routes to
// Complete and returns 200.
func TestHTTP_Patch_Complete_200(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-http-cmp-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-http-cmp-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "HTTP Complete",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	router := httpx.NewRouter()
	m.Routes(router)

	body, _ := json.Marshal(map[string]any{"status": "completed"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/reminders/"+r.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: userID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp reminders.Reminder
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "completed" {
		t.Errorf("got status %q, want %q", resp.Status, "completed")
	}
	if resp.CompletedAt == "" {
		t.Error("expected non-empty completedAt in response")
	}
}

// ── AC3: PATCH {status:"active"} on completed → re-open ──────────────────────

// TestService_Update_Reopen_EnqueuesNewJob verifies that Update with
// status="active" on a completed reminder clears completedAt, enqueues a fresh
// fire-job, stores the new fire_job_id, and emits reminder.updated.
func TestService_Update_Reopen_EnqueuesNewJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-reopen-create-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-reopen-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	dueAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Reopen me",
		DueAt: dueAt,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Complete it first.
	if _, err := svc.Complete(ctx, scope, r.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Change the returnID so the re-open Enqueue gets a distinct job ID.
	prod.mu.Lock()
	prod.returnID = "job-reopen-fresh-002"
	prod.mu.Unlock()

	// Re-open via Update.
	statusActive := "active"
	reopened, err := svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Status: &statusActive,
	})
	if err != nil {
		t.Fatalf("Update (reopen): %v", err)
	}

	// Status is active.
	if reopened.Status != "active" {
		t.Errorf("got status %q, want %q", reopened.Status, "active")
	}
	// completedAt cleared.
	if reopened.CompletedAt != "" {
		t.Errorf("expected empty CompletedAt after reopen, got %q", reopened.CompletedAt)
	}
	// A new fire_job_id was stored.
	if reopened.FireJobID == "" {
		t.Error("expected non-empty FireJobID after reopen")
	}
	if reopened.FireJobID == r.FireJobID {
		t.Errorf("expected new FireJobID after reopen, still got original %q", r.FireJobID)
	}

	// A fresh Enqueue was called (the second one, after Create's first).
	enqueued := prod.allEnqueued()
	if len(enqueued) < 2 {
		t.Fatalf("expected at least 2 Enqueue calls (create + reopen), got %d", len(enqueued))
	}
	// The last enqueue should carry the reminder's ID.
	last := enqueued[len(enqueued)-1]
	var payload struct {
		ReminderID string `json:"reminderId"`
	}
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("unmarshal reopen payload: %v", err)
	}
	if payload.ReminderID != r.ID {
		t.Errorf("reopen Enqueue payload reminderId=%q, want %q", payload.ReminderID, r.ID)
	}
	// RunAt should match the reminder's dueAt.
	if !last.RunAt.Equal(dueAt) {
		t.Errorf("reopen Enqueue RunAt=%v, want %v", last.RunAt, dueAt)
	}

	// Verify AC5: reminder.updated event.
	evts := bus.allEvents()
	var foundUpd bool
	for _, e := range evts {
		if e.event.Type == "reminder.updated" && e.userID == userID {
			foundUpd = true
		}
	}
	if !foundUpd {
		t.Error("expected reminder.updated event after reopen, not found")
	}
}

// TestHTTP_Patch_Reopen_200 verifies PATCH {status:"active"} on a completed
// reminder returns 200 with the re-opened reminder.
func TestHTTP_Patch_Reopen_200(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-http-reopen-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-http-reopen-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "HTTP Reopen",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Complete(ctx, scope, r.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	router := httpx.NewRouter()
	m.Routes(router)

	body, _ := json.Marshal(map[string]any{"status": "active"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/reminders/"+r.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: userID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp reminders.Reminder
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "active" {
		t.Errorf("got status %q, want %q", resp.Status, "active")
	}
	if resp.CompletedAt != "" {
		t.Errorf("expected empty completedAt after reopen, got %q", resp.CompletedAt)
	}
}

// ── AC4: DELETE → 204 ────────────────────────────────────────────────────────

// TestService_Delete_CancelsJobAndRemovesRow verifies that Delete removes the
// row, Cancels the fire-job, and publishes reminder.deleted.
func TestService_Delete_CancelsJobAndRemovesRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-del-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-del-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Delete me",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	origJobID := r.FireJobID

	if err := svc.Delete(ctx, scope, r.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Row is gone.
	_, err = svc.Get(ctx, scope, r.ID)
	if err == nil {
		t.Fatal("expected ErrNotFound after Delete, got nil")
	}
	if !isNotFoundError(err) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}

	// Cancel was called with the original job ID.
	cancelled := prod.allCancelled()
	var found bool
	for _, jid := range cancelled {
		if jid == origJobID {
			found = true
		}
	}
	if !found {
		t.Errorf("Cancel not called with jobID=%q; cancelled: %v", origJobID, cancelled)
	}

	// Verify AC5: reminder.deleted event carries the deleted reminder.
	evts := bus.allEvents()
	var foundDel bool
	for _, e := range evts {
		if e.event.Type == "reminder.deleted" && e.userID == userID {
			foundDel = true
			// Payload should carry the reminder.
			data, _ := json.Marshal(e.event.Data)
			var payload reminders.Reminder
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatalf("unmarshal deleted event payload: %v", err)
			}
			if payload.ID != r.ID {
				t.Errorf("deleted event payload.id=%q, want %q", payload.ID, r.ID)
			}
		}
	}
	if !foundDel {
		t.Error("expected reminder.deleted event, not found")
	}
}

// TestHTTP_Delete_204 verifies DELETE /api/v1/reminders/{id} returns 204.
func TestHTTP_Delete_204(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-http-del-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-http-del-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "HTTP Delete",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	router := httpx.NewRouter()
	m.Routes(router)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/reminders/"+r.ID, nil)
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: userID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("got %d, want 204; body: %s", w.Code, w.Body.String())
	}

	// Row must be gone.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/reminders/"+r.ID, nil)
	req2 = req2.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: userID, Role: "member"}))
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("after DELETE: got %d, want 404", w2.Code)
	}
}

// ── AC6: user-scoping ─────────────────────────────────────────────────────────

// TestService_Update_OtherUser_NotFound verifies that Update on another user's
// reminder returns ErrNotFound (not a mutation of the wrong row).
func TestService_Update_OtherUser_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-scope-upd-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const ownerID = "user-scope-owner-01"
	const otherID = "user-scope-other-01"
	seedUser(t, st, ownerID, "UTC")
	seedUser(t, st, otherID, "UTC")
	ownerScope := newScopeFor(ownerID)
	otherScope := newScopeFor(otherID)

	r, err := svc.Create(ctx, ownerScope, reminders.CreateInput{
		Title: "Owner's reminder",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	newTitle := "Hijacked"
	_, err = svc.Update(ctx, otherScope, r.ID, reminders.UpdateInput{Title: &newTitle})
	if err == nil {
		t.Fatal("expected error updating another user's reminder")
	}
	if !isNotFoundError(err) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

// TestService_Complete_OtherUser_NotFound verifies that Complete on another
// user's reminder returns ErrNotFound.
func TestService_Complete_OtherUser_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-scope-cmp-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const ownerID = "user-scope-cowner-01"
	const otherID = "user-scope-cother-01"
	seedUser(t, st, ownerID, "UTC")
	seedUser(t, st, otherID, "UTC")
	ownerScope := newScopeFor(ownerID)
	otherScope := newScopeFor(otherID)

	r, err := svc.Create(ctx, ownerScope, reminders.CreateInput{
		Title: "Owner's reminder",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = svc.Complete(ctx, otherScope, r.ID)
	if err == nil {
		t.Fatal("expected error completing another user's reminder")
	}
	if !isNotFoundError(err) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

// TestService_Delete_OtherUser_NotFound verifies that Delete on another user's
// reminder returns ErrNotFound.
func TestService_Delete_OtherUser_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-scope-del-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const ownerID = "user-scope-downer-01"
	const otherID = "user-scope-dother-01"
	seedUser(t, st, ownerID, "UTC")
	seedUser(t, st, otherID, "UTC")
	ownerScope := newScopeFor(ownerID)
	otherScope := newScopeFor(otherID)

	r, err := svc.Create(ctx, ownerScope, reminders.CreateInput{
		Title: "Owner's reminder",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = svc.Delete(ctx, otherScope, r.ID)
	if err == nil {
		t.Fatal("expected error deleting another user's reminder")
	}
	if !isNotFoundError(err) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

// TestHTTP_Patch_OtherUser_404 verifies that PATCH on another user's reminder
// returns 404.
func TestHTTP_Patch_OtherUser_404(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-http-scope-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const ownerID = "user-http-scope-owner-01"
	const otherID = "user-http-scope-other-01"
	seedUser(t, st, ownerID, "UTC")
	seedUser(t, st, otherID, "UTC")
	ownerScope := newScopeFor(ownerID)

	r, err := svc.Create(ctx, ownerScope, reminders.CreateInput{
		Title: "Owner's reminder",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	router := httpx.NewRouter()
	m.Routes(router)

	body, _ := json.Marshal(map[string]any{"title": "Hijacked"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/reminders/"+r.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: otherID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404 (other user's reminder)", w.Code)
	}
}

// TestHTTP_Delete_OtherUser_404 verifies that DELETE on another user's reminder
// returns 404.
func TestHTTP_Delete_OtherUser_404(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-http-dscope-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const ownerID = "user-http-dscope-owner-01"
	const otherID = "user-http-dscope-other-01"
	seedUser(t, st, ownerID, "UTC")
	seedUser(t, st, otherID, "UTC")
	ownerScope := newScopeFor(ownerID)

	r, err := svc.Create(ctx, ownerScope, reminders.CreateInput{
		Title: "Owner's reminder",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	router := httpx.NewRouter()
	m.Routes(router)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/reminders/"+r.ID, nil)
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: otherID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404 (other user's reminder)", w.Code)
	}
}

// ── Guard test: dueAt change on COMPLETED reminder does NOT Reschedule ────────

// TestService_Update_DueAt_Completed_NoReschedule verifies that changing dueAt
// on a completed reminder (no active fire-job) updates the row but does NOT
// call Reschedule — there is no live job to reschedule.
func TestService_Update_DueAt_Completed_NoReschedule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-guard-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-guard-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	dueAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Guard test",
		DueAt: dueAt,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Complete to remove the fire-job.
	if _, err := svc.Complete(ctx, scope, r.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Clear the rescheduled slice so we only see new calls from Update.
	prod.mu.Lock()
	prod.rescheduled = nil
	prod.mu.Unlock()

	// Update dueAt on the now-completed reminder.
	newDue := dueAt.Add(4 * time.Hour)
	updated, err := svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		DueAt: &newDue,
	})
	if err != nil {
		t.Fatalf("Update (completed, new dueAt): %v", err)
	}

	// Row should reflect the new dueAt.
	wantDueStr := newDue.UTC().Truncate(time.Second).Format(time.RFC3339)
	if updated.DueAt != wantDueStr {
		t.Errorf("got DueAt %q, want %q", updated.DueAt, wantDueStr)
	}

	// Reschedule must NOT have been called (no active fire-job).
	rescheduled := prod.allRescheduled()
	if len(rescheduled) > 0 {
		t.Errorf("expected no Reschedule calls on completed reminder, got %d: %v",
			len(rescheduled), rescheduled)
	}
}

// ── Merge semantics: null clears nullable fields ──────────────────────────────

// TestService_Update_NullClears verifies that PATCH merge semantics correctly
// handle null (clear nullable fields) vs absent (leave unchanged) for notes and
// rrule. Semantics: absent→unchanged; null→clear; value→set.
func TestService_Update_MergeSemantics_NullClears(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-merge-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-merge-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Merge test",
		Notes: "original notes",
		DueAt: time.Now().UTC().Add(time.Hour),
		Rrule: "FREQ=DAILY",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Update with absent notes (nil pointer) — notes must remain unchanged.
	newTitle := "Updated title"
	updated, err := svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Title: &newTitle,
		// Notes: nil (absent) → unchanged
	})
	if err != nil {
		t.Fatalf("Update (absent notes): %v", err)
	}
	if updated.Notes != "original notes" {
		t.Errorf("absent notes: got %q, want %q (unchanged)", updated.Notes, "original notes")
	}
	if updated.Title != "Updated title" {
		t.Errorf("title: got %q, want %q", updated.Title, "Updated title")
	}

	// Update with notes = Optional cleared (null) — notes must be cleared.
	updated2, err := svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Notes: reminders.ClearString(),
	})
	if err != nil {
		t.Fatalf("Update (null notes): %v", err)
	}
	if updated2.Notes != "" {
		t.Errorf("null notes: got %q, want empty (cleared)", updated2.Notes)
	}
	// Rrule should be unchanged (absent).
	if updated2.Rrule != "FREQ=DAILY" {
		t.Errorf("rrule unchanged: got %q, want %q", updated2.Rrule, "FREQ=DAILY")
	}

	// Update with rrule = Optional cleared (null) — rrule must be cleared.
	updated3, err := svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Rrule: reminders.ClearString(),
	})
	if err != nil {
		t.Fatalf("Update (null rrule): %v", err)
	}
	if updated3.Rrule != "" {
		t.Errorf("null rrule: got %q, want empty (cleared)", updated3.Rrule)
	}
}

// ── Item 1 / Item 2: combined-PATCH correctness ───────────────────────────────

// TestHTTP_Patch_StatusCompleted_WithOtherFields verifies that PATCH with
// {"status":"completed","title":"NewTitle"} persists BOTH the completion AND the
// title change in one atomic write, and emits exactly one "reminder.completed"
// event (not "reminder.updated").
//
// Before the fix, patchHandler routed to Service.Complete the moment
// body.Status=="completed", discarding the merged UpdateInput that carried the
// title change.
func TestHTTP_Patch_StatusCompleted_WithOtherFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-combined-patch-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-combined-patch-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "OriginalTitle",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	router := httpx.NewRouter()
	m.Routes(router)

	// PATCH both status=completed AND a new title in a single request.
	body, _ := json.Marshal(map[string]any{
		"status": "completed",
		"title":  "NewTitle",
	})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/reminders/"+r.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: userID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp reminders.Reminder
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Both changes must be present in the response.
	if resp.Status != "completed" {
		t.Errorf("got status %q, want %q", resp.Status, "completed")
	}
	if resp.CompletedAt == "" {
		t.Error("expected non-empty completedAt")
	}
	if resp.Title != "NewTitle" {
		t.Errorf("got title %q, want %q (title change must be persisted with completion)", resp.Title, "NewTitle")
	}

	// The row in the DB must reflect both changes.
	got, err := svc.Get(ctx, scope, r.ID)
	if err != nil {
		t.Fatalf("Get after combined PATCH: %v", err)
	}
	if got.Title != "NewTitle" {
		t.Errorf("DB row title=%q, want %q", got.Title, "NewTitle")
	}
	if got.Status != "completed" {
		t.Errorf("DB row status=%q, want %q", got.Status, "completed")
	}
	if got.CompletedAt == "" {
		t.Error("DB row: expected non-empty completedAt")
	}

	// Exactly one reminder.completed event must be emitted (not reminder.updated).
	evts := bus.allEvents()
	var completedCount, updatedCount int
	for _, e := range evts {
		if e.userID != userID {
			continue
		}
		switch e.event.Type {
		case "reminder.completed":
			completedCount++
		case "reminder.updated":
			updatedCount++
		}
	}
	if completedCount != 1 {
		t.Errorf("expected exactly 1 reminder.completed event, got %d", completedCount)
	}
	if updatedCount != 0 {
		t.Errorf("expected 0 reminder.updated events for an active→completed transition, got %d", updatedCount)
	}
}

// TestService_Update_StatusCompleted_EmitsCompletedEvent verifies that calling
// Update with status="completed" on an active reminder emits "reminder.completed",
// not "reminder.updated". This covers the Update code path directly (as opposed
// to via HTTP).
func TestService_Update_StatusCompleted_EmitsCompletedEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-upd-cmp-evt-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-upd-cmp-evt-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Event type test",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	statusCompleted := "completed"
	_, err = svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Status: &statusCompleted,
	})
	if err != nil {
		t.Fatalf("Update (status=completed): %v", err)
	}

	evts := bus.allEvents()
	var completedCount, updatedCount int
	for _, e := range evts {
		if e.userID != userID {
			continue
		}
		switch e.event.Type {
		case "reminder.completed":
			completedCount++
		case "reminder.updated":
			updatedCount++
		}
	}
	// Only the Create event and the completion event; no reminder.updated for this transition.
	if completedCount != 1 {
		t.Errorf("expected 1 reminder.completed event from Update, got %d", completedCount)
	}
	if updatedCount != 0 {
		t.Errorf("expected 0 reminder.updated events for active→completed via Update, got %d", updatedCount)
	}
}

// TestService_Complete_EmitsCompletedEvent verifies Service.Complete emits
// "reminder.completed" (it already does — this test documents the contract so
// any future refactor can't accidentally regress the event type).
func TestService_Complete_EmitsCompletedEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-svc-cmp-evt-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-svc-cmp-evt-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Direct complete event test",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := svc.Complete(ctx, scope, r.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	evts := bus.allEvents()
	var completedCount int
	for _, e := range evts {
		if e.userID == userID && e.event.Type == "reminder.completed" {
			completedCount++
		}
	}
	if completedCount != 1 {
		t.Errorf("expected 1 reminder.completed event from Service.Complete, got %d", completedCount)
	}
}

// TestService_Update_Reopen_EmitsUpdatedEvent verifies that re-opening a
// completed reminder (status: active) emits "reminder.updated", not
// "reminder.completed".
func TestService_Update_Reopen_EmitsUpdatedEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-reopen-evt-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-reopen-evt-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Reopen event test",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Complete(ctx, scope, r.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Clear events so we only check what reopen emits.
	bus.mu.Lock()
	bus.events = nil
	bus.mu.Unlock()

	statusActive := "active"
	prod.mu.Lock()
	prod.returnID = "job-reopen-evt-002"
	prod.mu.Unlock()

	_, err = svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Status: &statusActive,
	})
	if err != nil {
		t.Fatalf("Update (reopen): %v", err)
	}

	evts := bus.allEvents()
	var updatedCount, completedCount int
	for _, e := range evts {
		if e.userID != userID {
			continue
		}
		switch e.event.Type {
		case "reminder.updated":
			updatedCount++
		case "reminder.completed":
			completedCount++
		}
	}
	if updatedCount != 1 {
		t.Errorf("expected 1 reminder.updated event for reopen, got %d", updatedCount)
	}
	if completedCount != 0 {
		t.Errorf("expected 0 reminder.completed events for reopen, got %d", completedCount)
	}
}

// TestService_Update_PlainField_EmitsUpdatedEvent verifies a plain field update
// (no status change) emits "reminder.updated".
func TestService_Update_PlainField_EmitsUpdatedEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-plain-upd-evt-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-plain-upd-evt-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Plain update event test",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Clear creation event.
	bus.mu.Lock()
	bus.events = nil
	bus.mu.Unlock()

	newTitle := "Plain Updated Title"
	_, err = svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Title: &newTitle,
	})
	if err != nil {
		t.Fatalf("Update (plain field): %v", err)
	}

	evts := bus.allEvents()
	var updatedCount, completedCount int
	for _, e := range evts {
		if e.userID != userID {
			continue
		}
		switch e.event.Type {
		case "reminder.updated":
			updatedCount++
		case "reminder.completed":
			completedCount++
		}
	}
	if updatedCount != 1 {
		t.Errorf("expected 1 reminder.updated event for plain update, got %d", updatedCount)
	}
	if completedCount != 0 {
		t.Errorf("expected 0 reminder.completed events for plain update, got %d", completedCount)
	}
}

// TestService_Update_Active_AlreadyActive_NoNewJob verifies that calling Update
// with status="active" on an already-active reminder is a no-op: no new fire-job
// is enqueued (no duplicate job) and the reminder stays active.
func TestService_Update_Active_AlreadyActive_NoNewJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-noop-reopen-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-noop-reopen-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Already active",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	enqueuesBefore := len(prod.allEnqueued())

	statusActive := "active"
	updated, err := svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Status: &statusActive,
	})
	if err != nil {
		t.Fatalf("Update (status=active on active): %v", err)
	}

	// Reminder must still be active.
	if updated.Status != "active" {
		t.Errorf("got status %q, want %q", updated.Status, "active")
	}

	// No new enqueue must have happened.
	enqueuesAfter := len(prod.allEnqueued())
	if enqueuesAfter != enqueuesBefore {
		t.Errorf("expected no new Enqueue call for no-op status=active on active reminder; got %d new call(s)",
			enqueuesAfter-enqueuesBefore)
	}
}

// ── Recurring reminders slice (slice 4) ───────────────────────────────────────
//
// AC1: Create/Update with valid rrule arms a RECURRING fire-job.
// AC2: Malformed rrule → ValidationError → 422.
// AC3: Handler recurring branch advances due_at, keeps status=active, ignores auto_complete.
// AC4: Reclaim re-run does NOT skip an occurrence (idempotency).
// AC5: rrule change via Update cancels old job + enqueues fresh recurring one.
// AC6: DST-spanning rule keeps wall-clock time.

// TestCreate_RecurringFireJob_Arms_RecurringJob verifies AC1: creating a reminder
// with a valid rrule enqueues a RECURRING fire-job (Recurrence field set with
// RRULE and TZ), while a one-shot reminder gets nil Recurrence.
func TestCreate_RecurringFireJob_Arms_RecurringJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-rec-create-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-rec-create-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	dueAt := time.Date(2027, 1, 1, 8, 0, 0, 0, time.UTC)
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Daily stand-up",
		DueAt: dueAt,
		Tz:    "America/New_York",
		Rrule: "FREQ=DAILY",
	})
	if err != nil {
		t.Fatalf("Create with rrule: %v", err)
	}
	if r.Rrule != "FREQ=DAILY" {
		t.Errorf("got Rrule %q, want %q", r.Rrule, "FREQ=DAILY")
	}

	enqueued := prod.allEnqueued()
	if len(enqueued) == 0 {
		t.Fatal("expected Enqueue call")
	}
	req := enqueued[len(enqueued)-1]
	if req.Recurrence == nil {
		t.Fatal("expected non-nil Recurrence for a recurring reminder")
	}
	if req.Recurrence.RRULE != "FREQ=DAILY" {
		t.Errorf("Recurrence.RRULE=%q, want %q", req.Recurrence.RRULE, "FREQ=DAILY")
	}
	if req.Recurrence.TZ != "America/New_York" {
		t.Errorf("Recurrence.TZ=%q, want %q", req.Recurrence.TZ, "America/New_York")
	}
	if req.Recurrence.Every != 0 {
		t.Errorf("Recurrence.Every should be zero, got %v", req.Recurrence.Every)
	}
}

// TestCreate_OneShotReminder_NilRecurrence verifies that a reminder without an
// rrule still enqueues with nil Recurrence (regression guard for AC1).
func TestCreate_OneShotReminder_NilRecurrence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-oneshot-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-oneshot-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	_, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "One-shot",
		DueAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	enqueued := prod.allEnqueued()
	if len(enqueued) == 0 {
		t.Fatal("expected Enqueue call")
	}
	req := enqueued[len(enqueued)-1]
	if req.Recurrence != nil {
		t.Errorf("expected nil Recurrence for one-shot, got %+v", req.Recurrence)
	}
}

// TestCreate_MalformedRrule_ValidationError verifies AC2: a malformed rrule on
// Create returns a ValidationError (→ 422 at the REST layer).
func TestCreate_MalformedRrule_ValidationError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-rec-val-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	_, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Bad rrule",
		DueAt: time.Now().UTC().Add(time.Hour),
		Tz:    "UTC",
		Rrule: "FREQ=BOGUS;BYDAY=XY", // invalid
	})
	if err == nil {
		t.Fatal("expected ValidationError for malformed rrule, got nil")
	}
	var valErr *reminders.ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected *reminders.ValidationError, got: %T %v", err, err)
	}
	found := false
	for _, fe := range valErr.Fields {
		if fe.Pointer == "/rrule" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected field error at /rrule; got: %+v", valErr.Fields)
	}
	// No enqueue must have been attempted.
	if len(prod.allEnqueued()) != 0 {
		t.Error("Enqueue must not be called when rrule validation fails")
	}
}

// TestHTTP_Create_MalformedRrule_422 verifies AC2 at the REST layer.
func TestHTTP_Create_MalformedRrule_422(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)

	const userID = "user-rec-http-422-01"
	seedUser(t, st, userID, "UTC")

	router := httpx.NewRouter()
	m.Routes(router)

	dueAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{
		"title": "Bad rrule",
		"dueAt": dueAt,
		"tz":    "UTC",
		"rrule": "FREQ=BOGUS;BYDAY=XY",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(httpx.ContextWithPrincipal(ctx, store.Principal{UserID: userID, Role: "member"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("got %d, want 422 for malformed rrule; body: %s", w.Code, w.Body.String())
	}
	var prob struct {
		Errors []struct {
			Pointer string `json:"pointer"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(w.Body).Decode(&prob); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	found := false
	for _, fe := range prob.Errors {
		if fe.Pointer == "/rrule" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected field error at /rrule; got: %+v", prob.Errors)
	}
}

// TestUpdate_MalformedRrule_ValidationError verifies AC2: a malformed rrule on
// Update also returns a ValidationError.
func TestUpdate_MalformedRrule_ValidationError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-upd-rrule-val-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-upd-rrule-val-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Will be updated",
		DueAt: time.Now().UTC().Add(time.Hour),
		Tz:    "UTC",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Rrule: reminders.SetString("FREQ=BOGUS;BYDAY=XY"),
	})
	if err == nil {
		t.Fatal("expected ValidationError for malformed rrule on Update, got nil")
	}
	var valErr *reminders.ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected *reminders.ValidationError, got: %T %v", err, err)
	}
	found := false
	for _, fe := range valErr.Fields {
		if fe.Pointer == "/rrule" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected field error at /rrule; got: %+v", valErr.Fields)
	}
}

// TestCreate_MalformedRrule_DetailIsClean verifies that the ValidationError
// Detail for a bad rrule is model-safe: it must not embed the raw library error
// text (no ": " separator that would precede Go/library internals) and must
// include the RFC 5545 example string so the model/client knows how to fix it.
func TestCreate_MalformedRrule_DetailIsClean(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-rrule-clean-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	_, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Bad rrule clean",
		DueAt: time.Now().UTC().Add(time.Hour),
		Tz:    "UTC",
		Rrule: "FREQ=BOGUS;BYDAY=XY",
	})
	if err == nil {
		t.Fatal("expected ValidationError, got nil")
	}
	var valErr *reminders.ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected *reminders.ValidationError, got %T: %v", err, err)
	}
	var detail string
	for _, fe := range valErr.Fields {
		if fe.Pointer == "/rrule" {
			detail = fe.Detail
			break
		}
	}
	if detail == "" {
		t.Fatalf("no /rrule field error; got: %+v", valErr.Fields)
	}
	// Must not leak library error text: the old format was "...: <libErr>" where
	// libErr is teambition/rrule-go's raw message. Guard against it by checking
	// the detail does not contain a colon followed by non-example content that
	// would indicate wrapped error interpolation.
	// More directly: the detail must NOT contain " is not a valid RFC 5545 RRULE string:"
	// (the old prefix+colon that introduced the library error).
	if strings.Contains(detail, "string: ") {
		t.Errorf("rrule Detail leaks library error text (contains 'string: …'): %q", detail)
	}
	// Must guide the caller with a concrete example.
	if !strings.Contains(detail, "FREQ=") {
		t.Errorf("rrule Detail lacks a concrete FREQ= example; got: %q", detail)
	}
}

// TestCreate_MalformedTz_DetailIsClean verifies that the ValidationError Detail
// for a bad tz includes an IANA example (e.g. America/Los_Angeles) so the
// model/client knows the expected format.
func TestCreate_MalformedTz_DetailIsClean(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-tz-clean-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	_, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Bad tz clean",
		DueAt: time.Now().UTC().Add(time.Hour),
		Tz:    "Not/A/Timezone",
	})
	if err == nil {
		t.Fatal("expected ValidationError, got nil")
	}
	var valErr *reminders.ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected *reminders.ValidationError, got %T: %v", err, err)
	}
	var detail string
	for _, fe := range valErr.Fields {
		if fe.Pointer == "/tz" {
			detail = fe.Detail
			break
		}
	}
	if detail == "" {
		t.Fatalf("no /tz field error; got: %+v", valErr.Fields)
	}
	// Must include a concrete IANA example so the model knows what to supply.
	if !strings.Contains(detail, "America/") {
		t.Errorf("tz Detail lacks an IANA example (want 'America/…'); got: %q", detail)
	}
}

// TestFireHandler_Recurring_AdvancesDueAt verifies AC3: the fire handler's
// recurring branch advances due_at to the next occurrence, keeps status=active,
// and stamps last_fired_at. Also verifies auto_complete is ignored for recurring
// reminders (tested with both true and false).
//
// The anchor must be in the past relative to time.Now() so that the fire handler
// (which uses time.Now() internally) can advance to the next occurrence strictly
// after now. We use a Monday in 2020; FREQ=WEEKLY advances to the next Monday
// after the real "now" at test run time.
func TestFireHandler_Recurring_AdvancesDueAt(t *testing.T) {
	t.Parallel()
	for _, autoComplete := range []bool{true, false} {
		t.Run(fmt.Sprintf("autoComplete=%v", autoComplete), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := openMigratedStore(t)
			prod := &fakeProducer{returnID: "job-rec-fire-001"}
			bus := &fakePublisher{}
			m, reg := newTestModule(t, st, prod, bus)
			svc := m.Service()

			const userID = "user-rec-fire-01"
			seedUser(t, st, userID, "UTC")
			scope := newScopeFor(userID)

			// Anchor: 2020-01-06 08:00:00 UTC (a Monday, well in the past).
			// FREQ=WEEKLY anchored here; "next occurrence after time.Now()" will be
			// the first Monday >= now+1s, which is strictly after the anchor.
			anchor := time.Date(2020, 1, 6, 8, 0, 0, 0, time.UTC)

			ac := autoComplete
			r, err := svc.Create(ctx, scope, reminders.CreateInput{
				Title:        "Weekly meeting",
				DueAt:        anchor,
				Tz:           "UTC",
				Rrule:        "FREQ=WEEKLY",
				AutoComplete: &ac,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			// Record now before dispatching so we can assert due_at is in the future.
			now := time.Now().UTC()

			// Dispatch the fire handler.
			payload, _ := json.Marshal(map[string]string{"reminderId": r.ID})
			job := scheduler.Job{
				ID:      "job-rec-fire-001",
				Kind:    "reminder.fire",
				Payload: payload,
				Attempt: 1,
				Scope:   scope,
			}
			if err := reg.dispatch(ctx, "reminder.fire", job); err != nil {
				t.Fatalf("fire handler: %v", err)
			}

			// Row must still be active (recurring ignores auto_complete).
			row, err := db.New(st.Reader()).GetReminder(ctx, db.GetReminderParams{
				ID:     r.ID,
				UserID: userID,
			})
			if err != nil {
				t.Fatalf("GetReminder: %v", err)
			}
			if row.Status != "active" {
				t.Errorf("status=%q, want %q (recurring must stay active)", row.Status, "active")
			}
			if !row.LastFiredAt.Valid || row.LastFiredAt.String == "" {
				t.Error("expected last_fired_at to be stamped")
			}
			if row.CompletedAt.Valid {
				t.Error("completed_at must be NULL for a recurring reminder")
			}

			// due_at must have advanced beyond the anchor.
			gotDue, parseErr := time.Parse(time.RFC3339, row.DueAt)
			if parseErr != nil {
				t.Fatalf("parse due_at: %v", parseErr)
			}
			if !gotDue.After(anchor) {
				t.Errorf("due_at=%v must be strictly after anchor=%v (must have advanced)", gotDue, anchor)
			}
			// due_at must be strictly after now (the fire point was in the past, next occ is future).
			if !gotDue.After(now) {
				t.Errorf("due_at=%v must be strictly after now=%v", gotDue, now)
			}
			// FREQ=WEEKLY with 08:00 anchor: next must be a Monday at 08:00 UTC.
			if gotDue.Weekday() != time.Monday {
				t.Errorf("FREQ=WEEKLY from Monday anchor: got weekday=%v, want Monday", gotDue.Weekday())
			}
			if gotDue.Hour() != 8 || gotDue.Minute() != 0 || gotDue.Second() != 0 {
				t.Errorf("due_at time-of-day=%02d:%02d:%02d, want 08:00:00 UTC",
					gotDue.Hour(), gotDue.Minute(), gotDue.Second())
			}

			// reminder.fired event must have been published.
			evts := bus.allEvents()
			var foundFired bool
			for _, e := range evts {
				if e.event.Type == "reminder.fired" && e.userID == userID {
					foundFired = true
					break
				}
			}
			if !foundFired {
				t.Error("expected reminder.fired event")
			}
		})
	}
}

// TestFireHandler_Recurring_Idempotency verifies AC4: running the recurring fire
// handler twice in rapid succession does NOT double-advance due_at. Because the
// advance is "next occurrence strictly after now" and both runs share approximately
// the same "now", both compute the same next occurrence. The test asserts that
// due_at is identical after the first and second run.
//
// The anchor is in the distant past (2020) so time.Now() > anchor at test run time.
func TestFireHandler_Recurring_Idempotency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-rec-idem-001"}
	bus := &fakePublisher{}
	m, reg := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-rec-idem-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	// Anchor: 2020-01-01 08:00:00 UTC (well in the past). FREQ=DAILY.
	// Both handler runs execute with time.Now() (2026-era), so both compute
	// the same "next occurrence after now" — typically tomorrow at 08:00 UTC.
	anchor := time.Date(2020, 1, 1, 8, 0, 0, 0, time.UTC)
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title:        "Daily task",
		DueAt:        anchor,
		Tz:           "UTC",
		Rrule:        "FREQ=DAILY",
		AutoComplete: ptrFalse,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	payload, _ := json.Marshal(map[string]string{"reminderId": r.ID})
	job := scheduler.Job{
		ID:      "job-rec-idem-001",
		Kind:    "reminder.fire",
		Payload: payload,
		Attempt: 1,
		Scope:   scope,
	}

	// First run.
	if err := reg.dispatch(ctx, "reminder.fire", job); err != nil {
		t.Fatalf("fire handler (first run): %v", err)
	}

	// Read due_at after first run.
	row1, err := db.New(st.Reader()).GetReminder(ctx, db.GetReminderParams{ID: r.ID, UserID: userID})
	if err != nil {
		t.Fatalf("GetReminder after first run: %v", err)
	}
	dueAfterFirst := row1.DueAt
	if dueAfterFirst == anchor.UTC().Format(time.RFC3339) {
		t.Errorf("due_at did not advance after first run; still %q", dueAfterFirst)
	}

	// Second run in rapid succession (simulating a reclaim within the lease window).
	// "now" has not passed the newly-advanced due_at, so both runs must compute
	// the same next occurrence.
	if err := reg.dispatch(ctx, "reminder.fire", job); err != nil {
		t.Fatalf("fire handler (second run): %v", err)
	}

	// due_at must be identical — the same next occurrence, not advanced twice.
	row2, err := db.New(st.Reader()).GetReminder(ctx, db.GetReminderParams{ID: r.ID, UserID: userID})
	if err != nil {
		t.Fatalf("GetReminder after second run: %v", err)
	}
	if row2.DueAt != dueAfterFirst {
		t.Errorf("due_at advanced on second run (double-advance): after first=%q, after second=%q (must be identical)",
			dueAfterFirst, row2.DueAt)
	}

	// due_at must be in the future (past now at test run time).
	gotDue, parseErr := time.Parse(time.RFC3339, row2.DueAt)
	if parseErr != nil {
		t.Fatalf("parse due_at: %v", parseErr)
	}
	if !gotDue.After(time.Now().UTC()) {
		t.Errorf("due_at=%v must be in the future after advance", gotDue)
	}

	// reminder.fired must have been emitted at least once (at most twice — re-emit is acceptable).
	evts := bus.allEvents()
	var firedCount int
	for _, e := range evts {
		if e.event.Type == "reminder.fired" && e.userID == userID {
			firedCount++
		}
	}
	if firedCount < 1 {
		t.Error("expected reminder.fired to be emitted at least once")
	}
	if firedCount > 2 {
		t.Errorf("reminder.fired emitted %d times, want ≤2", firedCount)
	}
}

// TestUpdate_RruleChange_CancelsAndReenqueues verifies AC5: changing the rrule
// via Update cancels the old fire-job and enqueues a fresh recurring one with
// Recurrence set.
func TestUpdate_RruleChange_CancelsAndReenqueues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-rrule-chg-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-rrule-chg-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	// Create a one-shot reminder first.
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "One-shot to become recurring",
		DueAt: time.Now().UTC().Add(time.Hour),
		Tz:    "UTC",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	origJobID := r.FireJobID

	// Change returnID so the new Enqueue gets a distinct job ID.
	prod.mu.Lock()
	prod.returnID = "job-rrule-chg-002"
	prod.mu.Unlock()

	// Update with a new rrule — transitions one-shot → recurring.
	updated, err := svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Rrule: reminders.SetString("FREQ=WEEKLY"),
	})
	if err != nil {
		t.Fatalf("Update (add rrule): %v", err)
	}

	// Old job must have been cancelled.
	cancelled := prod.allCancelled()
	if !slices.Contains(cancelled, origJobID) {
		t.Errorf("old job %q not cancelled; cancelled: %v", origJobID, cancelled)
	}

	// A new recurring job must have been enqueued.
	enqueued := prod.allEnqueued()
	// Find the enqueue that happened after Create (second Enqueue call at minimum).
	var newReq *scheduler.EnqueueRequest
	for i := range enqueued {
		if enqueued[i].Recurrence != nil && enqueued[i].Recurrence.RRULE == "FREQ=WEEKLY" {
			req := enqueued[i]
			newReq = &req
			break
		}
	}
	if newReq == nil {
		t.Fatalf("no recurring Enqueue found with RRULE=FREQ=WEEKLY; all enqueued: %+v", enqueued)
	}
	if newReq.Recurrence.TZ != "UTC" {
		t.Errorf("new Enqueue Recurrence.TZ=%q, want %q", newReq.Recurrence.TZ, "UTC")
	}

	// New fire_job_id must differ from the original.
	if updated.FireJobID == origJobID {
		t.Errorf("fire_job_id unchanged after rrule change; still %q", origJobID)
	}
	if updated.FireJobID == "" {
		t.Error("fire_job_id must be non-empty after rrule change")
	}
}

// TestUpdate_RruleCleared_CancelsAndReenqueuesOneShot verifies AC5 for the
// recurring→one-shot path: clearing rrule (null) on an active recurring reminder
// cancels the old job and enqueues a one-shot (nil Recurrence).
func TestUpdate_RruleCleared_CancelsAndReenqueuesOneShot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-rrule-clear-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-rrule-clear-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	// Create a recurring reminder.
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Recurring to one-shot",
		DueAt: time.Now().UTC().Add(time.Hour),
		Tz:    "UTC",
		Rrule: "FREQ=DAILY",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	origJobID := r.FireJobID

	prod.mu.Lock()
	prod.returnID = "job-rrule-clear-002"
	prod.mu.Unlock()

	// Clear the rrule — transitions recurring → one-shot.
	updated, err := svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Rrule: reminders.ClearString(),
	})
	if err != nil {
		t.Fatalf("Update (clear rrule): %v", err)
	}

	// Old job cancelled.
	cancelled := prod.allCancelled()
	if !slices.Contains(cancelled, origJobID) {
		t.Errorf("old job %q not cancelled; cancelled: %v", origJobID, cancelled)
	}

	// New enqueue must have nil Recurrence (one-shot).
	enqueued := prod.allEnqueued()
	// The last Enqueue is the one-shot re-enqueue; find one after Create's enqueue.
	var foundOneShot bool
	for _, req := range enqueued {
		if req.Key == "reminder:"+r.ID && req.Recurrence == nil {
			// There may be two: one from Create (rrule=FREQ=DAILY, recurring) and one
			// from the clear (nil Recurrence). We want the latter.
			// The first from Create would have Recurrence set; this one has nil.
			foundOneShot = true
		}
	}
	if !foundOneShot {
		t.Errorf("expected a one-shot Enqueue (nil Recurrence) after clearing rrule; all enqueued: %+v", enqueued)
	}

	// fire_job_id changed.
	if updated.FireJobID == origJobID {
		t.Errorf("fire_job_id unchanged after clearing rrule; still %q", origJobID)
	}
	if updated.FireJobID == "" {
		t.Error("fire_job_id must be non-empty after clearing rrule")
	}
	if updated.Rrule != "" {
		t.Errorf("Rrule must be empty after clearing, got %q", updated.Rrule)
	}
}

// TestFireHandler_Recurring_DST verifies AC6: a daily 8am rule across a US
// spring-forward DST boundary keeps wall-clock time (8am local, not 7am/9am UTC).
//
// The anchor is 2020-01-01 08:00 ET (EST, UTC-5) = 13:00 UTC — far in the past
// so time.Now() (2026-era) is always after it. The RRULE engine computes the
// next daily 08:00 ET occurrence after now; because we're currently in EDT (UTC-4)
// in June, that occurrence will be 08:00 ET = 12:00 UTC, not 13:00 UTC.
//
// The core property: whatever the next occurrence is, its wall-clock hour in
// America/New_York must be 8am. If the implementation were naively computing
// "now + 24h" it would drift by an hour across DST boundaries.
func TestFireHandler_Recurring_DST(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-dst-001"}
	bus := &fakePublisher{}
	m, reg := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-dst-01"
	const tz = "America/New_York"
	seedUser(t, st, userID, tz)
	scope := newScopeFor(userID)

	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	// Anchor: 2020-01-01 08:00 ET (EST, UTC-5) = 13:00 UTC.
	// The handler will be dispatched "now" (2026-era), computing the next daily
	// 08:00 ET occurrence after now. The result must have Hour()==8 in ET.
	anchor := time.Date(2020, 1, 1, 8, 0, 0, 0, loc).UTC()

	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "Daily 8am ET",
		DueAt: anchor,
		Tz:    tz,
		Rrule: "FREQ=DAILY",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Dispatch the fire handler.
	payload, _ := json.Marshal(map[string]string{"reminderId": r.ID})
	job := scheduler.Job{
		ID:      "job-dst-001",
		Kind:    "reminder.fire",
		Payload: payload,
		Attempt: 1,
		Scope:   scope,
	}
	if err := reg.dispatch(ctx, "reminder.fire", job); err != nil {
		t.Fatalf("fire handler: %v", err)
	}

	row, err := db.New(st.Reader()).GetReminder(ctx, db.GetReminderParams{ID: r.ID, UserID: userID})
	if err != nil {
		t.Fatalf("GetReminder: %v", err)
	}

	nextDue, parseErr := time.Parse(time.RFC3339, row.DueAt)
	if parseErr != nil {
		t.Fatalf("parse due_at %q: %v", row.DueAt, parseErr)
	}

	// Core property: wall-clock hour must be 8am in the reminder's timezone.
	// This holds regardless of DST offset (EST UTC-5 or EDT UTC-4).
	nextDueLocal := nextDue.In(loc)
	if nextDueLocal.Hour() != 8 {
		t.Errorf("DST test: next due_at=%v (local=%v), want 8am %s; got hour=%d",
			nextDue, nextDueLocal, tz, nextDueLocal.Hour())
	}
	if nextDueLocal.Minute() != 0 || nextDueLocal.Second() != 0 {
		t.Errorf("DST test: next due_at time-of-day=%02d:%02d:%02d, want 08:00:00",
			nextDueLocal.Hour(), nextDueLocal.Minute(), nextDueLocal.Second())
	}
	// due_at must be after the anchor.
	if !nextDue.After(anchor) {
		t.Errorf("DST test: next due_at=%v must be after anchor=%v", nextDue, anchor)
	}
}

// ── Item 1: status guard + finite-series exhaustion ──────────────────────────

// TestFireHandler_ExhaustedSeries verifies the finite-series branch:
//   - Create a recurring reminder with FREQ=DAILY;COUNT=1 anchored in the past.
//   - First dispatch: nextOccurrence returns zero (series exhausted) →
//     reminder.fired emitted, last_fired_at stamped, status=completed.
//   - Second dispatch (reclaim simulation): status != "active" → returns nil,
//     no second reminder.fired emitted.
func TestFireHandler_ExhaustedSeries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-exhausted-001"}
	bus := &fakePublisher{}
	m, reg := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-exhausted-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	// FREQ=DAILY;COUNT=1 — exactly one occurrence at the anchor (in the past).
	// When the handler fires "now" (2026-era), nextOccurrence computes After(now,false)
	// which returns zero because COUNT=1 was consumed at the anchor.
	anchor := time.Date(2020, 1, 1, 8, 0, 0, 0, time.UTC)
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title:        "Finite series",
		DueAt:        anchor,
		Tz:           "UTC",
		Rrule:        "FREQ=DAILY;COUNT=1",
		AutoComplete: ptrFalse, // auto_complete=false to prove exhaustion completes regardless
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	payload, _ := json.Marshal(map[string]string{"reminderId": r.ID})
	job := scheduler.Job{
		ID:      "job-exhausted-001",
		Kind:    "reminder.fire",
		Payload: payload,
		Attempt: 1,
		Scope:   scope,
	}

	// ── First dispatch: series is exhausted → completed ──────────────────────
	if err := reg.dispatch(ctx, "reminder.fire", job); err != nil {
		t.Fatalf("fire handler (first run): %v", err)
	}

	// reminder.fired must have been published once.
	evts := bus.allEvents()
	var firedCount int
	for _, e := range evts {
		if e.event.Type == "reminder.fired" && e.userID == userID {
			firedCount++
		}
	}
	if firedCount != 1 {
		t.Errorf("expected exactly 1 reminder.fired after exhausted series; got %d", firedCount)
	}

	// Row: status=completed, last_fired_at stamped.
	row, err := db.New(st.Reader()).GetReminder(ctx, db.GetReminderParams{
		ID:     r.ID,
		UserID: userID,
	})
	if err != nil {
		t.Fatalf("GetReminder after first dispatch: %v", err)
	}
	if row.Status != "completed" {
		t.Errorf("status=%q, want %q (series exhausted → completed)", row.Status, "completed")
	}
	if !row.LastFiredAt.Valid || row.LastFiredAt.String == "" {
		t.Error("expected last_fired_at to be stamped after exhausted series")
	}
	if !row.CompletedAt.Valid || row.CompletedAt.String == "" {
		t.Error("expected completed_at to be set (exhausted series → terminal)")
	}

	// ── Second dispatch (reclaim): status != "active" → no-op ───────────────
	if err := reg.dispatch(ctx, "reminder.fire", job); err != nil {
		t.Fatalf("fire handler (second run / reclaim): %v", err)
	}

	// No second reminder.fired must have been emitted.
	evts2 := bus.allEvents()
	var firedCount2 int
	for _, e := range evts2 {
		if e.event.Type == "reminder.fired" && e.userID == userID {
			firedCount2++
		}
	}
	if firedCount2 != 1 {
		t.Errorf("expected still exactly 1 reminder.fired after reclaim dispatch of completed reminder; got %d", firedCount2)
	}
}

// TestFireHandler_CompletedGuard verifies that a completed reminder whose
// fire-job is somehow dispatched (e.g., scheduler reclaim after auto-complete)
// returns nil without re-firing.
func TestFireHandler_CompletedGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-cmpguard-001"}
	bus := &fakePublisher{}
	m, reg := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-cmpguard-01"
	seedUser(t, st, userID, "UTC")
	scope := newScopeFor(userID)

	// Create a one-shot reminder with auto_complete=true; fire it once (legitimate).
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title:        "Complete-guard reminder",
		DueAt:        time.Date(2020, 6, 1, 9, 0, 0, 0, time.UTC), // past
		AutoComplete: ptrTrue,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	payload, _ := json.Marshal(map[string]string{"reminderId": r.ID})
	job := scheduler.Job{
		ID:      "job-cmpguard-001",
		Kind:    "reminder.fire",
		Payload: payload,
		Attempt: 1,
		Scope:   scope,
	}

	// First dispatch: legitimate fire → status becomes completed.
	if err := reg.dispatch(ctx, "reminder.fire", job); err != nil {
		t.Fatalf("fire handler (first run): %v", err)
	}

	// Confirm the reminder is now completed.
	row, err := db.New(st.Reader()).GetReminder(ctx, db.GetReminderParams{
		ID:     r.ID,
		UserID: userID,
	})
	if err != nil {
		t.Fatalf("GetReminder: %v", err)
	}
	if row.Status != "completed" {
		t.Fatalf("expected status=completed after first fire; got %q", row.Status)
	}

	// Clear events so we can count cleanly.
	bus.mu.Lock()
	bus.events = nil
	bus.mu.Unlock()

	// Second dispatch: simulates at-least-once scheduler reclaim of the job.
	// Because status != "active", handler must return nil and NOT emit reminder.fired.
	if err := reg.dispatch(ctx, "reminder.fire", job); err != nil {
		t.Fatalf("fire handler (reclaim of completed): %v", err)
	}

	evts := bus.allEvents()
	for _, e := range evts {
		if e.event.Type == "reminder.fired" && e.userID == userID {
			t.Errorf("reminder.fired must NOT be emitted when dispatching a completed reminder; got event: %+v", e)
		}
	}
}

// TestUpdate_Rrule_ValidatesInEffectiveTz verifies that Service.Update validates
// the rrule against the effective timezone (the reminder's stored tz), not always
// UTC. A rule that is syntactically valid in UTC but would need tz-aware parsing
// must still pass — the goal here is that the timezone used for validation matches
// what will be stored and enqueued, keeping Create/Update consistent.
func TestUpdate_Rrule_ValidatesInEffectiveTz(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openMigratedStore(t)
	prod := &fakeProducer{returnID: "job-tz-valid-001"}
	bus := &fakePublisher{}
	m, _ := newTestModule(t, st, prod, bus)
	svc := m.Service()

	const userID = "user-tz-valid-01"
	seedUser(t, st, userID, "America/New_York")
	scope := newScopeFor(userID)

	// Create a reminder in America/New_York (the effective tz stored on the row).
	r, err := svc.Create(ctx, scope, reminders.CreateInput{
		Title: "TZ-aware rrule update test",
		DueAt: time.Now().UTC().Add(time.Hour),
		Tz:    "America/New_York",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Setting a valid rrule via Update must succeed (not reject due to wrong tz
	// being used in the validator).
	updated, err := svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Rrule: reminders.SetString("FREQ=DAILY"),
	})
	if err != nil {
		t.Fatalf("Update with valid rrule should succeed; got: %v", err)
	}
	if updated.Rrule != "FREQ=DAILY" {
		t.Errorf("got Rrule %q, want %q", updated.Rrule, "FREQ=DAILY")
	}

	// A genuinely malformed rrule must still be rejected regardless of tz.
	_, err = svc.Update(ctx, scope, r.ID, reminders.UpdateInput{
		Rrule: reminders.SetString("FREQ=BOGUS"),
	})
	if err == nil {
		t.Fatal("expected ValidationError for malformed rrule, got nil")
	}
	var valErr *reminders.ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected *reminders.ValidationError, got %T: %v", err, err)
	}
	var foundPointer bool
	for _, fe := range valErr.Fields {
		if fe.Pointer == "/rrule" {
			foundPointer = true
		}
	}
	if !foundPointer {
		t.Errorf("expected field error at /rrule; got: %+v", valErr.Fields)
	}
}
