// Package reminders implements the reminders capability module: create a one-shot
// reminder, persist it, enqueue a fire-job, and fire it live at due_at via the
// "reminder.fire" scheduler handler.
//
// This slice establishes the Service + REST adapter + fire path.
// AI tools, list/update/complete/delete, and rrule recurrence are later slices.
package reminders

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/id"
	"github.com/qovira/qovira/internal/scheduler"
	"github.com/qovira/qovira/internal/store"
	"github.com/qovira/qovira/internal/store/db"
)

// ── Domain types ──────────────────────────────────────────────────────────────

// Reminder is the public domain type returned by Service methods and serialised
// in REST responses. camelCase JSON field names follow the HTTP house guide.
type Reminder struct {
	ID           string `json:"id"`
	UserID       string `json:"userId"`
	Title        string `json:"title"`
	Notes        string `json:"notes,omitempty"`
	DueAt        string `json:"dueAt"`
	Rrule        string `json:"rrule,omitempty"`
	Tz           string `json:"tz"`
	AutoComplete bool   `json:"autoComplete"`
	Status       string `json:"status"`
	CompletedAt  string `json:"completedAt,omitempty"`
	LastFiredAt  string `json:"lastFiredAt,omitempty"`
	FireJobID    string `json:"fireJobId,omitempty"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

// CreateInput carries the caller-facing parameters for creating a reminder.
// All validation is performed in Service.Create.
type CreateInput struct {
	// Title is required and must be non-empty after trimming.
	Title string
	// Notes is optional free-text.
	Notes string
	// DueAt is the first (and for one-shot, only) fire instant. Required.
	DueAt time.Time
	// Tz is an optional IANA timezone name. When empty, Service.Create defaults
	// it from the user's profile, or "UTC" if the profile zone is also empty.
	Tz string
	// AutoComplete controls whether the reminder auto-completes on fire.
	// When nil, defaults to true. Pass a *bool to override.
	AutoComplete *bool
	// Rrule is an optional RFC 5545 RRULE string stored as-is.
	// Recurrence logic is a later slice; this field is stored only.
	Rrule string
}

// FiredEventPayload is the payload of the "reminder.fired" bus event.
// camelCase JSON field names follow the HTTP house guide.
type FiredEventPayload struct {
	ReminderID string `json:"reminderId"`
	Title      string `json:"title"`
	DueAt      string `json:"dueAt"`
	FiredAt    string `json:"firedAt"`
}

// firePayload is the scheduler job payload: just the reminder id.
// The fire handler loads the full row fresh on each dispatch.
type firePayload struct {
	ReminderID string `json:"reminderId"`
}

// ListQuery carries the caller-facing parameters for listing reminders.
// It corresponds to the query parameters on GET /api/v1/reminders.
type ListQuery struct {
	// Cursor is the opaque pagination cursor returned by a prior page.  Empty
	// means "start from the beginning".  A non-empty value that cannot be
	// decoded is a caller error (400).
	Cursor string
	// Limit is the maximum number of items to return.  0 uses the default (25).
	// Values above the maximum (100) are silently clamped to 100.
	Limit int
	// Status is an optional filter.  Accepted values: "active", "completed".
	// Empty means no filter (all statuses).
	Status string
	// DueAfter, when non-zero, filters reminders whose due_at is strictly after
	// the given instant.
	DueAfter time.Time
	// DueBefore, when non-zero, filters reminders whose due_at is strictly
	// before the given instant.
	DueBefore time.Time
}

// Page is the service-layer result of a list query.  The HTTP layer maps it to
// the httpx.Page[Reminder] envelope.
type Page struct {
	// Items is the current page of reminders, ordered by (due_at, id).
	Items []Reminder
	// NextCursor is the opaque cursor for the next page, or empty string when
	// this is the last page.  The HTTP layer maps an empty string to JSON null.
	NextCursor string
	// HasMore is true when there is at least one more page.
	HasMore bool
}

// listCursor is the internal structure encoded into the opaque pagination cursor.
// Both DueAt and ID are required for a stable, gap-free total order.
type listCursor struct {
	DueAt string `json:"d"` // RFC 3339 UTC
	ID    string `json:"i"`
}

// encodeCursor base64-encodes a JSON listCursor into an opaque string suitable
// for inclusion in an HTTP response.
func encodeCursor(dueAt, id string) string {
	raw, _ := json.Marshal(listCursor{DueAt: dueAt, ID: id})
	return base64.RawStdEncoding.EncodeToString(raw)
}

// decodeCursor reverses encodeCursor.  Returns an error when the cursor string
// is not valid base64 or its JSON content is malformed.
func decodeCursor(cursor string) (dueAt, reminderID string, err error) {
	raw, err := base64.RawStdEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", fmt.Errorf("reminders: decode cursor: %w", err)
	}
	var c listCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", "", fmt.Errorf("reminders: decode cursor: invalid json: %w", err)
	}
	if c.DueAt == "" || c.ID == "" {
		return "", "", fmt.Errorf("reminders: decode cursor: missing required fields")
	}
	return c.DueAt, c.ID, nil
}

const (
	listDefaultLimit = 25
	listMaxLimit     = 100
)

// ── Errors ────────────────────────────────────────────────────────────────────

// ErrNotFound is returned by Service.Get when the reminder does not exist or
// does not belong to the requesting user.
var ErrNotFound = errors.New("reminders: not found")

// ErrInvalidCursor is returned by Service.List when the caller supplies a
// cursor value that cannot be decoded.  The HTTP adapter maps this to a 400
// Bad Request response.
var ErrInvalidCursor = errors.New("reminders: invalid cursor")

// ValidationError carries one or more field-level validation failures.
// The HTTP adapter maps it to a 422 problem+json response.
type ValidationError struct {
	Fields []httpx.FieldError
}

func (e *ValidationError) Error() string {
	msgs := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		msgs = append(msgs, f.Pointer+": "+f.Detail)
	}
	return "reminders: validation error: " + strings.Join(msgs, "; ")
}

// ── Producer / Registrar seams ────────────────────────────────────────────────

// Producer is the narrow interface the Service uses for job lifecycle operations.
// *scheduler.Scheduler satisfies it.
//
// This is the full producer seam the reminders module needs across all slices:
//   - Enqueue and Cancel are used in this slice (create + best-effort compensation).
//   - Reschedule lands in the edit/complete/delete slice.
type Producer interface {
	Enqueue(ctx context.Context, req scheduler.EnqueueRequest) (jobID string, err error)
	Reschedule(ctx context.Context, jobID string, runAt time.Time) error
	Cancel(ctx context.Context, jobID string) error
}

// Registrar is the narrow interface used at construction to register the
// "reminder.fire" handler. *scheduler.Scheduler satisfies it.
type Registrar interface {
	Register(kind string, h scheduler.Handler)
}

// ── Service ───────────────────────────────────────────────────────────────────

// Service owns the reminders domain logic. Construct via New.
type Service struct {
	st     *store.Store
	prod   Producer
	bus    events.Publisher
	logger *slog.Logger
}

// Create validates in, persists the reminder, enqueues a fire-job, and stamps
// the fire_job_id on the row. It publishes "reminder.created" on the bus.
//
// Validation (single ValidationError):
//   - title: required, non-empty after trim.
//   - dueAt: required (IsZero → rejected).
//   - tz: if explicit, must be a real IANA zone.
//
// A past dueAt is accepted; the scheduler fires on the next poll.
func (s *Service) Create(ctx context.Context, scope store.Scope, in CreateInput) (Reminder, error) {
	// ── Validation ──────────────────────────────────────────────────────────
	var fields []httpx.FieldError

	title := strings.TrimSpace(in.Title)
	if title == "" {
		fields = append(fields, httpx.FieldError{
			Pointer: "/title",
			Detail:  "title is required and must not be empty",
		})
	}

	if in.DueAt.IsZero() {
		fields = append(fields, httpx.FieldError{
			Pointer: "/dueAt",
			Detail:  "dueAt is required",
		})
	}

	// tz: validate the explicit value now; default is resolved below.
	if in.Tz != "" {
		if _, err := time.LoadLocation(in.Tz); err != nil {
			fields = append(fields, httpx.FieldError{
				Pointer: "/tz",
				Detail:  fmt.Sprintf("%q is not a valid IANA timezone", in.Tz),
			})
		}
	}

	if len(fields) > 0 {
		return Reminder{}, &ValidationError{Fields: fields}
	}

	// ── Resolve timezone ────────────────────────────────────────────────────
	tz := in.Tz
	if tz == "" {
		// Default from user profile; fall back to UTC if missing or invalid.
		prof, err := s.st.ForUser(scope).GetProfile(ctx)
		if err == nil && prof.Timezone != "" {
			if _, locErr := time.LoadLocation(prof.Timezone); locErr == nil {
				tz = prof.Timezone
			}
		}
		if tz == "" {
			tz = "UTC"
		}
	}

	// ── Resolve auto_complete default ───────────────────────────────────────
	autoComplete := true
	if in.AutoComplete != nil {
		autoComplete = *in.AutoComplete
	}

	// ── Persist ─────────────────────────────────────────────────────────────
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	// Canonical due-at: truncate to the second once so the persisted value and
	// the job RunAt are always identical (<1 s drift is otherwise possible when
	// using the raw in.DueAt for Enqueue and the formatted string for storage).
	dueAtCanon := in.DueAt.UTC().Truncate(time.Second)
	dueAtStr := dueAtCanon.Format(time.RFC3339)

	reminderID := id.New()

	var autoCompleteInt int64
	if autoComplete {
		autoCompleteInt = 1
	}

	params := db.InsertReminderParams{
		ID:           reminderID,
		UserID:       scope.UserID(),
		Title:        title,
		Notes:        nullStr(in.Notes),
		DueAt:        dueAtStr,
		Rrule:        nullStr(in.Rrule),
		Tz:           tz,
		AutoComplete: autoCompleteInt,
		Status:       "active",
		CompletedAt:  sql.NullString{},
		LastFiredAt:  sql.NullString{},
		FireJobID:    sql.NullString{},
		CreatedAt:    nowStr,
		UpdatedAt:    nowStr,
	}

	if err := db.New(s.st.Writer()).InsertReminder(ctx, params); err != nil {
		return Reminder{}, fmt.Errorf("reminders: create insert: %w", err)
	}

	// ── Enqueue fire-job ─────────────────────────────────────────────────────
	payload, err := json.Marshal(firePayload{ReminderID: reminderID})
	if err != nil {
		// Row was inserted but we cannot build the payload; delete the orphan.
		if delErr := s.deleteReminder(ctx, scope, reminderID); delErr != nil {
			s.logger.Error("reminders: create: cleanup after marshal failure",
				"reminder_id", reminderID, "err", delErr)
		}
		return Reminder{}, fmt.Errorf("reminders: marshal fire payload: %w", err)
	}

	jobID, err := s.prod.Enqueue(ctx, scheduler.EnqueueRequest{
		Kind:    "reminder.fire",
		Scope:   scope,
		RunAt:   dueAtCanon, // canonical truncated instant matches persisted due_at
		Key:     "reminder:" + reminderID,
		Payload: payload,
	})
	if err != nil {
		// Best-effort: delete the orphan row so it doesn't persist with no fire-job.
		enqErr := fmt.Errorf("reminders: enqueue fire job: %w", err)
		if delErr := s.deleteReminder(ctx, scope, reminderID); delErr != nil {
			s.logger.Error("reminders: create: cleanup after enqueue failure",
				"reminder_id", reminderID, "err", delErr)
		}
		return Reminder{}, enqErr
	}

	// ── Stamp fire_job_id ────────────────────────────────────────────────────
	_, err = db.New(s.st.Writer()).SetReminderFireJobID(ctx, db.SetReminderFireJobIDParams{
		FireJobID: sql.NullString{String: jobID, Valid: true},
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		ID:        reminderID,
		UserID:    scope.UserID(),
	})
	if err != nil {
		// Best-effort: cancel the live job and delete the row to avoid a phantom
		// job with no fire_job_id on the row (which would block future reschedule/cancel).
		stampErr := fmt.Errorf("reminders: stamp fire_job_id: %w", err)
		if cancelErr := s.prod.Cancel(ctx, jobID); cancelErr != nil {
			s.logger.Error("reminders: create: cancel job after stamp failure",
				"reminder_id", reminderID, "job_id", jobID, "err", cancelErr)
		}
		if delErr := s.deleteReminder(ctx, scope, reminderID); delErr != nil {
			s.logger.Error("reminders: create: cleanup row after stamp failure",
				"reminder_id", reminderID, "err", delErr)
		}
		return Reminder{}, stampErr
	}

	r := Reminder{
		ID:           reminderID,
		UserID:       scope.UserID(),
		Title:        title,
		Notes:        in.Notes,
		DueAt:        dueAtStr,
		Rrule:        in.Rrule,
		Tz:           tz,
		AutoComplete: autoComplete,
		Status:       "active",
		FireJobID:    jobID,
		CreatedAt:    nowStr,
		UpdatedAt:    nowStr,
	}

	// ── Publish reminder.created ─────────────────────────────────────────────
	s.bus.Publish(scope.UserID(), events.Event{
		Type: "reminder.created",
		Data: r,
	})

	return r, nil
}

// Get retrieves a reminder by id for the requesting user. Returns ErrNotFound
// when the reminder does not exist or belongs to a different user.
func (s *Service) Get(ctx context.Context, scope store.Scope, reminderID string) (Reminder, error) {
	row, err := db.New(s.st.Reader()).GetReminder(ctx, db.GetReminderParams{
		ID:     reminderID,
		UserID: scope.UserID(),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Reminder{}, fmt.Errorf("reminders: get %q: %w", reminderID, ErrNotFound)
		}
		return Reminder{}, fmt.Errorf("reminders: get %q: %w", reminderID, err)
	}
	return reminderFromRow(row), nil
}

// List returns a cursor-paginated slice of reminders for the requesting user,
// filtered and ordered per q.  It returns exactly q.Limit items (default 25,
// max 100) plus a next-cursor when more pages exist.
//
// A non-empty q.Cursor that cannot be decoded causes an error wrapping
// ErrInvalidCursor; the HTTP adapter maps this to a 400 response.
func (s *Service) List(ctx context.Context, scope store.Scope, q ListQuery) (Page, error) {
	// Resolve limit.
	limit := q.Limit
	if limit <= 0 {
		limit = listDefaultLimit
	}
	if limit > listMaxLimit {
		limit = listMaxLimit
	}

	// Decode cursor (if provided).
	var cursorDue, cursorID string
	if q.Cursor != "" {
		var err error
		cursorDue, cursorID, err = decodeCursor(q.Cursor)
		if err != nil {
			return Page{}, fmt.Errorf("%w: %w", ErrInvalidCursor, err)
		}
	}

	// Build query params.  sqlc generated Status/DueAfter/DueBefore as
	// any (narg) and CursorDue/CursorID as any/sql.NullString.
	// We pass nil for absent optional params so the predicate is a no-op.
	var statusArg any
	if q.Status != "" {
		statusArg = q.Status
	}
	var dueAfterArg any
	if !q.DueAfter.IsZero() {
		dueAfterArg = q.DueAfter.UTC().Format(time.RFC3339)
	}
	var dueBeforeArg any
	if !q.DueBefore.IsZero() {
		dueBeforeArg = q.DueBefore.UTC().Format(time.RFC3339)
	}
	var cursorDueArg any
	var cursorIDArg sql.NullString
	if cursorDue != "" {
		cursorDueArg = cursorDue
		cursorIDArg = sql.NullString{String: cursorID, Valid: true}
	}

	// Fetch one extra row to detect whether a next page exists.
	rows, err := db.New(s.st.Reader()).ListReminders(ctx, db.ListRemindersParams{
		UserID:    scope.UserID(),
		Status:    statusArg,
		DueAfter:  dueAfterArg,
		DueBefore: dueBeforeArg,
		CursorDue: cursorDueArg,
		CursorID:  cursorIDArg,
		Limit:     int64(limit + 1),
	})
	if err != nil {
		return Page{}, fmt.Errorf("reminders: list: %w", err)
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	items := make([]Reminder, 0, len(rows))
	for _, row := range rows {
		items = append(items, reminderFromRow(row))
	}

	var nextCursor string
	if hasMore {
		// items is non-empty when hasMore is true: we fetched limit+1 rows and
		// trimmed to limit, so at least one item is present.
		last := items[len(items)-1]
		nextCursor = encodeCursor(last.DueAt, last.ID)
	}

	return Page{Items: items, NextCursor: nextCursor, HasMore: hasMore}, nil
}

// ── fire handler ──────────────────────────────────────────────────────────────

// handleFire is the "reminder.fire" scheduler handler.  It loads the reminder
// fresh, publishes "reminder.fired", stamps last_fired_at, and optionally
// completes the reminder.
func (s *Service) handleFire(ctx context.Context, job scheduler.Job) error {
	// Decode payload.
	var p firePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return scheduler.Permanent(fmt.Errorf("reminders: fire: decode payload: %w", err))
	}

	// Load the reminder fresh.
	row, err := db.New(s.st.Reader()).GetReminder(ctx, db.GetReminderParams{
		ID:     p.ReminderID,
		UserID: job.Scope.UserID(),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Reminder was deleted before the fire — dead-letter without retry.
			return scheduler.Permanent(fmt.Errorf("reminders: fire: reminder %q gone: %w",
				p.ReminderID, ErrNotFound))
		}
		return fmt.Errorf("reminders: fire: load reminder %q: %w", p.ReminderID, err)
	}

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	// Publish reminder.fired fat event.
	s.bus.Publish(job.Scope.UserID(), events.Event{
		Type: "reminder.fired",
		Data: FiredEventPayload{
			ReminderID: row.ID,
			Title:      row.Title,
			DueAt:      row.DueAt,
			FiredAt:    nowStr,
		},
	})

	// Stamp last_fired_at and optionally complete.
	if row.AutoComplete == 1 {
		_, err = db.New(s.st.Writer()).StampFiredAutoComplete(ctx, db.StampFiredAutoCompleteParams{
			LastFiredAt: sql.NullString{String: nowStr, Valid: true},
			CompletedAt: sql.NullString{String: nowStr, Valid: true},
			UpdatedAt:   nowStr,
			ID:          row.ID,
			UserID:      job.Scope.UserID(),
		})
	} else {
		_, err = db.New(s.st.Writer()).StampFiredKeepActive(ctx, db.StampFiredKeepActiveParams{
			LastFiredAt: sql.NullString{String: nowStr, Valid: true},
			UpdatedAt:   nowStr,
			ID:          row.ID,
			UserID:      job.Scope.UserID(),
		})
	}
	if err != nil {
		return fmt.Errorf("reminders: fire: stamp fired: %w", err)
	}

	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// deleteReminder is the scope-bound best-effort delete used by Create's
// compensation paths. It silently tolerates a 0-row result (already gone).
func (s *Service) deleteReminder(ctx context.Context, scope store.Scope, reminderID string) error {
	_, err := db.New(s.st.Writer()).DeleteReminder(ctx, db.DeleteReminderParams{
		ID:     reminderID,
		UserID: scope.UserID(),
	})
	return err
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func reminderFromRow(row db.Reminder) Reminder {
	r := Reminder{
		ID:           row.ID,
		UserID:       row.UserID,
		Title:        row.Title,
		DueAt:        row.DueAt,
		Tz:           row.Tz,
		AutoComplete: row.AutoComplete == 1,
		Status:       row.Status,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
	if row.Notes.Valid {
		r.Notes = row.Notes.String
	}
	if row.Rrule.Valid {
		r.Rrule = row.Rrule.String
	}
	if row.CompletedAt.Valid {
		r.CompletedAt = row.CompletedAt.String
	}
	if row.LastFiredAt.Valid {
		r.LastFiredAt = row.LastFiredAt.String
	}
	if row.FireJobID.Valid {
		r.FireJobID = row.FireJobID.String
	}
	return r
}

// ── Module ────────────────────────────────────────────────────────────────────

// Module wires the reminders Service to the HTTP router and the capability
// registry. It satisfies app.Module.
type Module struct {
	svc    *Service
	logger *slog.Logger
}

// New constructs a Module, registers the "reminder.fire" handler on reg, and
// returns the Module. reg must be the concrete scheduler; it is used only for
// handler registration.
//
// Call New before scheduler.Start so the handler is visible on the first tick.
func New(st *store.Store, prod Producer, bus events.Publisher, reg Registrar) *Module {
	svc := &Service{
		st:     st,
		prod:   prod,
		bus:    bus,
		logger: slog.Default(),
	}

	// Register the fire handler before Start is called.
	reg.Register("reminder.fire", svc.handleFire)

	return &Module{svc: svc, logger: slog.Default()}
}

// Service returns the underlying Service for direct use in tests or wiring.
func (m *Module) Service() *Service { return m.svc }

// Name returns the module name.
func (m *Module) Name() string { return "reminders" }

// Tools returns nil — AI tools are a later slice.
func (m *Module) Tools() []capability.Tool { return nil }

// Routes registers the reminders REST endpoints on r.
//
//	POST /api/v1/reminders      → createHandler (201)
//	GET  /api/v1/reminders      → listHandler   (200)
//	GET  /api/v1/reminders/{id} → getHandler    (200)
func (m *Module) Routes(r *httpx.Router) {
	r.HandleFunc("POST /api/v1/reminders", m.createHandler)
	r.HandleFunc("GET /api/v1/reminders", m.listHandler)
	r.HandleFunc("GET /api/v1/reminders/{id}", m.getHandler)
}

// createHandler handles POST /api/v1/reminders.
func (m *Module) createHandler(w http.ResponseWriter, r *http.Request) {
	principal, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, Problem401())
		return
	}
	scope := store.UserScope(principal)

	// Parse body.
	var body createRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteProblem(w, r, httpx.MalformedBodyProblem())
		return
	}

	// Parse dueAt.
	var dueAt time.Time
	if body.DueAt == "" {
		httpx.WriteProblem(w, r, httpx.ValidationProblem(
			"validation_error",
			"Request validation failed.",
			httpx.FieldError{Pointer: "/dueAt", Detail: "dueAt is required"},
		))
		return
	}
	var parseErr error
	dueAt, parseErr = time.Parse(time.RFC3339, body.DueAt)
	if parseErr != nil {
		httpx.WriteProblem(w, r, httpx.ValidationProblem(
			"validation_error",
			"Request validation failed.",
			httpx.FieldError{Pointer: "/dueAt", Detail: "dueAt must be an RFC 3339 timestamp"},
		))
		return
	}

	in := CreateInput{
		Title:        body.Title,
		Notes:        body.Notes,
		DueAt:        dueAt,
		Tz:           body.Tz,
		AutoComplete: body.AutoComplete,
		Rrule:        body.Rrule,
	}

	reminder, err := m.svc.Create(r.Context(), scope, in)
	if err != nil {
		var valErr *ValidationError
		if errors.As(err, &valErr) {
			httpx.WriteProblem(w, r, httpx.ValidationProblem(
				"validation_error",
				"Request validation failed.",
				valErr.Fields...,
			))
			return
		}
		httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "create_reminder_failed", err.Error()))
		return
	}

	w.Header().Set("Location", "/api/v1/reminders/"+reminder.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(reminder); err != nil {
		m.logger.Error("reminders: encode create response", "err", err)
	}
}

// getHandler handles GET /api/v1/reminders/{id}.
func (m *Module) getHandler(w http.ResponseWriter, r *http.Request) {
	principal, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, Problem401())
		return
	}
	scope := store.UserScope(principal)

	reminderID := r.PathValue("id")
	if reminderID == "" {
		httpx.WriteProblem(w, r, httpx.ValidationProblem(
			"validation_error",
			"id path parameter is required.",
		))
		return
	}

	reminder, err := m.svc.Get(r.Context(), scope, reminderID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.WriteProblem(w, r, Problem404())
			return
		}
		httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "get_reminder_failed", err.Error()))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(reminder); err != nil {
		m.logger.Error("reminders: encode get response", "err", err)
	}
}

// listKnownParams is the set of query parameter names accepted by listHandler.
// Any name outside this set triggers a 400 Bad Request — per the HTTP house guide,
// unknown filter params must be rejected rather than silently ignored, because a
// typo'd param (e.g. ?staus=completed) would otherwise return the wrong data.
var listKnownParams = map[string]struct{}{
	"cursor":    {},
	"limit":     {},
	"status":    {},
	"dueBefore": {},
	"dueAfter":  {},
}

// listHandler handles GET /api/v1/reminders.
//
// Query parameters (all optional):
//   - cursor     opaque pagination cursor from a prior response's nextCursor
//   - limit      page size (default 25, max 100; over-max clamped to 100)
//   - status     "active" | "completed"
//   - dueBefore  RFC 3339 upper bound on due_at (exclusive)
//   - dueAfter   RFC 3339 lower bound on due_at (exclusive)
//
// Unknown query parameters are rejected with 400 Bad Request.
func (m *Module) listHandler(w http.ResponseWriter, r *http.Request) {
	principal, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, Problem401())
		return
	}
	scope := store.UserScope(principal)

	q := r.URL.Query()

	// ── reject unknown params ────────────────────────────────────────────────
	for name := range q {
		if _, known := listKnownParams[name]; !known {
			httpx.WriteProblem(w, r, httpx.Problem{
				Title:  "Unknown query parameter",
				Status: http.StatusBadRequest,
				Detail: fmt.Sprintf("Query parameter %q is not recognised. Accepted parameters: cursor, limit, status, dueBefore, dueAfter.", name),
				Code:   "unknown_query_param",
			})
			return
		}
	}

	// ── limit ────────────────────────────────────────────────────────────────
	limit := listDefaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpx.WriteProblem(w, r, httpx.ValidationProblem(
				"validation_error",
				"Request validation failed.",
				httpx.FieldError{Pointer: "/limit", Detail: "limit must be a positive integer"},
			))
			return
		}
		limit = n
	}

	// ── status ───────────────────────────────────────────────────────────────
	status := q.Get("status")
	if status != "" && status != "active" && status != "completed" {
		httpx.WriteProblem(w, r, httpx.ValidationProblem(
			"validation_error",
			"Request validation failed.",
			httpx.FieldError{Pointer: "/status", Detail: `status must be "active" or "completed"`},
		))
		return
	}

	// ── dueBefore / dueAfter ─────────────────────────────────────────────────
	var dueBefore, dueAfter time.Time
	if raw := q.Get("dueBefore"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpx.WriteProblem(w, r, httpx.ValidationProblem(
				"validation_error",
				"Request validation failed.",
				httpx.FieldError{Pointer: "/dueBefore", Detail: "dueBefore must be an RFC 3339 timestamp"},
			))
			return
		}
		dueBefore = parsed.UTC()
	}
	if raw := q.Get("dueAfter"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpx.WriteProblem(w, r, httpx.ValidationProblem(
				"validation_error",
				"Request validation failed.",
				httpx.FieldError{Pointer: "/dueAfter", Detail: "dueAfter must be an RFC 3339 timestamp"},
			))
			return
		}
		dueAfter = parsed.UTC()
	}

	// ── cursor ───────────────────────────────────────────────────────────────
	cursor := q.Get("cursor")

	lq := ListQuery{
		Cursor:    cursor,
		Limit:     limit,
		Status:    status,
		DueBefore: dueBefore,
		DueAfter:  dueAfter,
	}

	page, err := m.svc.List(r.Context(), scope, lq)
	if err != nil {
		if errors.Is(err, ErrInvalidCursor) {
			httpx.WriteProblem(w, r, httpx.Problem{
				Title:  "Invalid cursor",
				Status: http.StatusBadRequest,
				Detail: "The cursor value is malformed or has been corrupted.",
				Code:   "invalid_cursor",
			})
			return
		}
		httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "list_reminders_failed", err.Error()))
		return
	}

	// Map service Page to the shared httpx.Page[Reminder] envelope.
	// NextCursor is *string: nil on the last page (JSON null), non-nil with the
	// cursor token when HasMore is true.  Per the HTTP house guide, last-page
	// responses must emit null, not an empty string.
	var nextCursor *string
	if page.HasMore && page.NextCursor != "" {
		nextCursor = &page.NextCursor
	}
	envelope := httpx.Page[Reminder]{
		Data: page.Items,
		Pagination: httpx.PagePagination{
			NextCursor: nextCursor,
			HasMore:    page.HasMore,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(envelope); err != nil {
		m.logger.Error("reminders: encode list response", "err", err)
	}
}

// ── REST DTO ──────────────────────────────────────────────────────────────────

// createRequestBody is the JSON shape for POST /api/v1/reminders.
// camelCase per the HTTP house guide.
type createRequestBody struct {
	Title        string `json:"title"`
	Notes        string `json:"notes"`
	DueAt        string `json:"dueAt"`
	Tz           string `json:"tz"`
	AutoComplete *bool  `json:"autoComplete"`
	Rrule        string `json:"rrule"`
}

// ── Problem helpers ───────────────────────────────────────────────────────────

// Problem401 returns a 401 Unauthenticated problem.
func Problem401() httpx.Problem {
	return httpx.Problem{
		Title:  "Authentication required",
		Status: http.StatusUnauthorized,
		Detail: "You must be authenticated to access this resource.",
		Code:   "unauthenticated",
	}
}

// Problem404 returns a 404 Not Found problem for a reminder.
func Problem404() httpx.Problem {
	return httpx.Problem{
		Title:  "Reminder not found",
		Status: http.StatusNotFound,
		Detail: "The requested reminder does not exist or you do not have access to it.",
		Code:   "reminder_not_found",
	}
}
