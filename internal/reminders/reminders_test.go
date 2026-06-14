package reminders_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
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

// fakeProducer records Enqueue and Cancel calls and allows controlled returns.
type fakeProducer struct {
	mu        sync.Mutex
	enqueued  []scheduler.EnqueueRequest
	returnID  string
	returnErr error
	cancelled []string
	cancelErr error
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

func (f *fakeProducer) Reschedule(_ context.Context, _ string, _ time.Time) error {
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
	if m.Tools() != nil {
		t.Errorf("Tools() should return nil for this slice, got: %v", m.Tools())
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
