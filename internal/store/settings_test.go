package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qovira/qovira/internal/store"
)

// TestSettingsStore_SetAndGet verifies that Set persists a value and Get retrieves it correctly.
func TestSettingsStore_SetAndGet(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ss := s.Settings()
	ctx := context.Background()

	if err := ss.Set(ctx, "my.key", "hello"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, found, err := ss.Get(ctx, "my.key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("Get: expected found=true, got false")
	}
	if val != "hello" {
		t.Errorf("Get value = %q, want %q", val, "hello")
	}
}

// TestSettingsStore_GetMissing verifies that Get returns found=false and no error when the key does not exist.
func TestSettingsStore_GetMissing(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ss := s.Settings()
	ctx := context.Background()

	val, found, err := ss.Get(ctx, "nonexistent.key")
	if err != nil {
		t.Fatalf("Get missing key: unexpected error: %v", err)
	}
	if found {
		t.Errorf("Get missing key: expected found=false, got true (value=%q)", val)
	}
	if val != "" {
		t.Errorf("Get missing key: expected empty value, got %q", val)
	}
}

// TestSettingsStore_SetOverwrites verifies that Set on an existing key updates the value (upsert semantics).
func TestSettingsStore_SetOverwrites(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ss := s.Settings()
	ctx := context.Background()

	if err := ss.Set(ctx, "k", "first"); err != nil {
		t.Fatalf("Set first: %v", err)
	}
	if err := ss.Set(ctx, "k", "second"); err != nil {
		t.Fatalf("Set second: %v", err)
	}

	val, found, err := ss.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || val != "second" {
		t.Errorf("Get after overwrite = %q (found=%v), want %q", val, found, "second")
	}
}

// TestSettingsStore_Delete verifies that Delete removes a key and subsequent Get returns found=false.
func TestSettingsStore_Delete(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ss := s.Settings()
	ctx := context.Background()

	if err := ss.Set(ctx, "del.me", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := ss.Delete(ctx, "del.me"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, found, err := ss.Get(ctx, "del.me")
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if found {
		t.Error("Get after Delete: expected found=false, got true")
	}
}

// TestSettingsStore_DeleteMissing verifies that Delete on a non-existent key returns no error (idempotent).
func TestSettingsStore_DeleteMissing(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ss := s.Settings()
	ctx := context.Background()

	if err := ss.Delete(ctx, "no.such.key"); err != nil {
		t.Errorf("Delete missing key: unexpected error: %v", err)
	}
}

// TestSettingsStore_ByPrefix verifies that ByPrefix returns only keys with the given prefix, ordered by key, excluding
// keys outside the prefix.
func TestSettingsStore_ByPrefix(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ss := s.Settings()
	ctx := context.Background()

	// Write a deterministic set of keys.
	for k, v := range map[string]string{
		"alpha.a": "1",
		"alpha.b": "2",
		"beta.a":  "3",
	} {
		if err := ss.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %q: %v", k, err)
		}
	}

	got, err := ss.ByPrefix(ctx, "alpha.")
	if err != nil {
		t.Fatalf("ByPrefix: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ByPrefix returned %d entries, want 2: %v", len(got), got)
	}
	// Results are ordered by key (alpha.a before alpha.b).
	if got[0].Key != "alpha.a" || got[0].Value != "1" {
		t.Errorf("got[0] = %+v, want {Key:alpha.a Value:1}", got[0])
	}
	if got[1].Key != "alpha.b" || got[1].Value != "2" {
		t.Errorf("got[1] = %+v, want {Key:alpha.b Value:2}", got[1])
	}
}

// TestSettingsStore_PersistsAcrossReopen verifies that settings survive a close-and-reopen cycle of the encrypted
// database (AC: settings persist and survive restart).
func TestSettingsStore_PersistsAcrossReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	const encKey = "a-sufficiently-long-passphrase-for-sqlcipher"
	ctx := context.Background()

	// Phase 1: open, migrate, and write a setting.
	{
		s, err := store.Open(store.Config{Path: dbPath, Key: encKey, ReadPoolSize: 1})
		if err != nil {
			t.Fatalf("Open phase 1: %v", err)
		}
		if err := store.NewRunner().Up(ctx, s.Writer()); err != nil {
			t.Fatalf("migrations up: %v", err)
		}
		if err := s.Settings().Set(ctx, "persist.test", "survivor"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close phase 1: %v", err)
		}
	}

	// Phase 2: reopen with the same key and verify the setting is present.
	{
		s, err := store.Open(store.Config{Path: dbPath, Key: encKey, ReadPoolSize: 1})
		if err != nil {
			t.Fatalf("Open phase 2: %v", err)
		}
		defer func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close phase 2: %v", err)
			}
		}()

		val, found, err := s.Settings().Get(ctx, "persist.test")
		if err != nil {
			t.Fatalf("Get after reopen: %v", err)
		}
		if !found {
			t.Fatal("setting not found after close/reopen — must survive restart")
		}
		if val != "survivor" {
			t.Errorf("Get after reopen = %q, want %q", val, "survivor")
		}
	}
}

