// Package room is a room-server analog running on ordinary DMs: users post
// with ".room send", read history with ".room get"/"last", and subscribe to
// new-post delivery with ".room on"/"off". Posts are stored bot-side with a
// per-user cursor; ".room on" subscribers receive every new post as a DM,
// everyone else accumulates unread posts until their next ".room get".
package room

import (
	"embed"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stempit/meshcore-go-bot/internal/support/helpers"
	"github.com/stempit/meshcore-go-bot/internal/support/i18n"
	"github.com/stempit/meshcore-go-bot/internal/support/registry"
)

//go:embed lang/*.yaml
var langFS embed.FS

var tr = i18n.MustLoad(langFS)

// nameClipRunes caps display names in rendered posts (mesh budget).
const nameClipRunes = 10

func init() { registry.Register(plugin{}) }

type plugin struct{}

func (plugin) Meta() registry.Meta {
	return registry.Meta{
		Name:      "room",
		Triggers:  []string{".room"},
		Match:     registry.Prefix,
		HelpShort: "shared room: post, read, subscribe",
		// HelpLong must be non-empty for ".room help" to intercept; the
		// localized catalog overrides it via LocalizedHelp.
		HelpLong: ".room - room status (subscribed? unread? last post)\n.room send <text> - post to the room\n.room get [n] - fetch unread posts (default 10)\n.room last [n] - read last posts\n.room on / .room off - subscribe / unsubscribe to new posts\n.room who - who is in the room",
		Group:    "Service",
	}
}

// LocalizedHelp returns the per-language long help for ".room help".
func (plugin) LocalizedHelp(lang string) string { return tr.T(lang, "help.long") }

func (plugin) Handle(c registry.Ctx) ([]string, error) {
	// The room is a DM service: channel posts have no per-user identity for
	// cursors and would turn the sender's channel into a push target.
	if !c.IsDM() {
		return []string{tr.T(c.Lang, "need_dm")}, nil
	}
	if c.SenderKey == "" {
		// Sessions and cursors key on SenderKey; "" would collapse every
		// unresolved sender into one participant (bbs need_key precedent).
		return []string{tr.T(c.Lang, "need_key")}, nil
	}
	keyHex := keyOf(c.SenderKey)
	if !isOpen(keyHex) {
		return nil, nil // closed room: stay silent toward non-members
	}

	rest := strings.TrimSpace(c.Rest)
	lower := strings.ToLower(rest)
	// Sub-verb matching follows the registry prefix rule: exact, or the verb
	// followed by a space — so ".room getter" is not parsed as ".room get ter".
	switch {
	case lower == "" || lower == "status":
		return roomStatus(c.Lang, keyHex, c.SenderName), nil
	case lower == "get" || strings.HasPrefix(lower, "get "):
		return roomGet(c.Lang, keyHex, c.SenderName, rest)
	case lower == "send" || strings.HasPrefix(lower, "send "):
		return roomSend(c, keyHex, rest)
	case lower == "on":
		return roomSetSub(c.Lang, keyHex, c.SenderName, true), nil
	case lower == "off":
		return roomSetSub(c.Lang, keyHex, c.SenderName, false), nil
	case lower == "last" || strings.HasPrefix(lower, "last "):
		return roomLast(c.Lang, rest)
	case lower == "who":
		return roomWho(c.Lang), nil
	default:
		return []string{tr.T(c.Lang, "unknown")}, nil
	}
}

// roomSend appends a post, marks the author current, and pushes to
// subscribers. The author's message timestamp keys the replay guard: a stored
// command redelivered by the device after a restart must not post twice.
func roomSend(c registry.Ctx, keyHex, rest string) ([]string, error) {
	text := strings.TrimSpace(rest[len("send"):])
	if text == "" {
		return []string{tr.T(c.Lang, "usage.send")}, nil
	}

	roomMu.Lock()
	defer roomMu.Unlock()
	if err := roomLoad(); err != nil {
		return []string{tr.T(c.Lang, "err.load")}, err
	}
	if c.PingMeta != nil {
		id := msgID(keyHex, c.PingMeta.Timestamp)
		if wasSeen(id) {
			return nil, nil // replayed stored command: already posted
		}
		markSeen(id)
	}

	p := RoomPost{
		Seq:        state.NextSeq,
		CreatedAt:  time.Now(),
		AuthorKey:  keyHex,
		AuthorName: c.SenderName,
		Text:       text,
	}
	state.NextSeq++
	state.Posts = append(state.Posts, p)
	u := user(keyHex, c.SenderName)
	// The cursor is deliberately NOT advanced past the author's own post:
	// posts they missed between their cursor and here must stay unread (their
	// own post is skipped when they next run get).
	if err := roomSave(); err != nil {
		return []string{tr.T(c.Lang, "err.save")}, err
	}
	pushPost(c.Lang, p)
	return []string{tr.T(c.Lang, "sent", p.Seq, unread(u))}, nil
}

