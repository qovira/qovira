package events

// Event is the envelope carried across the hub to every subscriber on a topic. Type is a dotted, namespaced
// identifier (e.g. "system.ready", "reminder.fired") that the SSE layer will later emit in the event:
// field of an SSE frame. Data is the payload; it will be JSON-marshaled into the data: field of that frame
// by a future issue — the hub itself is agnostic to its shape.
type Event struct {
	// Type is the event's namespaced kind identifier, e.g. "system.ready" or "reminder.fired".
	Type string

	// Data is the event's payload. It will be JSON-marshaled by the SSE transport layer (a future issue);
	// the hub treats it as opaque.
	Data any
}
