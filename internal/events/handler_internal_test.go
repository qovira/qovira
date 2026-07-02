package events

// handler_internal_test.go unit-tests the pure SSE frame serialization (formatFrame) and the NewHandler
// constructor in the events package itself. The internal package scope lets tests type-assert the returned
// http.Handler to *handler and read unexported fields like .timing — impossible from the external _test package.

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestFormatFrame_SingleLine(t *testing.T) {
	t.Parallel()

	got := string(formatFrame("system.ready", []byte(`{"connectionId":"req_abc"}`)))
	want := "event: system.ready\n" +
		"data: {\"connectionId\":\"req_abc\"}\n" +
		"\n"

	if got != want {
		t.Errorf("formatFrame single-line:\n got %q\nwant %q", got, want)
	}

	if strings.Contains(got, "id:") {
		t.Error("formatFrame must not emit an id: field")
	}
}

// TestFormatFrame_MultiLineSplitsAcrossDataLines proves the SSE-spec requirement: a payload containing raw
// newlines is split into one "data: " line per segment, so no raw newline ever sits inside a data: value
// (which the SSE parser would read as a frame boundary). Without the split this test fails — the whole
// payload would land on a single data: line carrying embedded newlines.
func TestFormatFrame_MultiLineSplitsAcrossDataLines(t *testing.T) {
	t.Parallel()

	// Pre-formatted (indented) JSON — exactly the kind of multi-line payload a future producer might emit.
	payload := "{\n  \"a\": 1,\n  \"b\": 2\n}"

	got := string(formatFrame("test.multi", []byte(payload)))
	want := "event: test.multi\n" +
		"data: {\n" +
		"data:   \"a\": 1,\n" +
		"data:   \"b\": 2\n" +
		"data: }\n" +
		"\n"

	if got != want {
		t.Errorf("formatFrame multi-line split:\n got %q\nwant %q", got, want)
	}

	// The only newlines in the frame must be the line terminators we wrote — none may sit *inside* a
	// data: value. Every line between the event: line and the terminating blank line must start with
	// "data: ".
	lines := strings.Split(strings.TrimSuffix(got, "\n\n"), "\n")
	for _, line := range lines[1:] { // skip the event: line
		if !strings.HasPrefix(line, "data: ") {
			t.Errorf("multi-line frame has a non-data: continuation line %q — split is broken", line)
		}
	}
}

// TestFormatFrame_NoNewlinePayload covers a single-segment payload that contains no newlines — the common
// production case (compact json.Marshal output). Ensures the single-line path emits exactly one data: line and
// no spurious blank data: lines from an off-by-one in the split logic.
func TestFormatFrame_NoNewlinePayload(t *testing.T) {
	t.Parallel()

	got := string(formatFrame("system.ping", []byte(`{"time":"2026-06-30T00:00:00Z"}`)))
	want := "event: system.ping\n" +
		"data: {\"time\":\"2026-06-30T00:00:00Z\"}\n" +
		"\n"

	if got != want {
		t.Errorf("formatFrame no-newline payload:\n got %q\nwant %q", got, want)
	}
}

// TestNewHandler_InvalidTimingsFallsBackToDefault verifies that NewHandler rejects a Timings where
// PingInterval >= WriteDeadline and falls back to DefaultTimings wholesale, leaving a valid Timings
// caller's values unchanged.
func TestNewHandler_InvalidTimingsFallsBackToDefault(t *testing.T) {
	t.Parallel()

	hub := New(DefaultBufferSize)
	log := slog.Default()

	// (a) Invalid: PingInterval (30 s) >= WriteDeadline (15 s) — violates the invariant.
	bad := Timings{
		PingInterval:  30 * time.Second,
		WriteDeadline: 15 * time.Second,
		RetryHint:     3 * time.Second,
	}
	h := NewHandler(hub, log, bad).(*handler)

	if h.timing != DefaultTimings {
		t.Errorf("invalid Timings: expected DefaultTimings %+v, got %+v", DefaultTimings, h.timing)
	}

	// (b) Valid custom Timings must be preserved unchanged.
	good := Timings{
		PingInterval:  5 * time.Second,
		WriteDeadline: 20 * time.Second,
		RetryHint:     2 * time.Second,
	}
	h2 := NewHandler(hub, log, good).(*handler)

	if h2.timing != good {
		t.Errorf("valid Timings: expected %+v preserved, got %+v", good, h2.timing)
	}
}