// roomGet returns up to n unread posts (oldest first), advancing the user's
// cursor by what was rendered.
func roomGet(lang, keyHex, name, rest string) ([]string, error) {
	n := batchArg(rest, getBatch())

	roomMu.Lock()
	defer roomMu.Unlock()
	if err := roomLoad(); err != nil {
		return []string{tr.T(lang, "err.load")}, err
	}
	u := user(keyHex, name)

	var msgs []string
	for _, p := range state.Posts {
		if len(msgs) >= n {
			break
		}
		if p.Seq <= u.Cursor {
			continue
		}
		if p.AuthorKey == keyHex {
			// The author knows their own post; skip it but still consume it so
			// the cursor moves past it.
			u.Cursor = p.Seq
			continue
		}
		msgs = append(msgs, renderPost(lang, p))
		u.Cursor = p.Seq
	}
	if len(msgs) == 0 {
		return []string{tr.T(lang, "get.none")}, nil
	}
	if remaining := unread(u); remaining > 0 {
		msgs = append(msgs, tr.T(lang, "get.more", remaining))
	}
	if err := roomSave(); err != nil {
		return msgs, err // posts are delivered; the cursor save failure is logged upstream
	}
	return msgs, nil
}

// roomSetSub toggles subscription and reports the pending unread count.
func roomSetSub(lang, keyHex, name string, on bool) []string {
	roomMu.Lock()
	defer roomMu.Unlock()
	if err := roomLoad(); err != nil {
		return []string{tr.T(lang, "err.load")}
	}
	u := user(keyHex, name)
	u.Subscribed = on
	if err := roomSave(); err != nil {
		return []string{tr.T(lang, "err.save")}
	}
	if on {
		return []string{tr.T(lang, "sub.on", unread(u))}
	}
	return []string{tr.T(lang, "sub.off")}
}

// roomStatus reports subscription, unread count and the last post preview.
func roomStatus(lang, keyHex, name string) []string {
	roomMu.Lock()
	defer roomMu.Unlock()
	if err := roomLoad(); err != nil {
		return []string{tr.T(lang, "err.load")}
	}
	u := user(keyHex, name)
	if len(state.Posts) == 0 {
		return []string{tr.T(lang, "empty")}
	}
	last := state.Posts[len(state.Posts)-1]
	lines := []string{tr.T(lang, "status", subLabel(lang, u.Subscribed), unread(u), len(state.Posts))}
	lines = append(lines, renderPost(lang, last))
	return lines
}

// roomLast renders the last n posts regardless of cursors — history browsing.
func roomLast(lang, rest string) ([]string, error) {
	n := batchArg(rest, getBatch())

	roomMu.Lock()
	defer roomMu.Unlock()
	if err := roomLoad(); err != nil {
		return []string{tr.T(lang, "err.load")}, err
	}
	if len(state.Posts) == 0 {
		return []string{tr.T(lang, "empty")}, nil
	}
	if n > len(state.Posts) {
		n = len(state.Posts)
	}
	tail := state.Posts[len(state.Posts)-n:]
	msgs := make([]string, 0, len(tail))
	for _, p := range tail {
		msgs = append(msgs, renderPost(lang, p))
	}
	return msgs, nil
}

// roomWho lists the participants, subscribers first.
func roomWho(lang string) []string {
	roomMu.Lock()
	defer roomMu.Unlock()
	if err := roomLoad(); err != nil {
		return []string{tr.T(lang, "err.load")}
	}
	if len(state.Users) == 0 {
		return []string{tr.T(lang, "who.none")}
	}
	keys := make([]string, 0, len(state.Users))
	for k := range state.Users {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var subs, others []string
	for _, k := range keys {
		u := state.Users[k]
		label := u.Name
		if label == "" {
			label = helpers.ClipRunes(u.Key, 6)
		}
		if u.Subscribed {
			subs = append(subs, label)
		} else {
			others = append(others, label)
		}
	}
	if len(subs)+len(others) == 0 {
		return []string{tr.T(lang, "who.none")}
	}
	if len(subs) == 0 {
		return []string{tr.T(lang, "who.no_subs", len(others), strings.Join(others, ", "))}
	}
	return []string{tr.T(lang, "who", len(subs), strings.Join(subs, ", "), len(others), strings.Join(others, ", "))}
}

// batchArg parses an optional "[n]" argument: the first integer field wins,
// garbage or absence falls back to def.
func batchArg(rest string, def int) int {
	if fields := strings.Fields(rest); len(fields) >= 2 {
		if v, err := strconv.Atoi(fields[1]); err == nil && v > 0 {
			def = v
		}
	}
	if def > maxBatch() {
		def = maxBatch()
	}
	return def
}

// subLabel renders the on/off subscription state.
func subLabel(lang string, on bool) string {
	if on {
		return tr.T(lang, "sub.on_short")
	}
	return tr.T(lang, "sub.off_short")
}

// timeAgo renders a coarse relative age for post lines.
func timeAgo(lang string, t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return tr.T(lang, "ago.now")
	case d < time.Hour:
		return tr.T(lang, "ago.min", int(d.Minutes()))
	case d < 24*time.Hour:
		return tr.T(lang, "ago.hour", int(d.Hours()))
	default:
		return tr.T(lang, "ago.day", int(d.Hours()/24))
	}
}