// TestSettingsStore_Namespace verifies that two SettingsNamespace values with the same logical key name do not collide
// — their values are stored under distinct prefixed storage keys (AC: subsystems can own their own keys without
// colliding).
func TestSettingsStore_Namespace(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ss := s.Settings()
	ctx := context.Background()

	nsA := ss.Namespace("subsystem.a")
	nsB := ss.Namespace("subsystem.b")

	if err := nsA.Set(ctx, "timeout", "30s"); err != nil {
		t.Fatalf("nsA.Set: %v", err)
	}
	if err := nsB.Set(ctx, "timeout", "60s"); err != nil {
		t.Fatalf("nsB.Set: %v", err)
	}

	valA, foundA, err := nsA.Get(ctx, "timeout")
	if err != nil {
		t.Fatalf("nsA.Get: %v", err)
	}
	valB, foundB, err := nsB.Get(ctx, "timeout")
	if err != nil {
		t.Fatalf("nsB.Get: %v", err)
	}

	if !foundA || valA != "30s" {
		t.Errorf("nsA.Get timeout = %q (found=%v), want 30s", valA, foundA)
	}
	if !foundB || valB != "60s" {
		t.Errorf("nsB.Get timeout = %q (found=%v), want 60s", valB, foundB)
	}
}

// TestSettingsStore_NamespaceByPrefix verifies that ByPrefix on a namespace returns only the keys within that
// namespace, with logical (non-prefixed) names exposed to the caller.
func TestSettingsStore_NamespaceByPrefix(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ss := s.Settings()
	ctx := context.Background()

	ns := ss.Namespace("model")

	if err := ns.Set(ctx, "endpoint", "https://api.example.com"); err != nil {
		t.Fatalf("Set endpoint: %v", err)
	}
	if err := ns.Set(ctx, "api_key", "secret"); err != nil {
		t.Fatalf("Set api_key: %v", err)
	}
	// A key outside the namespace must not appear.
	if err := ss.Set(ctx, "other.key", "noise"); err != nil {
		t.Fatalf("Set other.key: %v", err)
	}

	entries, err := ns.ByPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ByPrefix: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ByPrefix returned %d entries, want 2: %v", len(entries), entries)
	}

	keys := make(map[string]string, len(entries))
	for _, e := range entries {
		keys[e.Key] = e.Value
	}
	if keys["endpoint"] != "https://api.example.com" {
		t.Errorf("endpoint = %q, want https://api.example.com", keys["endpoint"])
	}
	if keys["api_key"] != "secret" {
		t.Errorf("api_key = %q, want secret", keys["api_key"])
	}
}

// TestSettingsStore_NoMasterKeyAccess documents and verifies that the SettingsStore carries no mechanism to access or
// persist the master encryption key.  The master key is boot-only config (store.Config.Key) that is never written to
// the database.
//
// By construction the accessor is a plain string KV — it has no path to store.Config.Key.  This test is a contract
// anchor: it asserts the interface carries no MasterKey method and that the master_key is absent from a freshly-opened
// DB.
func TestSettingsStore_NoMasterKeyAccess(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ss := s.Settings()
	ctx := context.Background()

	// The settings store must not implement any hypothetical MasterKeyProvider interface.  This assertion is a
	// compile-time shape check at test time.
	type masterKeyProvider interface{ MasterKey() string }
	if _, ok := any(ss).(masterKeyProvider); ok {
		t.Error(
			"SettingsStore must not implement MasterKey() — the master key must never be accessible through the settings store",
		)
	}

	// The master key must not be present in the DB (it is never written there).
	_, found, err := ss.Get(ctx, "master_key")
	if err != nil {
		t.Fatalf("Get master_key: %v", err)
	}
	if found {
		t.Error("master_key found in settings DB — the master key must never be persisted")
	}
}

// TestSettingsStore_ByPrefixEscapesWildcards verifies that ByPrefix treats LIKE metacharacters in the prefix literally.
// Without escaping, the '_' in "model.api_" would match any single character and leak a sibling key ("model.apiXkey")
// into the result — exactly the cross-key-space bleed that namespacing must prevent.
func TestSettingsStore_ByPrefixEscapesWildcards(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ss := s.Settings()
	ctx := context.Background()

	if err := ss.Set(ctx, "model.api_key", "real"); err != nil {
		t.Fatalf("Set api_key: %v", err)
	}
	if err := ss.Set(ctx, "model.apiXkey", "decoy"); err != nil {
		t.Fatalf("Set apiXkey: %v", err)
	}

	got, err := ss.ByPrefix(ctx, "model.api_")
	if err != nil {
		t.Fatalf("ByPrefix: %v", err)
	}
	if len(got) != 1 || got[0].Key != "model.api_key" {
		t.Errorf(
			"ByPrefix(%q) = %+v, want exactly [model.api_key] (the '_' must be literal, not a wildcard)",
			"model.api_", got)
	}
}

