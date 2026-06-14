package reminders

// tools.go — AI tool adapters for the reminders module (slice 5).
//
// Tools() returns four capability.Tool instances that are thin adapters over the
// same Service methods used by the REST handlers. This proves one-service-two-surfaces:
// all validation, fire-job sync, and event emission stay in the Service layer.
//
// The adapters decode structured camelCase args (RFC 3339 timestamps, RFC 5545
// RRULE strings, IANA timezone names) — the model converts natural language to
// these formats using the now+timezone injected into context by the harness.
// No natural-language date parsing is performed here.
//
// Error mapping:
//   - *ValidationError (same that yields 422 on REST) → *capability.ToolError
//     (model-correctable; turn continues).
//   - ErrNotFound (non-existent or other-user's id) → *capability.ToolError
//     (model-correctable; turn continues).
//   - All other errors (DB down etc.) pass through as plain errors (abort turn).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/store"
)

// ── Args structs ──────────────────────────────────────────────────────────────

// createReminderArgs mirrors CreateInput fields for the AI tool boundary.
// All timestamps are RFC 3339; rrule is RFC 5545; tz is an IANA zone name.
// The model must produce these structured values — no natural-language parsing.
type createReminderArgs struct {
	// Title is the reminder headline. Required; must be non-empty after trimming.
	Title string `json:"title"`
	// Notes is optional free text.
	Notes string `json:"notes,omitempty"`
	// DueAt is the RFC 3339 timestamp for the first (or only) fire instant.
	// The model must convert natural language (e.g. "Thursday 8am") to this
	// format using the user's current time and timezone from the harness context.
	DueAt string `json:"dueAt"`
	// Rrule is an optional RFC 5545 RRULE string (e.g. "FREQ=WEEKLY;BYDAY=MO").
	Rrule string `json:"rrule,omitempty"`
	// Tz is an optional IANA timezone name (e.g. "America/New_York"). When
	// omitted the Service defaults to the user's profile timezone or "UTC".
	Tz string `json:"tz,omitempty"`
	// AutoComplete controls whether the reminder auto-completes when it fires.
	// Omit to use the default (true).
	AutoComplete *bool `json:"autoComplete,omitempty"`
}

// updateReminderArgs mirrors UpdateInput fields for the AI tool boundary.
// Only present fields are applied; absent fields leave the stored value unchanged.
// Note: tz is intentionally absent — it is immutable after creation.
//
// For nullable string columns (notes, rrule) the three-way semantics are:
//   - absent field       → leave unchanged (JSON omitempty + OptionalString{Present:false}).
//   - present, null      → clear the column (JSON null → OptionalString{Present:true, Value:""}).
//   - present, non-empty → set the column (OptionalString{Present:true, Value:"x"}).
//
// Because encoding/json unmarshals an absent optional field to its zero value
// (Present=false), and JSON null to a non-nil *json.RawMessage, we need an
// intermediate raw-message type for the two nullable fields. The same approach
// is used by patchRequestBody in the REST handler.
type updateReminderArgs struct {
	// ID of the reminder to update. Required.
	ID string `json:"id"`
	// Title replaces the reminder's title when present. Must be non-empty.
	Title *string `json:"title,omitempty"`
	// Notes replaces (or clears) the notes column when present.
	// Set to null to clear; set to a string to replace; omit to leave unchanged.
	Notes *json.RawMessage `json:"notes,omitempty"`
	// DueAt replaces the fire instant when present. Must be RFC 3339.
	DueAt *string `json:"dueAt,omitempty"`
	// Rrule replaces (or clears) the RRULE when present.
	// Set to null to clear; set to an RFC 5545 string to replace; omit to leave.
	Rrule *json.RawMessage `json:"rrule,omitempty"`
	// AutoComplete replaces the auto-complete flag when present.
	AutoComplete *bool `json:"autoComplete,omitempty"`
	// Status transitions the reminder. Accepted: "active", "completed".
	Status *string `json:"status,omitempty"`
}

