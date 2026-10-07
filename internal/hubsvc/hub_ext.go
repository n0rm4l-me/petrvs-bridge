package hubsvc

import (
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// This file holds what the parts beyond relaying text share: the composed menu and the test of which payloads from an
// agent are confirmed with a receipt. The parts themselves attach at the seams in hub_up.go (files to the chat),
// hub_down.go (files from the chat), hub_ui.go (buttons, screen, chat commands) and hub_mach.go (machine commands,
// status card), each of which replaces the bodies of its seams and keeps its state in its own type.

// fullMenu is the menu the bot publishes.
func fullMenu() []telegram.Command {
	return append(append(append([]telegram.Command(nil), menu...), chatMenu()...), machineMenu()...)
}

// initExt prepares the state of the parts; it runs at the end of New.
func (h *Hub) initExt() {
	h.up.init(h)
	h.down.init(h)
	h.ui.init(h)
	h.mach.init(h)
}

// spooledType says which payloads from an agent are confirmed with a receipt once the hub has dealt with them.
func spooledType(t wire.Type) bool {
	switch t {
	case wire.TypeReply, wire.TypeNotice, wire.TypeAlert, wire.TypePart, wire.TypeFile:
		return true
	}
	return false
}
