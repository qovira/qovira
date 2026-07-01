package events

// Event is the envelope carried across the hub to every subscriber on a topic. The SSE handler emits Type in
// the "event:" field of a frame and JSON-marshals Data into its "data:" field; the hub is agnostic to Data's
// shape.
type Event struct {
	// Type is the event's dotted, namespaced kind, e.g. "system.ready" or "reminder.fired".
	Type string

	// Data is the event's payload, treated as opaque by the hub.
	Data any
}
