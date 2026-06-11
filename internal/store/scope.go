package store

import "errors"

// Principal is the authenticated identity resolved by the auth middleware.
// It is a plain carrier type; the auth middleware will populate it before
// passing control to the handler. Only its fields are consumed here.
type Principal struct {
	UserID string
	Role   string
}

// Scope is the sole source of user identity for data access. Its fields are
// unexported so callers cannot construct a Scope with an arbitrary user ID —
// they must go through UserScope or SystemScope.
//
// A Scope is either a user scope (IsSystem() == false, UserID() returns the
// authenticated user's ID) or a system scope (IsSystem() == true, used for
// queries against system-owned tables that carry no user_id column).
type Scope struct {
	userID string
	system bool
}

// UserScope returns a Scope bound to the given Principal's UserID. The
// caller must supply a Principal whose UserID is non-empty; a Scope with an
// empty userID is invalid and will be rejected by ScopedQueries methods before
// any query is executed.
func UserScope(p Principal) Scope {
	return Scope{userID: p.UserID}
}

// SystemScope returns a Scope that grants access to system-owned tables (those
// with no user_id column, e.g. instance). A system scope may NOT be used to
// call user-scoped query methods — those methods check and return an error.
func SystemScope() Scope {
	return Scope{system: true}
}

// UserID returns the bound user ID. It is empty for a system scope.
func (s Scope) UserID() string { return s.userID }

// IsSystem reports whether this is a system scope.
func (s Scope) IsSystem() bool { return s.system }

// ErrSystemScope is returned when a user-scoped method is called with a system scope.
var ErrSystemScope = errors.New("store: user-scoped method called with a system scope")

// ErrEmptyUserID is returned when a user scope carries an empty user ID.
var ErrEmptyUserID = errors.New("store: user scope has an empty user ID")
