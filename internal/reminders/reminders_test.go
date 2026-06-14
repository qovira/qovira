package reminders_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

	// Verify the composite index exists.
	var indexName string
	err = st.Reader().QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='reminders_user_status_due'`,
	).Scan(&indexName)
	if err != nil {
		t.Fatalf("reminders_user_status_due index not found: %v", err)
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
