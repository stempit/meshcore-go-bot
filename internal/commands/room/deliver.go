package room

import (
	"github.com/stempit/meshcore-go"
	"github.com/stempit/meshcore-go-bot/internal/logging"
	"github.com/stempit/meshcore-go-bot/internal/shrink"
	"github.com/stempit/meshcore-go-bot/internal/support/helpers"
)

var log = logging.For("commands")

// The room pushes posts to subscribers over ordinary DMs. A plugin cannot
// reach the dispatcher directly, so the composition root injects a contact
// lookup and a sender (the migrator/games injection pattern). Defaults are
// no-ops so tests run unwired.
var (
	lookupContact = func(rawKey string) *meshcore.Contact { return nil }
	sendDM        = func(contact *meshcore.Contact, text string, ack bool) {}
)

// SetDelivery wires the push pipeline. Called once at startup, before the
// dispatcher runs; nil functions are ignored so the no-ops stay in place.
func SetDelivery(lookup func(rawKey string) *meshcore.Contact,
	send func(contact *meshcore.Contact, text string, ack bool)) {
	if lookup != nil {
		lookupContact = lookup
	}
	if send != nil {
		sendDM = send
	}
}

// renderPost formats one post as a single logical wire message (one or more
// physical parts after the shrink split).
func renderPost(lang string, p RoomPost) string {
	return tr.T(lang, "post.line", p.Seq, authorLabel(p), timeAgo(lang, p.CreatedAt), p.Text)
}

// authorLabel returns the display name, falling back to the first 6 hex chars
// of the key (bbs bbsAuthor style).
func authorLabel(p RoomPost) string {
	if p.AuthorName != "" {
		return helpers.ClipRunes(p.AuthorName, nameClipRunes)
	}
	if len(p.AuthorKey) >= 6 {
		return p.AuthorKey[:6]
	}
	return p.AuthorKey
}

// pushPost delivers a post to every subscriber except its author. Pushes from
// non-default rooms carry a "[name] " prefix — unsolicited DMs from several
// rooms are indistinguishable otherwise. Delivery is best-effort: a subscriber
// whose contact the device no longer knows is skipped with their cursor left
// untouched, so ".room get" delivers the post to them later; drops on a full
// outgoing queue are logged by the send layer.
// Caller must hold roomMu (cursor updates mutate the store).
func pushPost(lang, room string, s *store, p RoomPost) {
	rendered := renderPost(lang, p)
	if room != defaultRoom() {
		rendered = "[" + room + "] " + rendered
	}
	for _, u := range s.st.Users {
		if !u.Subscribed || u.Key == p.AuthorKey {
			continue
		}
		contact := lookupContact(rawKeyOf(u.Key))
		if contact == nil {
			log().Warn("room: push skipped, contact not found", "room", room, "key", u.Key, "seq", p.Seq)
			continue
		}
		for _, part := range shrink.Fit("room", "", []string{rendered}) {
			sendDM(contact, part, true)
		}
		u.Cursor = p.Seq
	}
}
