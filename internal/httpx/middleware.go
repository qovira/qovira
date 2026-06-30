package httpx

// MaxBodyBytes is the single server-wide request body size ceiling. It is enforced at the server edge by
// http.MaxBytesHandler in the middleware chain and is the source of truth that internal/api's per-operation
// cap (maxBodyBytes) references, so the edge backstop and the per-operation cap can never drift. 4 MiB
// balances typical JSON payloads against server resource protection; individual API operations can override
// downward as their contract demands.
//
// TODO(config): make this configurable via the instance config model (unit 9) if operators need a
// different ceiling for their deployment.
const MaxBodyBytes int64 = 4 * 1024 * 1024
