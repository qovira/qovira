package events

// export_test.go exposes internal hub state to the external events_test package. It is compiled only under
// `go test`, so these accessors widen the test surface without widening the production API.

// SubscriberCount reports the number of active subscriptions registered on topic. Tests use it to prove
// that a connection's Subscribe/Unsubscribe lifecycle actually mutates the hub — e.g. that a client
// disconnect removes the subscription rather than leaking it.
func (h *Hub) SubscriberCount(topic string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return len(h.topics[topic])
}
