// Package reminders implements the reminders capability module: create a reminder,
// persist it, enqueue a fire-job, and fire it live at due_at via the
// "reminder.fire" scheduler handler. Supports one-shot and recurring (RRULE)
// reminders. Recurring reminders advance due_at on each fire and never auto-complete.
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

	rrulego "github.com/teambition/rrule-go"

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

// UpdateInput carries the caller-facing parameters for a partial/merge update.
// Pointer fields distinguish "absent" (nil → leave unchanged) from "present"
// (non-nil → apply).
//
// PATCH merge semantics for nullable string columns (notes, rrule):
//   - nil pointer    → field absent; leave the stored value unchanged.
//   - OptionalString with Present=true, Value=""  → field present with null;
//     clear the column (set to NULL in the database).
//   - OptionalString with Present=true, Value="x" → field present with a value;
//     set the column to that value.
//
// This three-way distinction maps cleanly to the HTTP house guide PATCH merge
// rule: omitted=unchanged, null=clear, value=set. A plain *string cannot
// represent "present + null" without a sentinel value, so OptionalString is
// used for the two nullable columns. All non-nullable fields use *T directly.
type UpdateInput struct {
	// Title, when non-nil, replaces the reminder's title. Must be non-empty
	// after trim (validated in Update).
	Title *string
	// Notes, when Present=true, replaces (or clears) the notes column.
	Notes OptionalString
	// DueAt, when non-nil, replaces the fire instant. Triggers a Reschedule on
	// any active fire-job; ignored if no fire-job exists (completed reminder).
	DueAt *time.Time
	// Rrule, when Present=true, replaces (or clears) the stored rrule string.
	// NOTE: rrule is stored only in this slice; recurrence logic is a later
	// slice (slice 4). No validation of the rrule value is performed here — that
	// belongs to the recurring-reminders slice.
	Rrule OptionalString
	// AutoComplete, when non-nil, replaces the auto_complete flag.
	AutoComplete *bool
	// Status, when non-nil, sets the reminder status. Accepted values: "active",
	// "completed". The REST adapter routes "completed" to Complete and "active"
	// on a completed reminder to the re-open path. Direct callers can also pass
	// these values.
	Status *string
}

// OptionalString represents a nullable string field in a PATCH merge update.
// When Present is false the field is absent (leave unchanged). When Present is
// true the field is present: Value="" clears it, Value="x" sets it to "x".
//
// Construct with SetString (set a value) or ClearString (clear to null).
type OptionalString struct {
	Value   string
	Present bool
}

// SetString returns an OptionalString that sets the column to v.
func SetString(v string) OptionalString {
	return OptionalString{Value: v, Present: true}
}

