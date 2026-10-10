// Package slack is the chat client of the hub for Slack: the same small interface as the Telegram client (telegram.Client and its
// optional parts), so that the hub, with its ratchet sessions, queues and file transfers, does not know which chat it is in.
//
// It connects with Socket Mode, a WebSocket that the hub opens outward, so the hub still listens on nothing. It talks to one
// person in one direct message and ignores everyone and everything else. It never puts a token, or the one-time address of the
// socket, in an error or a log line.
package slack

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/slack/slackid"
)

// The hub tells chats and users apart by number. Slack's ids are strings, so the client keeps the one chat and the one user that
// are allowed under the fixed numbers below, and gives every other channel and user a number of its own (never these two), so
// that the hub's own check ("this chat and this user, or nobody") stays a second wall behind the client's.
const (
	// ChatID is the number of the allowed direct message, UserID the number of the allowed user.
	ChatID = slackid.ChatID
	UserID = slackid.UserID
)

// otherID gives a stranger (a channel or a user that is not allowed) a number: at least 2, so it never equals ChatID or UserID.
// It is for counting and logging unauthorised messages; nothing ever sends to it.
func otherID(s string) int64 {
	sum := sha256.Sum256([]byte(s))
	return 2 + int64(binary.BigEndian.Uint64(sum[:8])&(1<<62-1))
}

// A message "ts" is the time with microseconds, "1700000000.123456", unique in a channel. As a number (seconds times a million,
// plus the microseconds) it is the message id the hub keeps, and it turns back into the exact string.
var tsRe = regexp.MustCompile(`^(\d{9,11})\.(\d{6})$`)

func tsToID(ts string) (int64, bool) {
	m := tsRe.FindStringSubmatch(ts)
	if m == nil {
		return 0, false
	}
	sec, _ := strconv.ParseInt(m[1], 10, 64)
	us, _ := strconv.ParseInt(m[2], 10, 64)
	return sec*1_000_000 + us, true
}

func idToTS(id int64) (string, bool) {
	if id <= 0 {
		return "", false
	}
	ts := fmt.Sprintf("%d.%06d", id/1_000_000, id%1_000_000)
	_, ok := tsToID(ts)
	return ts, ok
}

// tsTime is the time of a message, the zero time when ts is not one.
func tsTime(ts string) time.Time {
	id, ok := tsToID(ts)
	if !ok {
		return time.Time{}
	}
	return time.Unix(id/1_000_000, (id%1_000_000)*1000).UTC()
}

// What Slack's ids look like. The client checks every id it is given or hears, because they come from the network and from the
// settings and are sent back in requests.
var (
	teamIDRe     = regexp.MustCompile(`^[TE][A-Z0-9]{8,31}$`)
	fileIDRe     = regexp.MustCompile(`^F[A-Z0-9]{8,31}$`)
	anyChannelRe = regexp.MustCompile(`^[CDG][A-Z0-9]{8,31}$`)
	triggerRe    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// The checks of ids and tokens are in slackid, which the settings use without the client.
func ValidUserID(s string) bool    { return slackid.ValidUserID(s) }
func ValidChannelID(s string) bool { return slackid.ValidChannelID(s) }
func ValidBotToken(s string) bool  { return slackid.ValidBotToken(s) }
func ValidAppToken(s string) bool  { return slackid.ValidAppToken(s) }
