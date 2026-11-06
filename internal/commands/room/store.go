package room

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"
	"time"
)

// The room state lives under log/room; vars (not consts) so tests can redirect
// them.
var (
	roomDir     = "log/room"
	roomFile    = "log/room/room.json"
	seenRingCap = 64 // replay-guard memory: recent "key-timestamp" ids
)

// RoomPost is one room message.
type RoomPost struct {
	Seq        int       `json:"seq"`
	CreatedAt  time.Time `json:"created_at"`
	AuthorKey  string    `json:"author_key"` // lowercase-hex pubkey
	AuthorName string    `json:"author_name"`
	Text       string    `json:"text"`
}

// RoomUser is a room participant. Cursor is the last post Seq the user has
// been delivered (push or explicit get); unread = NextSeq-1-Cursor.
type RoomUser struct {
	Key        string `json:"key"` // lowercase-hex pubkey
	Name       string `json:"name"`
	Subscribed bool   `json:"subscribed"`
	Cursor     int    `json:"cursor"`
}

// roomState is the persisted store: the post history ring, per-user cursors,
// and the replay-guard ring. The ring layout follows the bbs store (atomic
// save + .bak generation), but the room history is append-only — prune drops
// the oldest posts, cursors pointing into pruned range stay valid (they only
// ever count).
type roomState struct {
	Posts   []RoomPost           `json:"posts"`
	Users   map[string]*RoomUser `json:"users"`
	NextSeq int                  `json:"next_seq"`
	Seen    []string             `json:"seen"` // replay guard: recent msg ids
}

var (
	roomMu     sync.Mutex
	state      *roomState
	roomLoaded bool
)

// keyOf normalizes the DM SenderKey flavor (raw public-key bytes) into the
// canonical lowercase-hex identity the store is keyed by. The room is DM-only,
// so the raw-byte path is the only one that reaches it.
func keyOf(raw string) string {
	return hex.EncodeToString([]byte(raw))
}

// rawKeyOf recovers the raw public-key bytes a contact lookup needs.
func rawKeyOf(keyHex string) string {
	raw, err := hex.DecodeString(keyHex)
	if err != nil {
		return ""
	}
	return string(raw)
}

// roomLoad loads the store from disk if not already loaded. Caller must hold
// roomMu.
func roomLoad() error {
	if roomLoaded {
		return nil
	}
	if err := os.MkdirAll(roomDir, 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(roomFile)
	if errors.Is(err, os.ErrNotExist) {
		// Either a fresh install or the atomic-save rename window: fall back
		// to the previous generation kept by roomSave.
		data, err = os.ReadFile(roomFile + ".bak")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) == 0 {
		state = newState()
		roomLoaded = true
		return nil
	}
	st := newState()
	if err := json.Unmarshal(data, st); err != nil {
		// The main file is corrupt; try the previous generation before
		// giving up (roomSave keeps one around).
		bak, bakErr := os.ReadFile(roomFile + ".bak")
		if bakErr != nil {
			return err
		}
		if bakErr := json.Unmarshal(bak, st); bakErr != nil {
			return err // both generations unreadable
		}
	}
	if st.Users == nil {
		st.Users = map[string]*RoomUser{}
	}
	if st.NextSeq < 1 {
		st.NextSeq = 1
	}
	state = st
	roomLoaded = true
	return nil
}

// newState returns an empty store with the sequence watermark at 1.
func newState() *roomState {
	return &roomState{Users: map[string]*RoomUser{}, NextSeq: 1}
}

// roomSave serialises the store to disk. Caller must hold roomMu. The write
// is atomic (temp file + rename) with the previous generation kept as .bak —
// same scheme as the bbs store: these nodes run unattended and a crash
// mid-write must not lose the room.
func roomSave() error {
	prunePosts()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(roomDir, 0o755); err != nil {
		return err
	}
	tmp := roomFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(roomFile, roomFile+".bak"); err != nil && !errors.Is(err, os.ErrNotExist) {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, roomFile)
}

// prunePosts caps history growth: beyond the configured max the oldest posts
// are dropped. Caller holds roomMu.
func prunePosts() {
	max := maxPosts()
	if len(state.Posts) <= max {
		return
	}
	state.Posts = state.Posts[len(state.Posts)-max:]
}

// user returns the participant record for keyHex, creating (and naming) one on
// first contact. Caller holds roomMu.
func user(keyHex, name string) *RoomUser {
	u, ok := state.Users[keyHex]
	if !ok {
		u = &RoomUser{Key: keyHex}
		state.Users[keyHex] = u
	}
	if name != "" {
		u.Name = name
	}
	return u
}

// unread counts the user's unseen posts. Pruned posts still count: a cursor
// below the retained range means "saw everything older", not "more to fetch".
func unread(u *RoomUser) int {
	n := state.NextSeq - 1 - u.Cursor
	if n < 0 {
		return 0
	}
	return n
}

// lastSeq returns the newest post's sequence number (0 for an empty room).
func lastSeq() int {
	if n := len(state.Posts); n > 0 {
		return state.Posts[n-1].Seq
	}
	return 0
}

// wasSeen reports whether the msg id is in the replay-guard ring. Caller holds
// roomMu.
func wasSeen(id string) bool {
	for _, s := range state.Seen {
		if s == id {
			return true
		}
	}
	return false
}

// markSeen records the msg id in the replay-guard ring. Caller holds roomMu.
func markSeen(id string) {
	state.Seen = append(state.Seen, id)
	if len(state.Seen) > seenRingCap {
		state.Seen = state.Seen[len(state.Seen)-seenRingCap:]
	}
}

// msgID builds the replay-guard id for a DM: sender hex + sender-clock unix
// seconds — the same tuple the driver's protocol-level dedup keys on, stable
// across live reception and device-replayed sync.
func msgID(keyHex string, ts time.Time) string {
	return keyHex + "-" + strconv.FormatInt(ts.Unix(), 10)
}
