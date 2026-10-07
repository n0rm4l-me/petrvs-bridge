package hubsvc

import "time"

// SetFileAckWait changes how long the hub waits for the agent's answer to a part of a file; for tests of the external
// package, which cannot wait ten seconds.
func SetFileAckWait(h *Hub, d time.Duration) {
	h.down.mu.Lock()
	h.down.ackWait = d
	h.down.mu.Unlock()
}
