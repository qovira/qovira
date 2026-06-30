package events

// handler_internal_test.go unit-tests the pure SSE frame serialization (formatFrame) in the events package
// itself, where it can feed multi-line payloads the production path (compact json.Marshal) never produces —
// proving the defensive data:-line split is correct rather than dead.

import (
	"strings"
	"testing"
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
