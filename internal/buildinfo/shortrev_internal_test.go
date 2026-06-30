package buildinfo

import "testing"

// TestShortRevision covers the development-fallback abbreviation: a full 40-character revision is truncated to
// the documented short form, the "-dirty" suffix is appended when the tree was modified, an already-short
// revision is left alone, and an empty revision stays empty (the caller maps that to "unknown").
func TestShortRevision(t *testing.T) {
	t.Parallel()

	const full = "96fd4ef5f1bc2139af33e8b56f35fde372e0be3b"

	tests := []struct {
		name  string
		rev   string
		dirty bool
		want  string
	}{
		{name: "full hash truncated to short", rev: full, dirty: false, want: "96fd4ef"},
		{name: "full hash dirty truncated then suffixed", rev: full, dirty: true, want: "96fd4ef-dirty"},
		{name: "already short kept as-is", rev: "abc1234", dirty: false, want: "abc1234"},
		{name: "shorter than limit kept as-is", rev: "abc", dirty: false, want: "abc"},
		{name: "empty stays empty", rev: "", dirty: false, want: ""},
		{name: "empty dirty stays empty", rev: "", dirty: true, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := shortRevision(tt.rev, tt.dirty); got != tt.want {
				t.Errorf("shortRevision(%q, %v): want %q, got %q", tt.rev, tt.dirty, tt.want, got)
			}
		})
	}
}
