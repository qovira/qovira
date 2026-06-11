package store

import "path/filepath"

// dbFilename is the name of the SQLCipher database file within DataDir.
const dbFilename = "qovira.db"

// DBPath derives the filesystem path to the SQLCipher database file from dataDir. This is the single authoritative location for that derivation; callers (serve, migrate) import this rather than duplicating the join.
func DBPath(dataDir string) string {
	return filepath.Join(dataDir, dbFilename)
}
