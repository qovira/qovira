package httpx

// MaxBodyBytes is the server-edge request body size ceiling applied by http.MaxBytesHandler in the
// middleware chain. It mirrors the per-operation cap set in internal/api (maxBodyBytes) — both must stay
// in sync. 4 MiB balances typical JSON payloads against server resource protection; individual API
// operations can override downward as their contract demands.
//
// TODO(config): make this configurable via the instance config model (unit 9) if operators need a
// different ceiling for their deployment.
const MaxBodyBytes int64 = 4 * 1024 * 1024
