// Package room is a room-server analog running on ordinary DMs: users post
// with ".room send", read history with ".room get"/"last", and subscribe to
// new-post delivery with ".room on"/"off". Multiple named rooms are supported:
// the first token after the trigger selects the room unless it is a verb
// (".room secret send hi"), so bare commands always target the default room.
// Posts are stored bot-side with per-room, per-user cursors; subscribers
// receive every new post as a DM, everyone else accumulates unread posts
// until their next ".room get".
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
		HelpShort: "shared rooms: post, read, subscribe",
		// HelpLong must be non-empty for ".room help" to intercept; the
		// localized catalog overrides it via LocalizedHelp.
		HelpLong: ".room - room status (subscribed? unread? last post)\n.room send <text> - post to the room\n.room get [n] - fetch unread posts (default 10)\n.room last [n] - read last posts\n.room on / .room off - subscribe / unsubscribe to new posts\n.room who - who is in the room\n.room list - list rooms\n.room <name> <verb> ... - run the command in another room",
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

	rest := strings.TrimSpace(c.Rest)
	if rest == "" {
		return roomStatus(c.Lang, defaultRoom(), keyHex, c.SenderName)
	}

	// Disambiguation: a verb always owns the first token, so reserved words
	// can never be room names; anything else that resolves to a declared room
	// selects it and the verb follows (".room secret send hi").
	head, args := splitFirst(rest)
	roomName := defaultRoom()
	if !reservedVerbs[head] {
		if !roomExists(head) {
			if isRoomName(head) {
				return []string{tr.T(c.Lang, "err.no_room")}, nil
			}
			return []string{tr.T(c.Lang, "unknown")}, nil
		}
		roomName = head
		if args == "" {
			return roomStatus(c.Lang, roomName, keyHex, c.SenderName)
		}
		head, args = splitFirst(args)
		if !reservedVerbs[head] {
			return []string{tr.T(c.Lang, "unknown")}, nil
		}
	}

	if head == "list" {
		return roomList(c.Lang, keyHex)
	}
	// Closed rooms gate per room: silence toward non-members, like DMs from
	// strangers (isOpenRoom consults the config, no store needed).
	if !isOpenRoom(roomName, keyHex) {
		return nil, nil
	}

	switch head {
	case "status":
		return roomStatus(c.Lang, roomName, keyHex, c.SenderName)
	case "get":
		return roomGet(c.Lang, roomName, keyHex, c.SenderName, args)
	case "send":
		return roomSend(c, roomName, keyHex, args)
	case "on":
		return roomSetSub(c.Lang, roomName, keyHex, c.SenderName, true)
	case "off":
		return roomSetSub(c.Lang, roomName, keyHex, c.SenderName, false)
	case "last":
		return roomLast(c.Lang, roomName, args)
	case "who":
		return roomWho(c.Lang, roomName)
	default:
		return []string{tr.T(c.Lang, "unknown")}, nil
	}
}

// splitFirst cuts the leading word off rest: the lowercased head and the
// trimmed remainder ("" when none).
func splitFirst(rest string) (head, args string) {
	fields := strings.SplitN(rest, " ", 2)
	head = strings.ToLower(fields[0])
	if len(fields) == 2 {
		args = strings.TrimSpace(fields[1])
	}
	return head, args
}

// roomSend appends a post, marks the store current, and pushes to
// subscribers. The author's message timestamp keys the replay guard: a stored
// command redelivered by the device after a restart must not post twice.
func roomSend(c registry.Ctx, room, keyHex, text string) ([]string, error) {
	if text == "" {
		return []string{tr.T(c.Lang, "usage.send")}, nil
	}

	roomMu.Lock()
	defer roomMu.Unlock()
	s, err := loadStore(room)
	if err != nil {
		return []string{tr.T(c.Lang, "err.load")}, err
	}
	if c.PingMeta != nil {
		id := msgID(keyHex, c.PingMeta.Timestamp)
		if s.st.wasSeen(id) {
			return nil, nil // replayed stored command: already posted
		}
		s.st.markSeen(id)
	}

	p := RoomPost{
		Seq:        s.st.NextSeq,
		CreatedAt:  time.Now(),
		AuthorKey:  keyHex,
		AuthorName: c.SenderName,
		Text:       text,
	}
	s.st.NextSeq++
	s.st.Posts = append(s.st.Posts, p)
	u := s.st.user(keyHex, c.SenderName)
	// The cursor is deliberately NOT advanced past the author's own post:
	// posts they missed between their cursor and here must stay unread (their
	// own post is skipped when they next run get).
	if err := s.save(); err != nil {
		return []string{tr.T(c.Lang, "err.save")}, err
	}
	pushPost(c.Lang, room, s, p)
	if room != defaultRoom() {
		return []string{tr.T(c.Lang, "sent.in", p.Seq, room, s.st.unread(u))}, nil
	}
	return []string{tr.T(c.Lang, "sent", p.Seq, s.st.unread(u))}, nil
}

