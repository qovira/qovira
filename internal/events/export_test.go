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

// ConnStart is a test-only shim that forwards to the unexported connStart so that external tests in
// package events_test can exercise the WaitGroup lifecycle without the method appearing in the production API.
// It returns true iff the connection was admitted (connStart returned connAdmitted).
func (h *Hub) ConnStart() bool { return h.connStart() == connAdmitted }

// SetMaxConns sets h.maxConns under h.mu. Test-only: allows tests to drive the connection cap without
// opening 256 real connections.
func (h *Hub) SetMaxConns(n int) {
	h.mu.Lock()
	h.maxConns = n
	h.mu.Unlock()
}

// ConnDone is a test-only shim that forwards to the unexported connDone so that external tests in
// package events_test can exercise the WaitGroup lifecycle without the method appearing in the production API.
func (h *Hub) ConnDone() { h.connDone() }
