package store

// Package store — settings accessor.
//
// SettingsStore is the typed accessor for the settings table, which holds instance-global operational configuration
// (model endpoints, per-role overrides, SMTP settings, etc.).  It is obtained via (*Store).Settings().
//
// Security note: the master encryption key (store.Config.Key) is boot-only configuration and is NEVER stored in the
// database.  SettingsStore has no path to that value — it is a plain string KV over the application-level settings
// table, which is entirely separate from the SQLCipher key material.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/qovira/qovira/internal/store/db"
)

// likeEscaper escapes the LIKE metacharacters ('\', '%', '_') so a value can be matched literally by a
// "LIKE … ESCAPE '\'" predicate. The backslash itself must be escaped first.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// SettingEntry is a single setting row returned by ByPrefix.
type SettingEntry struct {
	Key   string
	Value string
}

// SettingsStore provides typed read/write access to the settings table. Reads use the store's read pool; writes use
// the write pool — consistent with the existing ScopedQueries pattern.
//
// Obtain a *SettingsStore via (*Store).Settings().
type SettingsStore struct {
	readQ  *db.Queries
	writeQ *db.Queries
}

// Settings returns a *SettingsStore backed by the store's connection pools. The returned value is cheap to create —
// it wraps two existing *db.Queries values and holds no state of its own.
func (s *Store) Settings() *SettingsStore {
	return &SettingsStore{
		readQ:  db.New(s.readDB),
		writeQ: db.New(s.writeDB),
	}
}

// Get retrieves the value for key.  It returns (value, true, nil) when the key exists and ("", false, nil) when it
// does not.  A non-nil error indicates a real database failure.
func (ss *SettingsStore) Get(ctx context.Context, key string) (string, bool, error) {
	row, err := ss.readQ.GetSetting(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: settings.Get %q: %w", key, err)
	}
	return row.Value, true, nil
}

// Set upserts the value for key.  If the key already exists its value and updated_at timestamp are replaced atomically.
func (ss *SettingsStore) Set(ctx context.Context, key, value string) error {
	if err := ss.writeQ.UpsertSetting(ctx, db.UpsertSettingParams{
		SettingKey: key,
		Value:      value,
	}); err != nil {
		return fmt.Errorf("store: settings.Set %q: %w", key, err)
	}
	return nil
}

// Delete removes the row for key.  If the key does not exist the call is a no-op and nil is returned.
func (ss *SettingsStore) Delete(ctx context.Context, key string) error {
	if err := ss.writeQ.DeleteSetting(ctx, key); err != nil {
		return fmt.Errorf("store: settings.Delete %q: %w", key, err)
	}
	return nil
}

// ByPrefix returns all settings whose key starts with prefix, ordered by key. An empty prefix returns all settings.
// The returned slice is nil (not empty) when no rows match. The prefix is matched literally: any LIKE metacharacters
// ('%', '_') it contains are escaped, so a prefix such as "model.api_" matches only that literal stem and never bleeds
// into a sibling key space.
func (ss *SettingsStore) ByPrefix(ctx context.Context, prefix string) ([]SettingEntry, error) {
	rows, err := ss.readQ.ListSettingsByPrefix(ctx, sql.NullString{
		String: likeEscaper.Replace(prefix),
		Valid:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("store: settings.ByPrefix %q: %w", prefix, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	entries := make([]SettingEntry, len(rows))
	for i, r := range rows {
		entries[i] = SettingEntry{Key: r.SettingKey, Value: r.Value}
	}
	return entries, nil
}

// Namespace returns a *SettingsNamespace whose Get/Set/Delete/ByPrefix methods transparently prepend "<prefix>." to
// every key, so that independent subsystems can each own their own key space without colliding.
//
// Example:
//
//	ns := store.Settings().Namespace("model")
//	ns.Set(ctx, "endpoint", "https://...")  // stored as "model.endpoint"
//	ns.Get(ctx, "endpoint")                 // reads "model.endpoint"
func (ss *SettingsStore) Namespace(prefix string) *SettingsNamespace {
	return &SettingsNamespace{parent: ss, prefix: prefix}
}

// SettingsNamespace wraps a *SettingsStore and transparently prefixes every key with "<prefix>.".  Callers interact
// with logical (unprefixed) key names; the prefix is an implementation detail.
type SettingsNamespace struct {
	parent *SettingsStore
	prefix string
}

// fullKey returns the storage key for a logical key within this namespace.
func (ns *SettingsNamespace) fullKey(logicalKey string) string {
	if logicalKey == "" {
		return ns.prefix
	}
	return ns.prefix + "." + logicalKey
}

// Get retrieves the value for logicalKey within this namespace.
func (ns *SettingsNamespace) Get(ctx context.Context, logicalKey string) (string, bool, error) {
	return ns.parent.Get(ctx, ns.fullKey(logicalKey))
}

// Set upserts the value for logicalKey within this namespace.
func (ns *SettingsNamespace) Set(ctx context.Context, logicalKey, value string) error {
	return ns.parent.Set(ctx, ns.fullKey(logicalKey), value)
}

// Delete removes logicalKey from this namespace.
func (ns *SettingsNamespace) Delete(ctx context.Context, logicalKey string) error {
	return ns.parent.Delete(ctx, ns.fullKey(logicalKey))
}

// ByPrefix returns all settings within this namespace whose logical key starts with subPrefix.  An empty subPrefix
// returns all settings in the namespace, including any value stored under the empty logical key itself.
//
// The returned SettingEntry.Key values are the logical (non-namespaced) key names; the namespace prefix is stripped.
//
// Convention for the empty logical key: fullKey("") returns the bare namespace prefix (no trailing dot), so a value
// written via Set(ctx, "", v) is stored under the storage key "<prefix>".  ByPrefix("") therefore queries with
// storagePrefix = "<prefix>" (matching both "<prefix>" itself and any "<prefix>.child" keys), which is consistent with
// how fullKey resolves the empty key.
//
// Because the underlying query matches on a raw string prefix, the bare-prefix query for the empty subPrefix can also
// match a SIBLING namespace whose name shares that string prefix (e.g. prefix "model" matching "model_gateway.*").
// Returned rows are therefore filtered to a proper namespace boundary — a key is kept only if it is exactly "<prefix>"
// (the empty logical key) or begins with "<prefix>." — so sibling namespaces never leak across.
func (ns *SettingsNamespace) ByPrefix(ctx context.Context, subPrefix string) ([]SettingEntry, error) {
	var storagePrefix string
	if subPrefix == "" {
		// Empty subPrefix: match the bare namespace prefix so the empty-logical-key entry ("<prefix>")
		// is included alongside all "<prefix>.child" entries.
		storagePrefix = ns.prefix
	} else {
		storagePrefix = ns.prefix + "." + subPrefix
	}
	rows, err := ns.parent.ByPrefix(ctx, storagePrefix)
	if err != nil {
		return nil, err
	}
	// Strip the namespace prefix from returned keys so callers see only the logical key name, and drop any sibling
	// namespace that merely shares the string prefix. The empty logical key is stored as exactly "<prefix>"; every
	// other in-namespace key begins with "<prefix>.".
	dot := ns.prefix + "."
	filtered := rows[:0]
	for _, r := range rows {
		switch {
		case r.Key == ns.prefix:
			r.Key = ""
			filtered = append(filtered, r)
		case strings.HasPrefix(r.Key, dot):
			r.Key = strings.TrimPrefix(r.Key, dot)
			filtered = append(filtered, r)
		default:
			// Sibling namespace sharing the string prefix (e.g. "model_gateway" for prefix "model") — exclude.
		}
	}
	return filtered, nil
}
