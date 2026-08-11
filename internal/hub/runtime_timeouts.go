package hub

import "time"

func (h *Hub) effectiveDeveloperContextTimeout() time.Duration {
	if h.developerContextTimeout > 0 {
		return h.developerContextTimeout
	}
	return 30 * time.Second
}