// completeReminderArgs holds the id for the complete_reminder tool.
type completeReminderArgs struct {
	// ID of the reminder to complete. Required.
	ID string `json:"id"`
}

// deleteReminderArgs holds the id for the delete_reminder tool.
type deleteReminderArgs struct {
	// ID of the reminder to delete. Required.
	ID string `json:"id"`
}

// ── JSON Schemas ──────────────────────────────────────────────────────────────

// schemaCreateReminder is the hand-authored JSON Schema for createReminderArgs.
// The model uses this to understand the expected structure and field semantics.
var schemaCreateReminder = json.RawMessage(`{
  "type": "object",
  "required": ["title", "dueAt"],
  "properties": {
    "title": {
      "type": "string",
      "description": "The reminder headline. Required; must not be empty."
    },
    "notes": {
      "type": "string",
      "description": "Optional free-text note for the reminder."
    },
    "dueAt": {
      "type": "string",
      "format": "date-time",
      "description": "RFC 3339 timestamp for when the reminder should fire (e.g. \"2026-06-14T09:00:00Z\"). Convert natural-language dates like \"Thursday 8am\" to RFC 3339 using the user's current time and timezone from context."
    },
    "rrule": {
      "type": "string",
      "description": "Optional RFC 5545 RRULE string for recurring reminders (e.g. \"FREQ=WEEKLY;BYDAY=MO\"). Omit for a one-shot reminder."
    },
    "tz": {
      "type": "string",
      "description": "Optional IANA timezone name (e.g. \"America/New_York\"). When omitted, defaults to the user's profile timezone or UTC. This is snapshotted at creation and cannot be changed later."
    },
    "autoComplete": {
      "type": "boolean",
      "description": "When true (default), the reminder automatically transitions to completed when it fires. Set to false to keep it active after firing."
    }
  },
  "additionalProperties": false
}`)

// schemaUpdateReminder is the hand-authored JSON Schema for updateReminderArgs.
var schemaUpdateReminder = json.RawMessage(`{
  "type": "object",
  "required": ["id"],
  "properties": {
    "id": {
      "type": "string",
      "description": "The ID of the reminder to update."
    },
    "title": {
      "type": "string",
      "description": "Replaces the reminder title. Must not be empty."
    },
    "notes": {
      "type": ["string", "null"],
      "description": "Replaces the notes field. Pass null to clear it; omit to leave it unchanged."
    },
    "dueAt": {
      "type": "string",
      "format": "date-time",
      "description": "RFC 3339 timestamp to reschedule the reminder's fire time. Convert natural-language dates to RFC 3339 first."
    },
    "rrule": {
      "type": ["string", "null"],
      "description": "RFC 5545 RRULE string. Pass null to remove recurrence (make one-shot); omit to leave unchanged."
    },
    "autoComplete": {
      "type": "boolean",
      "description": "Replaces the auto-complete flag."
    },
    "status": {
      "type": "string",
      "enum": ["active", "completed"],
      "description": "Transitions the reminder status. Use \"completed\" to mark done; \"active\" to re-open a completed reminder."
    }
  },
  "additionalProperties": false
}`)

// schemaCompleteReminder is the hand-authored JSON Schema for completeReminderArgs.
var schemaCompleteReminder = json.RawMessage(`{
  "type": "object",
  "required": ["id"],
  "properties": {
    "id": {
      "type": "string",
      "description": "The ID of the reminder to mark as completed."
    }
  },
  "additionalProperties": false
}`)

// schemaDeleteReminder is the hand-authored JSON Schema for deleteReminderArgs.
var schemaDeleteReminder = json.RawMessage(`{
  "type": "object",
  "required": ["id"],
  "properties": {
    "id": {
      "type": "string",
      "description": "The ID of the reminder to permanently delete."
    }
  },
  "additionalProperties": false
}`)

// ── Tools() ───────────────────────────────────────────────────────────────────

