package hubsvc

// fetchLimitNote is what the chat itself limits for a file the user sends: Telegram lets a bot download at most 20 MB; the
// other chats the hub knows leave the limit to the hub's own setting.
func (h *Hub) fetchLimitNote() string {
	if h.cfg.ChatName == "Telegram" {
		return ", and Telegram lets a bot download at most 20 MB"
	}
	return ""
}
