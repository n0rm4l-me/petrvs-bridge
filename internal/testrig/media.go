package testrig

import "github.com/n0rm4l-me/petrvs-bridge/internal/telegram"

// SayMedia sends a photo or document as the authorised user and returns the id the chat gave the message. The bytes have
// to be served with r.TG.Serve(ref.ID, data) for the hub to fetch them.
func (r *Rig) SayMedia(ref telegram.FileRef, caption string) int64 {
	return r.SayMediaWith(telegram.Update{Media: &ref, Text: caption})
}

// SayMediaWith is SayMedia for an update of your own making (a reply, an old date, another sender). The fields that are
// zero are filled in as for an ordinary message of the authorised user.
func (r *Rig) SayMediaWith(u telegram.Update) int64 {
	id := nextID()
	u.ID = id
	if u.ChatID == 0 {
		u.ChatID = ChatID
	}
	if u.UserID == 0 {
		u.UserID = UserID
	}
	if u.MessageID == 0 {
		u.MessageID = 100000 + id
	}
	r.TG.Push(u)
	return u.MessageID
}