// roomGet returns up to n unread posts (oldest first), advancing the user's
// cursor by what was rendered.
func roomGet(lang, room, keyHex, name, args string) ([]string, error) {
	n := batchArg(args, getBatch())

	roomMu.Lock()
	defer roomMu.Unlock()
	s, err := loadStore(room)
	if err != nil {
		return []string{tr.T(lang, "err.load")}, err
	}
	u := s.st.user(keyHex, name)

	var msgs []string
	for _, p := range s.st.Posts {
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
	if remaining := s.st.unread(u); remaining > 0 {
		msgs = append(msgs, tr.T(lang, "get.more", remaining))
	}
	if err := s.save(); err != nil {
		return msgs, err // posts are delivered; the cursor save failure is logged upstream
	}
	return msgs, nil
}

// roomSetSub toggles subscription in one room and reports pending unread.
func roomSetSub(lang, room, keyHex, name string, on bool) ([]string, error) {
	roomMu.Lock()
	defer roomMu.Unlock()
	s, err := loadStore(room)
	if err != nil {
		return []string{tr.T(lang, "err.load")}, err
	}
	u := s.st.user(keyHex, name)
	u.Subscribed = on
	if err := s.save(); err != nil {
		return []string{tr.T(lang, "err.save")}, err
	}
	if on {
		return []string{tr.T(lang, "sub.on", s.st.unread(u))}, nil
	}
	return []string{tr.T(lang, "sub.off")}, nil
}

// roomStatus reports subscription, unread count and the last post preview.
func roomStatus(lang, room, keyHex, name string) ([]string, error) {
	roomMu.Lock()
	defer roomMu.Unlock()
	s, err := loadStore(room)
	if err != nil {
		return []string{tr.T(lang, "err.load")}, err
	}
	u := s.st.user(keyHex, name)
	if len(s.st.Posts) == 0 {
		return []string{tr.T(lang, "empty")}, nil
	}
	last := s.st.Posts[len(s.st.Posts)-1]
	lines := []string{tr.T(lang, "status", room, subLabel(lang, u.Subscribed), s.st.unread(u), len(s.st.Posts))}
	lines = append(lines, renderPost(lang, last))
	return lines, nil
}

// roomLast renders the last n posts regardless of cursors — history browsing.
func roomLast(lang, room, args string) ([]string, error) {
	n := batchArg(args, getBatch())

	roomMu.Lock()
	defer roomMu.Unlock()
	s, err := loadStore(room)
	if err != nil {
		return []string{tr.T(lang, "err.load")}, err
	}
	if len(s.st.Posts) == 0 {
		return []string{tr.T(lang, "empty")}, nil
	}
	if n > len(s.st.Posts) {
		n = len(s.st.Posts)
	}
	tail := s.st.Posts[len(s.st.Posts)-n:]
	msgs := make([]string, 0, len(tail))
	for _, p := range tail {
		msgs = append(msgs, renderPost(lang, p))
	}
	return msgs, nil
}

// roomWho lists the room's participants, subscribers first.
func roomWho(lang, room string) ([]string, error) {
	roomMu.Lock()
	defer roomMu.Unlock()
	s, err := loadStore(room)
	if err != nil {
		return []string{tr.T(lang, "err.load")}, err
	}
	if len(s.st.Users) == 0 {
		return []string{tr.T(lang, "who.none")}, nil
	}
	keys := make([]string, 0, len(s.st.Users))
	for k := range s.st.Users {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var subs, others []string
	for _, k := range keys {
		u := s.st.Users[k]
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
	if len(subs) == 0 {
		return []string{tr.T(lang, "who.no_subs", len(others), strings.Join(others, ", "))}, nil
	}
	return []string{tr.T(lang, "who", len(subs), strings.Join(subs, ", "), len(others), strings.Join(others, ", "))}, nil
}

// roomList shows the declared rooms the caller may access, with their
// subscription state and unread count. Closed rooms stay invisible to
// non-members — the list must not advertise what they can't join.
// Listing never enrolls the caller in a room they haven't used.
func roomList(lang, keyHex string) ([]string, error) {
	roomMu.Lock()
	defer roomMu.Unlock()

	lines := make([]string, 0, len(conf.Rooms)+1)
	lines = append(lines, tr.T(lang, "list.header"))
	for _, name := range roomNames() {
		if !isOpenRoom(name, keyHex) {
			continue
		}
		state := tr.T(lang, "sub.off_short")
		unreadN := 0
		if s, err := loadStore(name); err == nil {
			if u, ok := s.st.Users[keyHex]; ok {
				if u.Subscribed {
					state = tr.T(lang, "sub.on_short")
				}
				unreadN = s.st.unread(u)
			} else {
				// Never touched the room: every post so far is unseen.
				unreadN = s.st.NextSeq - 1
			}
		} else {
			return []string{tr.T(lang, "err.load")}, err
		}
		lines = append(lines, tr.T(lang, "list.line", name, state, unreadN))
	}
	return lines, nil
}

// batchArg parses an optional "[n]" argument: the first integer field wins,
// garbage or absence falls back to def.
func batchArg(args string, def int) int {
	if fields := strings.Fields(args); len(fields) >= 1 {
		if v, err := strconv.Atoi(fields[0]); err == nil && v > 0 {
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