// ClearString returns an OptionalString that clears the column to NULL.
func ClearString() OptionalString {
	return OptionalString{Present: true}
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
	tzForRruleValidation := in.Tz
	if in.Tz != "" {
		if _, err := time.LoadLocation(in.Tz); err != nil {
			fields = append(fields, httpx.FieldError{
				Pointer: "/tz",
				Detail:  `tz must be a valid IANA timezone name, e.g. "America/Los_Angeles"`,
			})
			tzForRruleValidation = "" // tz invalid; skip rrule parse (tz will be the error)
		}
	}

	// rrule: validate when present. Use explicit tz for parsing (or UTC as fallback
	// when tz is absent/defaulted — UTC is always valid and lets us catch bad rrule syntax).
	if in.Rrule != "" {
		effectiveTz := tzForRruleValidation
		if effectiveTz == "" {
			effectiveTz = "UTC"
		}
		if fe := validateRrule(in.Rrule, effectiveTz); fe != nil {
			fields = append(fields, *fe)
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
		Kind:       "reminder.fire",
		Scope:      scope,
		RunAt:      dueAtCanon, // canonical truncated instant matches persisted due_at
		Key:        "reminder:" + reminderID,
		Payload:    payload,
		Recurrence: recurrenceFor(in.Rrule, tz),
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

// ── Update / Complete / Delete ────────────────────────────────────────────────

// Update applies a partial/merge update to the reminder identified by id in
// scope. Fields are merged in Go (load → apply → write all mutable columns).
// It publishes "reminder.updated".
//
// Merge semantics:
//   - A nil pointer field in in is absent — the loaded value is preserved.
//   - OptionalString with Present=false is absent.
//   - OptionalString with Present=true, Value="" clears the nullable column.
//   - OptionalString with Present=true, Value!="" sets the column.
//
// Fire-job sync:
//   - A dueAt change on an ACTIVE reminder (fire_job_id present) calls
//     Reschedule on the existing job.
//   - A dueAt change on a COMPLETED reminder (no fire_job_id) updates the
//     row only — there is no live job to reschedule.
//   - Status="active" on a completed reminder (re-open path) calls Enqueue for
//     a fresh one-shot fire-job and stores the new fire_job_id.
//
// Rrule / recurrence seam:
//   - syncFireJobForRecurrenceChange is the stub for the recurring-reminders
//     slice (slice 4). When rrule changes, slice 4 will Cancel the old job and
//     Enqueue a recurring one. For now the stub is a no-op and rrule is stored
//     as a plain column.
func (s *Service) Update(ctx context.Context, scope store.Scope, id string, in UpdateInput) (Reminder, error) {
	// ── Validation ──────────────────────────────────────────────────────────
	var fields []httpx.FieldError

	if in.Title != nil {
		if strings.TrimSpace(*in.Title) == "" {
			fields = append(fields, httpx.FieldError{
				Pointer: "/title",
				Detail:  "title is required and must not be empty",
			})
		}
	}

	var newDueAt time.Time
	if in.DueAt != nil {
		if in.DueAt.IsZero() {
			fields = append(fields, httpx.FieldError{
				Pointer: "/dueAt",
				Detail:  "dueAt must not be zero",
			})
		} else {
			newDueAt = in.DueAt.UTC().Truncate(time.Second)
		}
	}

	if in.Status != nil {
		switch *in.Status {
		case "active", "completed":
			// valid
		default:
			fields = append(fields, httpx.FieldError{
				Pointer: "/status",
				Detail:  `status must be "active" or "completed"`,
			})
		}
	}

	// Return structural-field errors (title, dueAt, status) early to avoid a
	// wasted DB round-trip. Rrule validation is deferred until after the row
	// load so we can use the reminder's effective timezone (see below).
	if len(fields) > 0 {
		return Reminder{}, &ValidationError{Fields: fields}
	}

	// ── Load current row ─────────────────────────────────────────────────────
	row, err := db.New(s.st.Reader()).GetReminder(ctx, db.GetReminderParams{
		ID:     id,
		UserID: scope.UserID(),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Reminder{}, fmt.Errorf("reminders: update %q: %w", id, ErrNotFound)
		}
		return Reminder{}, fmt.Errorf("reminders: update %q: load: %w", id, err)
	}

	// rrule: validate when a new value is being set (not cleared, not absent).
	// Use the reminder's stored tz (row.Tz) — the same timezone that will be
	// stored and passed to the scheduler on Enqueue — keeping Update consistent
	// with Create (which validates in the effective tz). validateRrule parses the
	// rrule using rrulego.StrToROptionInLocation, mirroring the scheduler's call
	// exactly, so an rrule accepted here is always accepted at dispatch.
	if in.Rrule.Present && in.Rrule.Value != "" {
		if fe := validateRrule(in.Rrule.Value, row.Tz); fe != nil {
			return Reminder{}, &ValidationError{Fields: []httpx.FieldError{*fe}}
		}
	}

	wasCompleted := row.Status == "completed"
	prevFireJobID := ""
	if row.FireJobID.Valid {
		prevFireJobID = row.FireJobID.String
	}

	// ── Apply merge ──────────────────────────────────────────────────────────
	// Mutable fields merged from in onto the loaded row.

	title := row.Title
	if in.Title != nil {
		title = strings.TrimSpace(*in.Title)
	}

	notes := row.Notes
	if in.Notes.Present {
		notes = nullStr(in.Notes.Value)
	}

	// Use the canonical newDueAt when DueAt is present, else keep row's value.
	dueAtStr := row.DueAt
	if !newDueAt.IsZero() {
		dueAtStr = newDueAt.Format(time.RFC3339)
	}

	rrule := row.Rrule
	rruleChanged := false
	if in.Rrule.Present {
		newRrule := nullStr(in.Rrule.Value)
		if newRrule != rrule {
			rruleChanged = true
		}
		rrule = newRrule
	}

	autoComplete := row.AutoComplete
	if in.AutoComplete != nil {
		if *in.AutoComplete {
			autoComplete = 1
		} else {
			autoComplete = 0
		}
	}

	// Status / completed_at merge.
	status := row.Status
	completedAt := row.CompletedAt
	reopening := false
	if in.Status != nil {
		switch *in.Status {
		case "completed":
			// Transition to completed: set completed_at if not already set.
			// The REST adapter routes all PATCH requests (including those that
			// combine status=completed with other field changes) through Update,
			// so this branch is the single writer for the active→completed transition.
			status = "completed"
			if !completedAt.Valid {
				completedAt = sql.NullString{String: time.Now().UTC().Format(time.RFC3339), Valid: true}
			}
		case "active":
			if wasCompleted {
				// Re-open path: clear completed_at and enqueue a fresh job.
				status = "active"
				completedAt = sql.NullString{}
				reopening = true
			}
			// If already active, status stays active (no-op).
		}
	}

	// Determine the fire_job_id to persist after this update.
	fireJobID := row.FireJobID
	if reopening {
		// Clear it now; we'll stamp after Enqueue below.
		fireJobID = sql.NullString{}
	} else if status == "completed" && !wasCompleted {
		// Completing via Update — cancel the job and clear.
		if prevFireJobID != "" {
			if cancelErr := s.prod.Cancel(ctx, prevFireJobID); cancelErr != nil {
				s.logger.Error("reminders: update: cancel job on complete",
					"reminder_id", id, "job_id", prevFireJobID, "err", cancelErr)
			}
		}
		fireJobID = sql.NullString{}
	}

	// ── Persist ──────────────────────────────────────────────────────────────
	now := time.Now().UTC().Format(time.RFC3339)

	n, err := db.New(s.st.Writer()).UpdateReminder(ctx, db.UpdateReminderParams{
		Title:        title,
		Notes:        notes,
		DueAt:        dueAtStr,
		Rrule:        rrule,
		AutoComplete: autoComplete,
		Status:       status,
		CompletedAt:  completedAt,
		FireJobID:    fireJobID,
		UpdatedAt:    now,
		ID:           id,
		UserID:       scope.UserID(),
	})
	if err != nil {
		return Reminder{}, fmt.Errorf("reminders: update %q: write: %w", id, err)
	}
	if n == 0 {
		// Defense-in-depth: the GetReminder above scoped the load, but a row
		// deleted between load and write produces a silent 0-row update. Map
		// it to ErrNotFound to surface the race rather than silently returning
		// stale data.
		return Reminder{}, fmt.Errorf("reminders: update %q: %w", id, ErrNotFound)
	}

	// ── Fire-job sync ─────────────────────────────────────────────────────────
	//
	// The cases are mutually exclusive: reopening wins, then dueAt-shift, then
	// rrule-change. A switch makes the mutual exclusion explicit for gocritic.
	switch {
	case reopening:
		// Re-open: enqueue a fresh one-shot fire-job (same shape as Create).
		dueAtCanon, _ := time.Parse(time.RFC3339, dueAtStr)
		payload, marshalErr := json.Marshal(firePayload{ReminderID: id})
		if marshalErr != nil {
			s.logger.Error("reminders: update: marshal reopen payload", "reminder_id", id, "err", marshalErr)
		} else {
			newJobID, enqErr := s.prod.Enqueue(ctx, scheduler.EnqueueRequest{
				Kind:    "reminder.fire",
				Scope:   scope,
				RunAt:   dueAtCanon,
				Key:     "reminder:" + id,
				Payload: payload,
			})
			if enqErr != nil {
				s.logger.Error("reminders: update: enqueue reopen fire-job",
					"reminder_id", id, "err", enqErr)
			} else {
				// Stamp the new fire_job_id onto the row.
				_, stampErr := db.New(s.st.Writer()).SetReminderFireJobID(ctx, db.SetReminderFireJobIDParams{
					FireJobID: sql.NullString{String: newJobID, Valid: true},
					UpdatedAt: time.Now().UTC().Format(time.RFC3339),
					ID:        id,
					UserID:    scope.UserID(),
				})
				if stampErr != nil {
					s.logger.Error("reminders: update: stamp reopen fire_job_id",
						"reminder_id", id, "job_id", newJobID, "err", stampErr)
					// Best-effort cancel so the job doesn't orphan.
					if cancelErr := s.prod.Cancel(ctx, newJobID); cancelErr != nil {
						s.logger.Error("reminders: update: cancel orphaned reopen job",
							"reminder_id", id, "job_id", newJobID, "err", cancelErr)
					}
				}
				// The reload below picks up the stamped fire_job_id.
			}
		}

	case !newDueAt.IsZero() && prevFireJobID != "" && status == "active" && !rrule.Valid:
		// Pure dueAt time-shift on an active ONE-SHOT reminder: Reschedule the existing job.
		// For RECURRING reminders, a dueAt shift also changes the anchor for the RRULE
		// engine — cancel and re-enqueue so the scheduler's recurrence columns stay
		// consistent with the new anchor. That path is handled by the rruleChanged case
		// below (or the combined-change fallthrough when both change at once).
		if reschedErr := s.prod.Reschedule(ctx, prevFireJobID, newDueAt); reschedErr != nil {
			s.logger.Error("reminders: update: reschedule fire-job",
				"reminder_id", id, "job_id", prevFireJobID, "err", reschedErr)
		}

	case status == "active" &&
		((!newDueAt.IsZero() && rrule.Valid && prevFireJobID != "") ||
			(rruleChanged && prevFireJobID != "")):
		// Recurrence-affecting change on an ACTIVE reminder: either rrule changed,
		// or dueAt shifted on a recurring reminder (which changes the RRULE anchor).
		// Cancel the old job and enqueue a fresh one with the correct Recurrence field.
		// Non-active reminders must not have a live fire-job re-enqueued here; the
		// reopen path (above) handles the active transition separately.
		s.syncFireJobForRecurrenceChange(ctx, scope, id, prevFireJobID, dueAtStr, rrule, row.Tz)
	}

	// ── Load final state and publish ─────────────────────────────────────────
	final, err := s.Get(ctx, scope, id)
	if err != nil {
		return Reminder{}, fmt.Errorf("reminders: update %q: reload: %w", id, err)
	}

	// Determine the event type based on the status transition:
	//   active → completed  → "reminder.completed"
	//   completed → active  → "reminder.updated"  (reopen)
	//   no status change    → "reminder.updated"  (plain field update)
	eventType := "reminder.updated"
	if !wasCompleted && status == "completed" {
		eventType = "reminder.completed"
	}

	s.bus.Publish(scope.UserID(), events.Event{
		Type: eventType,
		Data: final,
	})

	return final, nil
}

// syncFireJobForRecurrenceChange is called when an Update changes the recurrence
// of an active reminder (rrule set/changed/cleared, or dueAt shifted on a
// recurring reminder). It cancels the old fire-job and enqueues a fresh one with
// the correct Recurrence field (recurring when newRrule is set, one-shot when nil).
// The new fire_job_id is stamped on the row; errors are logged and best-efforted.
//
// Callers pass the canonical values already held in memory (dueAtStr from the
// merged state, newRrule, and tz) to avoid a read-after-write round-trip through
// the store. The Service remains the single writer of fire_job_id.
func (s *Service) syncFireJobForRecurrenceChange(
	ctx context.Context,
	scope store.Scope,
	reminderID string,
	prevFireJobID string,
	dueAtStr string,
	newRrule sql.NullString,
	tz string,
) {
	// Cancel the old fire-job (best-effort: log and continue if this fails).
	if err := s.prod.Cancel(ctx, prevFireJobID); err != nil {
		s.logger.Error("reminders: syncFireJobForRecurrenceChange: cancel old job",
			"reminder_id", reminderID, "job_id", prevFireJobID, "err", err)
	}

	// Parse the canonical due_at from the caller's merged state (avoids re-reading
	// the row from the store — the value was just written by UpdateReminder above).
	dueAtCanon, err := time.Parse(time.RFC3339, dueAtStr)
	if err != nil {
		s.logger.Error("reminders: syncFireJobForRecurrenceChange: parse due_at",
			"reminder_id", reminderID, "due_at", dueAtStr, "err", err)
		return
	}

	payload, err := json.Marshal(firePayload{ReminderID: reminderID})
	if err != nil {
		s.logger.Error("reminders: syncFireJobForRecurrenceChange: marshal payload",
			"reminder_id", reminderID, "err", err)
		return
	}

	var recurrence *scheduler.Recurrence
	if newRrule.Valid && newRrule.String != "" {
		recurrence = recurrenceFor(newRrule.String, tz)
	}

	newJobID, err := s.prod.Enqueue(ctx, scheduler.EnqueueRequest{
		Kind:       "reminder.fire",
		Scope:      scope,
		RunAt:      dueAtCanon,
		Key:        "reminder:" + reminderID,
		Payload:    payload,
		Recurrence: recurrence,
	})
	if err != nil {
		s.logger.Error("reminders: syncFireJobForRecurrenceChange: enqueue new job",
			"reminder_id", reminderID, "err", err)
		return
	}

	// Stamp the new fire_job_id. The Service is the sole writer of this column.
	_, err = db.New(s.st.Writer()).SetReminderFireJobID(ctx, db.SetReminderFireJobIDParams{
		FireJobID: sql.NullString{String: newJobID, Valid: true},
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		ID:        reminderID,
		UserID:    scope.UserID(),
	})
	if err != nil {
		s.logger.Error("reminders: syncFireJobForRecurrenceChange: stamp fire_job_id",
			"reminder_id", reminderID, "job_id", newJobID, "err", err)
		// Best-effort cancel the orphaned job.
		if cancelErr := s.prod.Cancel(ctx, newJobID); cancelErr != nil {
			s.logger.Error("reminders: syncFireJobForRecurrenceChange: cancel orphaned job",
				"reminder_id", reminderID, "job_id", newJobID, "err", cancelErr)
		}
	}
}

// Complete marks the reminder as completed, cancels the active fire-job (if
// any), and publishes "reminder.completed". It is idempotent: calling Complete
// on an already-completed reminder is a no-op (the job is already gone).
func (s *Service) Complete(ctx context.Context, scope store.Scope, id string) (Reminder, error) {
	// Load current row to get the fire_job_id.
	row, err := db.New(s.st.Reader()).GetReminder(ctx, db.GetReminderParams{
		ID:     id,
		UserID: scope.UserID(),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Reminder{}, fmt.Errorf("reminders: complete %q: %w", id, ErrNotFound)
		}
		return Reminder{}, fmt.Errorf("reminders: complete %q: load: %w", id, err)
	}

	// Cancel the active fire-job (idempotent: skip when already gone).
	if row.FireJobID.Valid && row.FireJobID.String != "" {
		if cancelErr := s.prod.Cancel(ctx, row.FireJobID.String); cancelErr != nil {
			s.logger.Error("reminders: complete: cancel fire-job",
				"reminder_id", id, "job_id", row.FireJobID.String, "err", cancelErr)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	completedAt := row.CompletedAt
	if !completedAt.Valid {
		completedAt = sql.NullString{String: now, Valid: true}
	}

	n, err := db.New(s.st.Writer()).UpdateReminder(ctx, db.UpdateReminderParams{
		Title:        row.Title,
		Notes:        row.Notes,
		DueAt:        row.DueAt,
		Rrule:        row.Rrule,
		AutoComplete: row.AutoComplete,
		Status:       "completed",
		CompletedAt:  completedAt,
		FireJobID:    sql.NullString{}, // cleared
		UpdatedAt:    now,
		ID:           id,
		UserID:       scope.UserID(),
	})
	if err != nil {
		return Reminder{}, fmt.Errorf("reminders: complete %q: write: %w", id, err)
	}
	if n == 0 {
		// Defense-in-depth: row deleted between the load above and this write.
		return Reminder{}, fmt.Errorf("reminders: complete %q: %w", id, ErrNotFound)
	}

	final, err := s.Get(ctx, scope, id)
	if err != nil {
		return Reminder{}, fmt.Errorf("reminders: complete %q: reload: %w", id, err)
	}

	s.bus.Publish(scope.UserID(), events.Event{
		Type: "reminder.completed",
		Data: final,
	})

	return final, nil
}

// Delete removes the reminder row (scoped to the user), cancels the active
// fire-job (if any), and publishes "reminder.deleted" carrying the deleted
// reminder so SSE clients can render without a follow-up fetch.
func (s *Service) Delete(ctx context.Context, scope store.Scope, id string) error {
	// Load the row first so the event carries the full reminder and we know the
	// fire_job_id to cancel.
	row, err := db.New(s.st.Reader()).GetReminder(ctx, db.GetReminderParams{
		ID:     id,
		UserID: scope.UserID(),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reminders: delete %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("reminders: delete %q: load: %w", id, err)
	}

	snapshot := reminderFromRow(row)

	// Delete the row.
	_, err = db.New(s.st.Writer()).DeleteReminder(ctx, db.DeleteReminderParams{
		ID:     id,
		UserID: scope.UserID(),
	})
	if err != nil {
		return fmt.Errorf("reminders: delete %q: %w", id, err)
	}

	// Cancel the fire-job (best-effort, after row is gone so scheduler can't
	// re-fire it even if Cancel fails transiently).
	if row.FireJobID.Valid && row.FireJobID.String != "" {
		if cancelErr := s.prod.Cancel(ctx, row.FireJobID.String); cancelErr != nil {
			s.logger.Error("reminders: delete: cancel fire-job",
				"reminder_id", id, "job_id", row.FireJobID.String, "err", cancelErr)
		}
	}

	s.bus.Publish(scope.UserID(), events.Event{
		Type: "reminder.deleted",
		Data: snapshot,
	})

	return nil
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

	// Gate on status: only an active reminder proceeds. A non-active reminder
	// (completed or cancelled) here means the job was re-dispatched after the
	// reminder reached a terminal state (at-least-once reclaim, or a finite
	// series that just exhausted on the previous run). Return nil — do NOT
	// dead-letter, so the scheduler can clean up the job normally.
	if row.Status != "active" {
		s.logger.Info("reminders: fire: reminder no longer active — skipping",
			"reminder_id", p.ReminderID, "status", row.Status)
		return nil
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

	// ── Recurring branch ─────────────────────────────────────────────────────
	// When the reminder has an rrule, advance due_at to the next occurrence
	// (strictly after now), keep status=active, and ignore auto_complete.
	// The anchor for the RRULE engine is the current due_at in the reminder's
	// TZ — this mirrors the scheduler's nextRunAt anchor exactly (see
	// claimedRow.nextRunAt: anchor = current.In(loc)).
	//
	// Idempotency: "next occurrence strictly after now" is stable within the
	// scheduler's lease window. A reclaim re-run before now crosses the next
	// occurrence recomputes the same next instant, so due_at does not double-advance.
	if row.Rrule.Valid && row.Rrule.String != "" {
		// Parse the current due_at as the anchor for the RRULE engine.
		anchor, parseErr := time.Parse(time.RFC3339, row.DueAt)
		if parseErr != nil {
			return scheduler.Permanent(fmt.Errorf("reminders: fire: parse due_at %q: %w", row.DueAt, parseErr))
		}

		// Two-clock note: this handler anchors the advance on due_at (the reminder's
		// stored instant) and advances to "next occurrence strictly after now", which
		// mirrors the scheduler's own nextRunAt computation (claimedRow.nextRunAt anchors
		// on the job's current run_at in the reminder's TZ). For rules coarser than the
		// scheduler's lease window (the minimum supported granularity — reminders are not
		// sub-minute), both clocks land on the same occurrence, so due_at and the
		// scheduler's next run_at stay in sync. Sub-lease-window (sub-minute) recurrence
		// is explicitly out of scope; at supported granularities the two-clock advance
		// cannot drift.
		next, nextErr := nextOccurrence(row.Rrule.String, row.Tz, anchor, now)
		if nextErr != nil {
			return scheduler.Permanent(fmt.Errorf("reminders: fire: next occurrence: %w", nextErr))
		}
		if next.IsZero() {
			// Finite RRULE exhausted (COUNT/UNTIL reached) — transition to completed so
			// the reminder does not linger as active with no future due_at. This is
			// distinct from a user-initiated completion: the series simply ran out.
			s.logger.Info("reminders: fire: recurrence series exhausted — completing reminder",
				"reminder_id", row.ID, "rrule", row.Rrule.String, "last_fired_at", nowStr)
			_, err = db.New(s.st.Writer()).StampFiredAutoComplete(ctx, db.StampFiredAutoCompleteParams{
				LastFiredAt: sql.NullString{String: nowStr, Valid: true},
				CompletedAt: sql.NullString{String: nowStr, Valid: true},
				UpdatedAt:   nowStr,
				ID:          row.ID,
				UserID:      job.Scope.UserID(),
			})
			if err != nil {
				return fmt.Errorf("reminders: fire: stamp exhausted recurring: %w", err)
			}
			return nil
		}

		nextStr := next.UTC().Format(time.RFC3339)
		_, err = db.New(s.st.Writer()).StampFiredRecurring(ctx, db.StampFiredRecurringParams{
			LastFiredAt: sql.NullString{String: nowStr, Valid: true},
			DueAt:       nextStr,
			UpdatedAt:   nowStr,
			ID:          row.ID,
			UserID:      job.Scope.UserID(),
		})
		if err != nil {
			return fmt.Errorf("reminders: fire: stamp recurring: %w", err)
		}
		// Return nil so the scheduler advances the recurring job to the next occurrence.
		return nil
	}

	// ── One-shot branch ──────────────────────────────────────────────────────
	// Stamp last_fired_at and optionally auto-complete.
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

// recurrenceFor builds a *scheduler.Recurrence for a reminder's rrule and tz when
// the rrule is set, or nil for a one-shot reminder. This is the single place that
// maps reminder fields onto the scheduler's Recurrence type.
func recurrenceFor(rruleStr, tz string) *scheduler.Recurrence {
	if rruleStr == "" {
		return nil
	}
	return &scheduler.Recurrence{RRULE: rruleStr, TZ: tz}
}

// validateRrule parses rruleStr under the given IANA timezone and returns a
// field error when parsing fails. The tz must already be valid (caller ensures
// this before calling). Returns nil when rruleStr is empty (no validation needed).
//
// Callers must pass the same timezone that will be stored and enqueued so that
// the invariant "accepted here ⟹ accepted by the scheduler at dispatch" holds:
//   - Create: passes the effective tz resolved from the input / profile / "UTC".
//   - Update: passes row.Tz — the reminder's stored timezone — after loading the row.
//
// The parsing mirrors the scheduler's StrToROptionInLocation call exactly.
func validateRrule(rruleStr, tz string) *httpx.FieldError {
	if rruleStr == "" {
		return nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		// tz validation is a separate field; rrule is not at fault here.
		return nil
	}
	_, err = rrulego.StrToROptionInLocation(rruleStr, loc)
	if err != nil {
		return &httpx.FieldError{
			Pointer: "/rrule",
			Detail:  `rrule must be a valid RFC 5545 recurrence rule string, e.g. "FREQ=WEEKLY;BYDAY=MO"`,
		}
	}
	return nil
}

// nextOccurrence computes the next occurrence of rruleStr in the given IANA tz,
// strictly after now, anchored at anchor (the current due_at converted to the
// reminder's tz). This mirrors the scheduler's nextRunAt (claimedRow) semantics
// exactly:
//   - anchor = current due_at in the reminder's TZ (preserves wall-clock phase).
//   - Dtstart = anchor (on-phase seed for the RRULE engine).
//   - rule.After(now, false) = strictly-after-now next occurrence.
//
// A zero time.Time is returned (with a nil error) when the RRULE series is
// exhausted (finite COUNT/UNTIL with no future occurrence).
func nextOccurrence(rruleStr, tz string, anchor, now time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("reminders: load tz %q: %w", tz, err)
	}

	// Anchor in the reminder's TZ — preserves the wall-clock time-of-day (e.g. always 8am).
	anchorLocal := anchor.In(loc)

	ropt, err := rrulego.StrToROptionInLocation(rruleStr, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("reminders: parse rrule %q: %w", rruleStr, err)
	}
	ropt.Dtstart = anchorLocal

	rule, err := rrulego.NewRRule(*ropt)
	if err != nil {
		return time.Time{}, fmt.Errorf("reminders: build rrule %q: %w", rruleStr, err)
	}

	next := rule.After(now, false) // strictly after now — skips missed backlog
	return next, nil
}

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

// Routes registers the reminders REST endpoints on r.
//
//	POST   /api/v1/reminders      → createHandler (201)
//	GET    /api/v1/reminders      → listHandler   (200)
//	GET    /api/v1/reminders/{id} → getHandler    (200)
//	PATCH  /api/v1/reminders/{id} → patchHandler  (200)
//	DELETE /api/v1/reminders/{id} → deleteHandler (204)
func (m *Module) Routes(r *httpx.Router) {
	r.HandleFunc("POST /api/v1/reminders", m.createHandler)
	r.HandleFunc("GET /api/v1/reminders", m.listHandler)
	r.HandleFunc("GET /api/v1/reminders/{id}", m.getHandler)
	r.HandleFunc("PATCH /api/v1/reminders/{id}", m.patchHandler)
	r.HandleFunc("DELETE /api/v1/reminders/{id}", m.deleteHandler)
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

// patchHandler handles PATCH /api/v1/reminders/{id}.
//
// All cases are routed through Service.Update so that field changes and status
// transitions are always applied in a single coherent write:
//   - status="completed": active→completed (cancels fire-job, sets completed_at,
//     emits "reminder.completed") — other fields in the body are also persisted.
//   - status="active" on a completed reminder: re-open (enqueues a fresh
//     fire-job, clears completed_at, emits "reminder.updated").
//   - any other combination: plain merge-patch (emits "reminder.updated").
//
// Returns 200 with the updated reminder.
func (m *Module) patchHandler(w http.ResponseWriter, r *http.Request) {
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

	// Decode body using the raw-JSON optional pattern so we can distinguish
	// absent fields from null.
	var body patchRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteProblem(w, r, httpx.MalformedBodyProblem())
		return
	}

	// ── Build UpdateInput from the patch body ─────────────────────────────────
	in := UpdateInput{}

	if body.Title != nil {
		in.Title = body.Title
	}
	// Notes: use raw JSON to distinguish absent / null / value.
	if body.Notes != nil {
		if string(*body.Notes) == "null" {
			in.Notes = ClearString()
		} else {
			var s string
			if err := json.Unmarshal(*body.Notes, &s); err != nil {
				httpx.WriteProblem(w, r, httpx.ValidationProblem(
					"validation_error",
					"Request validation failed.",
					httpx.FieldError{Pointer: "/notes", Detail: "notes must be a string or null"},
				))
				return
			}
			in.Notes = SetString(s)
		}
	}
	// DueAt.
	if body.DueAt != nil {
		parsed, err := time.Parse(time.RFC3339, *body.DueAt)
		if err != nil {
			httpx.WriteProblem(w, r, httpx.ValidationProblem(
				"validation_error",
				"Request validation failed.",
				httpx.FieldError{Pointer: "/dueAt", Detail: "dueAt must be an RFC 3339 timestamp"},
			))
			return
		}
		in.DueAt = &parsed
	}
	// Rrule: same raw-JSON pattern as notes.
	if body.Rrule != nil {
		if string(*body.Rrule) == "null" {
			in.Rrule = ClearString()
		} else {
			var s string
			if err := json.Unmarshal(*body.Rrule, &s); err != nil {
				httpx.WriteProblem(w, r, httpx.ValidationProblem(
					"validation_error",
					"Request validation failed.",
					httpx.FieldError{Pointer: "/rrule", Detail: "rrule must be a string or null"},
				))
				return
			}
			in.Rrule = SetString(s)
		}
	}
	if body.AutoComplete != nil {
		in.AutoComplete = body.AutoComplete
	}
	if body.Status != nil {
		in.Status = body.Status
	}

	// ── Route all cases through Update ───────────────────────────────────────
	// Update's status-transition branches handle every case:
	//   status="completed"  → active→completed (cancels job, sets completed_at)
	//   status="active"     → completed→active re-open (enqueues fresh job)
	//   no status           → plain field merge + optional dueAt Reschedule
	//
	// Funnelling through Update (rather than the old early-return to Complete)
	// ensures that when a caller sends {"status":"completed","title":"X"}, BOTH
	// the title change and the completion are persisted in one coherent write.
	// Service.Complete remains public for direct callers (AI tools, etc.) but
	// the PATCH handler no longer bypasses Update.
	result, err := m.svc.Update(r.Context(), scope, reminderID, in)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.WriteProblem(w, r, Problem404())
			return
		}
		var valErr *ValidationError
		if errors.As(err, &valErr) {
			httpx.WriteProblem(w, r, httpx.ValidationProblem(
				"validation_error",
				"Request validation failed.",
				valErr.Fields...,
			))
			return
		}
		httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "update_reminder_failed", err.Error()))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		m.logger.Error("reminders: encode patch response", "err", err)
	}
}

// deleteHandler handles DELETE /api/v1/reminders/{id}.
// Returns 204 No Content on success; 404 when the reminder does not exist or
// belongs to a different user.
func (m *Module) deleteHandler(w http.ResponseWriter, r *http.Request) {
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

	if err := m.svc.Delete(r.Context(), scope, reminderID); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.WriteProblem(w, r, Problem404())
			return
		}
		httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "delete_reminder_failed", err.Error()))
		return
	}

	w.WriteHeader(http.StatusNoContent)
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

// patchRequestBody is the JSON shape for PATCH /api/v1/reminders/{id}.
// camelCase per the HTTP house guide.
//
// Notes and Rrule use json.RawMessage so we can distinguish three states at
// the HTTP layer:
//   - field absent from JSON object → pointer is nil (leave unchanged)
//   - field present as JSON null    → pointer is non-nil, value is `null`
//     (clear the nullable column)
//   - field present with a value    → pointer is non-nil, unmarshal to string
//     (set the column)
//
// All other fields use plain *T: absent → nil (leave unchanged), present →
// non-nil (apply). There is no "present + null" semantic for non-nullable
// columns (title, dueAt, autoComplete, status).
type patchRequestBody struct {
	Title        *string          `json:"title"`
	Notes        *json.RawMessage `json:"notes"`
	DueAt        *string          `json:"dueAt"`
	Rrule        *json.RawMessage `json:"rrule"`
	AutoComplete *bool            `json:"autoComplete"`
	Status       *string          `json:"status"`
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