// Tools returns the four AI capability tools contributed by the reminders module.
// Each tool is a thin adapter over the same Service methods used by the REST
// handlers, proving one-service-two-surfaces. Risk tiers are declared here;
// confirmation and trust-level enforcement are the harness's responsibility.
func (m *Module) Tools() []capability.Tool {
	return []capability.Tool{
		capability.NewTool(
			"create_reminder",
			"Create a new reminder that will fire at the specified time. "+
				"The model must provide dueAt as an RFC 3339 timestamp (convert "+
				"natural-language dates using the user's current time and timezone). "+
				"Returns the created reminder including its id for future reference.",
			schemaCreateReminder,
			capability.RiskWrite,
			m.toolCreate,
		),
		capability.NewTool(
			"update_reminder",
			"Update one or more fields of an existing reminder. "+
				"Only provided fields are changed; absent fields are left unchanged. "+
				"Use null for notes or rrule to clear them. "+
				"Set status to \"completed\" to mark done, or \"active\" to re-open. "+
				"Returns the updated reminder.",
			schemaUpdateReminder,
			capability.RiskWrite,
			m.toolUpdate,
		),
		capability.NewTool(
			"complete_reminder",
			"Mark a reminder as completed and cancel its scheduled fire-job. "+
				"Idempotent: calling on an already-completed reminder is a no-op. "+
				"Returns the completed reminder.",
			schemaCompleteReminder,
			capability.RiskWrite,
			m.toolComplete,
		),
		capability.NewTool(
			"delete_reminder",
			"Permanently delete a reminder and cancel its scheduled fire-job. "+
				"This action is irreversible. Returns nothing on success.",
			schemaDeleteReminder,
			capability.RiskDestructive,
			m.toolDelete,
		),
	}
}

// ── tool handlers ─────────────────────────────────────────────────────────────

// toolCreate is the typed handler for create_reminder.
// It decodes the structured args, builds a CreateInput, and delegates to Service.Create.
func (m *Module) toolCreate(ctx context.Context, scope store.Scope, args createReminderArgs) (capability.Result, error) {
	// Parse dueAt: must be RFC 3339 (model provides it; natural language is rejected).
	if args.DueAt == "" {
		return nil, &capability.ToolError{
			Code:    "validation_failed",
			Message: "dueAt is required and must be an RFC 3339 timestamp (e.g. \"2026-06-14T09:00:00Z\")",
		}
	}
	dueAt, err := time.Parse(time.RFC3339, args.DueAt)
	if err != nil {
		return nil, &capability.ToolError{
			Code:    "validation_failed",
			Message: fmt.Sprintf("dueAt %q is not a valid RFC 3339 timestamp; convert the date to RFC 3339 format first", args.DueAt),
		}
	}

	in := CreateInput{
		Title:        args.Title,
		Notes:        args.Notes,
		DueAt:        dueAt,
		Tz:           args.Tz,
		AutoComplete: args.AutoComplete,
		Rrule:        args.Rrule,
	}

	result, err := m.svc.Create(ctx, scope, in)
	if err != nil {
		return nil, mapServiceError(err)
	}
	return result, nil
}

