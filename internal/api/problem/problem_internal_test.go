package problem

// White-box tests for the unexported locationToPointer and messageToCode helpers. Being in the same package
// lets us call the unexported functions directly, so the exported LocationToPointer / MessageToCode wrappers
// that were in the previous iteration are not needed and have been removed.

import "testing"

// locationToPointer table — load-bearing logic for RFC 6901 pointer conversion.

func TestLocationToPointer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		location string
		want     string
	}{
		// body segment converted to RFC 6901 pointer.
		{location: "body.items[0].dueDate", want: "/items/0/dueDate"},
		{location: "body.name", want: "/name"},
		{location: "body.friends[1].active", want: "/friends/1/active"},
		{location: "body.a[0][1]", want: "/a/0/1"},
		// whole-body pointer (bare "body")
		{location: "body", want: ""},
		// RFC 6901 escaping in field names
		{location: "body.a~b", want: "/a~0b"},
		{location: "body.a/b", want: "/a~1b"},
		// non-body prefixes — best-effort (strips prefix, adds leading /)
		{location: "query.limit", want: "/limit"},
		{location: "path.thing-id", want: "/thing-id"},
		{location: "header.X-Foo", want: "/X-Foo"},
		// edge: empty string produces empty pointer
		{location: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.location, func(t *testing.T) {
			t.Parallel()
			got := locationToPointer(tc.location)
			if got != tc.want {
				t.Errorf("locationToPointer(%q): want %q, got %q", tc.location, tc.want, got)
			}
		})
	}
}

// messageToCode table — classify Huma validation message text to house codes.
// All message strings are taken directly from validation/messages.go v2.38.0.

func TestMessageToCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		message string
		want    string
	}{
		// required
		{message: "expected required property email to be present", want: "required"},
		// minimum / maximum (numeric)
		{message: "expected number >= 1", want: "min"},
		{message: "expected number > 0", want: "min"},
		{message: "expected number <= 100", want: "max"},
		{message: "expected number < 200", want: "max"},
		// min_length / max_length
		{message: "expected length >= 3", want: "min_length"},
		{message: "expected length <= 50", want: "max_length"},
		// min / max items
		{message: "expected array length >= 1", want: "min"},
		{message: "expected array length <= 10", want: "max"},
		// pattern — MsgExpectedMatchPattern / MsgExpectedBePattern (non-format suffix)
		{message: "expected string to match pattern ^[a-z]+$", want: "pattern"},
		{message: "expected string to be ^[a-z]+$", want: "pattern"},
		// format — RFC-prefixed messages (MsgExpectedRFC*)
		{message: "expected string to be RFC 3339 date-time", want: "format"},
		{message: "expected string to be RFC 1123 date-time", want: "format"},
		{message: "expected string to be RFC 3339 date", want: "format"},
		{message: "expected string to be RFC 3339 time", want: "format"},
		{message: "expected string to be RFC 5322 email: ...", want: "format"},
		{message: "expected string to be RFC 4122 uuid: ...", want: "format"},
		{message: "expected string to be RFC 5890 hostname", want: "format"},
		{message: "expected string to be RFC 2673 ipv4", want: "format"},
		{message: "expected string to be RFC 2373 ipv6", want: "format"},
		{message: "expected string to be RFC 3986 uri: ...", want: "format"},
		{message: "expected string to be RFC 6570 uri-template", want: "format"},
		{message: "expected string to be RFC 6901 json-pointer", want: "format"},
		{message: "expected string to be RFC 6901 relative-json-pointer", want: "format"},
		// format — base64 (MsgExpectedBase64String)
		{message: "expected string to be base64 encoded", want: "format"},
		// format — "either" prefix (MsgExpectedRFCIPAddr): "expected string to be either RFC..."
		{message: "expected string to be either RFC 2673 ipv4 or RFC 2373 ipv6", want: "format"},
		// format — "regex:" prefix (MsgExpectedRegexp): "expected string to be regex: …"
		{message: "expected string to be regex: .*", want: "format"},
		// enum
		{message: `expected value to be one of "foo, bar"`, want: "enum"},
		// type
		{message: "expected boolean", want: "type"},
		{message: "expected number", want: "type"},
		{message: "expected integer", want: "type"},
		{message: "expected string", want: "type"},
		{message: "expected array", want: "type"},
		{message: "expected object", want: "type"},
		// unknown → default
		{message: "some unrecognized message", want: "invalid"},
		{message: "", want: "invalid"},
	}

	for _, tc := range tests {
		t.Run(tc.message, func(t *testing.T) {
			t.Parallel()
			got := messageToCode(tc.message)
			if got != tc.want {
				t.Errorf("messageToCode(%q): want %q, got %q", tc.message, tc.want, got)
			}
		})
	}
}