// TestSettingsNamespace_EmptyKey_SetGet verifies that Set and Get agree on the storage key when the logical key is
// empty. Set("", v) must be visible via Get("", …) — the empty key maps to the bare namespace prefix, not a "<prefix>."
// variant (finding 4).
func TestSettingsNamespace_EmptyKey_SetGet(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ns := s.Settings().Namespace("nsp")
	ctx := context.Background()

	if err := ns.Set(ctx, "", "root-value"); err != nil {
		t.Fatalf("Set empty key: %v", err)
	}
	val, found, err := ns.Get(ctx, "")
	if err != nil {
		t.Fatalf("Get empty key: %v", err)
	}
	if !found {
		t.Fatal("Get empty key: expected found=true after Set, got false")
	}
	if val != "root-value" {
		t.Errorf("Get empty key = %q, want %q", val, "root-value")
	}
}

// TestSettingsNamespace_EmptyKey_ByPrefix verifies that ByPrefix("") within a namespace returns a value that was stored
// under the empty logical key (finding 4). The fix must make fullKey("") and ByPrefix("") agree: both must use the bare
// namespace prefix as the storage prefix so a Set("", v) value appears in ByPrefix("").
func TestSettingsNamespace_EmptyKey_ByPrefix(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ns := s.Settings().Namespace("ns2")
	ctx := context.Background()

	if err := ns.Set(ctx, "", "root"); err != nil {
		t.Fatalf("Set empty key: %v", err)
	}
	if err := ns.Set(ctx, "child", "leaf"); err != nil {
		t.Fatalf("Set child: %v", err)
	}

	entries, err := ns.ByPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ByPrefix empty: %v", err)
	}

	found := false
	for _, e := range entries {
		if e.Key == "" && e.Value == "root" {
			found = true
		}
	}
	if !found {
		t.Errorf("ByPrefix(\"\") did not return the empty-key entry; got: %v", entries)
	}
}

// TestSettingsNamespace_EmptyKey_Delete verifies that Delete routes through the same empty-key convention as Set/Get:
// Delete("") must remove the value stored under the bare namespace prefix (finding 4 — all of Set/Get/Delete/ByPrefix
// must agree on the empty logical key).
func TestSettingsNamespace_EmptyKey_Delete(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ns := s.Settings().Namespace("ns3")
	ctx := context.Background()

	if err := ns.Set(ctx, "", "root"); err != nil {
		t.Fatalf("Set empty key: %v", err)
	}
	if err := ns.Delete(ctx, ""); err != nil {
		t.Fatalf("Delete empty key: %v", err)
	}
	_, found, err := ns.Get(ctx, "")
	if err != nil {
		t.Fatalf("Get empty key after delete: %v", err)
	}
	if found {
		t.Error("Get empty key after Delete: expected found=false, got true")
	}
}

// TestSettingsNamespace_ByPrefix_SiblingIsolation verifies that ByPrefix("") on a namespace does not leak entries from
// a sibling namespace whose name merely shares the same string prefix (e.g. "model" vs "model_gateway"). The
// bare-prefix query matches "model_gateway.*" at the storage layer, so ByPrefix must filter to a proper namespace
// boundary ("<prefix>" or "<prefix>.").
func TestSettingsNamespace_ByPrefix_SiblingIsolation(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	model := s.Settings().Namespace("model")
	sibling := s.Settings().Namespace("model_gateway")

	if err := model.Set(ctx, "", "root"); err != nil {
		t.Fatalf("Set model empty key: %v", err)
	}
	if err := model.Set(ctx, "endpoint", "https://m"); err != nil {
		t.Fatalf("Set model.endpoint: %v", err)
	}
	if err := sibling.Set(ctx, "token", "secret"); err != nil {
		t.Fatalf("Set model_gateway.token: %v", err)
	}

	entries, err := model.ByPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ByPrefix empty: %v", err)
	}

	got := make(map[string]string, len(entries))
	for _, e := range entries {
		got[e.Key] = e.Value
		if e.Value == "secret" {
			t.Errorf("ByPrefix(\"\") on \"model\" leaked the sibling \"model_gateway\" entry (key %q); got: %v", e.Key, entries)
		}
	}
	if len(got) != 2 {
		t.Errorf("ByPrefix(\"\") on \"model\" returned %d entries, want 2 (empty key + endpoint); got: %v", len(got), entries)
	}
	if got[""] != "root" || got["endpoint"] != "https://m" {
		t.Errorf("ByPrefix(\"\") on \"model\" = %v, want {\"\":\"root\", \"endpoint\":\"https://m\"}", got)
	}
}

// TestSettingsStore_ScopeGuardExempt verifies that the settings table queries produce no scope-guard violations.  The
// guard must recognise settings as a system-owned table and exempt all four queries from the user_id predicate
// requirement.
func TestSettingsStore_ScopeGuardExempt(t *testing.T) {
	t.Parallel()

	queriesDir := filepath.Join(repoRoot(t), "internal", "store", "queries")
	fsys := os.DirFS(queriesDir)

	violations, err := store.ScanQueryViolations(fsys)
	if err != nil {
		t.Fatalf("ScanQueryViolations: %v", err)
	}

	for _, v := range violations {
		if v.File == "settings.sql" {
			t.Errorf("unexpected scope-guard violation in settings.sql: query=%s", v.QueryName)
		}
	}
}