// toolUpdate is the typed handler for update_reminder.
// It decodes the structured args (using raw JSON for nullable fields to distinguish
// absent/null/value) and delegates to Service.Update with the same merge semantics
// as the PATCH handler.
func (m *Module) toolUpdate(ctx context.Context, scope store.Scope, args updateReminderArgs) (capability.Result, error) {
	if args.ID == "" {
		return nil, &capability.ToolError{
			Code:    "validation_failed",
			Message: "id is required",
		}
	}

	in := UpdateInput{}

	// Title: non-nil *string.
	if args.Title != nil {
		in.Title = args.Title
	}

	// Notes: three-way via raw JSON (absent/null/value).
	if args.Notes != nil {
		raw := string(*args.Notes)
		if raw == "null" {
			in.Notes = ClearString()
		} else {
			var s string
			if err := json.Unmarshal(*args.Notes, &s); err != nil {
				return nil, &capability.ToolError{
					Code:    "validation_failed",
					Message: "notes must be a string or null",
				}
			}
			in.Notes = SetString(s)
		}
	}

	// DueAt: parse when present.
	if args.DueAt != nil {
		parsed, err := time.Parse(time.RFC3339, *args.DueAt)
		if err != nil {
			return nil, &capability.ToolError{
				Code:    "validation_failed",
				Message: fmt.Sprintf("dueAt %q is not a valid RFC 3339 timestamp; convert the date to RFC 3339 format first", *args.DueAt),
			}
		}
		in.DueAt = &parsed
	}

	// Rrule: three-way via raw JSON (absent/null/value).
	if args.Rrule != nil {
		raw := string(*args.Rrule)
		if raw == "null" {
			in.Rrule = ClearString()
		} else {
			var s string
			if err := json.Unmarshal(*args.Rrule, &s); err != nil {
				return nil, &capability.ToolError{
					Code:    "validation_failed",
					Message: "rrule must be an RFC 5545 string or null",
				}
			}
			in.Rrule = SetString(s)
		}
	}

	// AutoComplete: direct pointer.
	if args.AutoComplete != nil {
		in.AutoComplete = args.AutoComplete
	}

	// Status: direct pointer.
	if args.Status != nil {
		in.Status = args.Status
	}

	result, err := m.svc.Update(ctx, scope, args.ID, in)
	if err != nil {
		return nil, mapServiceError(err)
	}
	return result, nil
}

// toolComplete is the typed handler for complete_reminder.
func (m *Module) toolComplete(ctx context.Context, scope store.Scope, args completeReminderArgs) (capability.Result, error) {
	if args.ID == "" {
		return nil, &capability.ToolError{
			Code:    "validation_failed",
			Message: "id is required",
		}
	}

	result, err := m.svc.Complete(ctx, scope, args.ID)
	if err != nil {
		return nil, mapServiceError(err)
	}
	return result, nil
}

// toolDelete is the typed handler for delete_reminder.
// The RiskDestructive tier is declared on the Tool; confirmation enforcement is
// the harness's responsibility — this adapter does not duplicate it.
func (m *Module) toolDelete(ctx context.Context, scope store.Scope, args deleteReminderArgs) (capability.Result, error) {
	if args.ID == "" {
		return nil, &capability.ToolError{
			Code:    "validation_failed",
			Message: "id is required",
		}
	}

	if err := m.svc.Delete(ctx, scope, args.ID); err != nil {
		return nil, mapServiceError(err)
	}
	// Delete has no meaningful entity to return. A struct{} result is
	// JSON-marshalable (→ {}) and satisfies the nilnil linter.
	return struct{}{}, nil
}

// ── error mapping ─────────────────────────────────────────────────────────────

// mapServiceError maps domain errors to *capability.ToolError for model-correctable
// failures, leaving genuine infrastructure errors as plain errors so the harness
// aborts the turn.
//
// Mapped (model-correctable):
//   - *ValidationError → *ToolError with code "validation_failed" and a
//     human-readable message listing all failing fields.
//   - ErrNotFound → *ToolError with code "not_found".
//
// Not mapped (infrastructure — plain error, abort turn):
//   - Everything else (DB errors, scheduler failures, etc.).
func mapServiceError(err error) error {
	// ValidationError mirrors the REST 422 mapping.
	var valErr *ValidationError
	if errors.As(err, &valErr) {
		msgs := make([]string, 0, len(valErr.Fields))
		for _, f := range valErr.Fields {
			msgs = append(msgs, fmt.Sprintf("%s: %s", f.Pointer, f.Detail))
		}
		return &capability.ToolError{
			Code:    "validation_failed",
			Message: "validation failed — " + strings.Join(msgs, "; "),
		}
	}

	// ErrNotFound: model provided a wrong or another user's id.
	if errors.Is(err, ErrNotFound) {
		return &capability.ToolError{
			Code:    "not_found",
			Message: "reminder not found; verify the id and try again",
		}
	}

	// Everything else is an infrastructure error — pass through to abort the turn.
	return err
}
