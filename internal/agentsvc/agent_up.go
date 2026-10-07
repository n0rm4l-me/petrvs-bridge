package agentsvc

import (
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// Files from the machine to the chat: the agent's side is the spool (parts and the closing item go out like replies) and
// holding them back while the machine is locked. The rest of the work is in `agent send-file` and on the hub.

// holdInSpool says that a spooled message must stay where it is for now.
//
// Files are not sent while the machine is locked: a lockdown means the chat may be in the wrong hands, and a file leaves
// the machine with no one looking at it. Replies, notices and alerts still go. The held parts stay in the spool, so the
// transfer goes on in its order once `agent unlock` lifts the lock.
//
// The closing item of a file is also kept back while a part of its transfer has no receipt yet. The parts come before it in
// the spool, but a bus does not keep order, and the hub cannot put a file together before its last part is in; sending the
// closing item only after the parts were confirmed saves a resend after a whole receipt timeout. (Only its own parts
// count: with the pace of the turns a pass comes a few times a second, and a closing item that waited for every other
// message as well would take a pass for each file.) An entry older than the receipt timeout is not waited for: it is sent
// again in this very pass, or its file is gone.
func (a *Agent) holdInSpool(it spool.Item) bool {
	if it.Type != wire.TypePart && it.Type != wire.TypeFile {
		return false
	}
	if a.Locked() {
		return true
	}
	if it.Type != wire.TypeFile {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.cfg.Now()
	transfer := it.Meta[wire.MetaTransfer]
	for _, w := range a.awaiting {
		if w.transfer == transfer && now.Sub(w.sentAt) < a.cfg.ReceiptTimeout {
			return true
		}
	}
	return false
}
