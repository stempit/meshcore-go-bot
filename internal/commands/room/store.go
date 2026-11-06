package room

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Room state lives under log/room, one file per room; roomDir is a var (not a
// const) so tests can redirect it.
var (
	roomDir     = "log/room"
	seenRingCap = 64 // replay-guard memory: recent "key-timestamp" ids per room
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

// roomState is one room's persisted store: the post history ring, per-user
// cursors, and the replay-guard ring. The layout follows the bbs store
// (atomic save + .bak generation); history is append-only — prune drops the
// oldest posts, cursors pointing into pruned range stay valid (they only
// ever count).
type roomState struct {
	Posts   []RoomPost           `json:"posts"`
	Users   map[string]*RoomUser `json:"users"`
	NextSeq int                  `json:"next_seq"`
	Seen    []string             `json:"seen"` // replay guard: recent msg ids
}

// store is one room's in-memory view, lazily loaded on first touch. All
// stores share roomMu.
type store struct {
	name   string
	st     *roomState
	loaded bool
}

var (
	roomMu sync.Mutex
	stores = map[string]*store{} // room name → lazy store
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

// roomPath is the store file for a room: log/room/<name>.json.
func roomPath(name string) string {
	return filepath.Join(roomDir, name+".json")
}

// loadStore returns the room's store, loading it from disk on first touch.
// Caller must hold roomMu.
func loadStore(name string) (*store, error) {
	s, ok := stores[name]
	if !ok {
		s = &store{name: name}
		stores[name] = s
	}
	if s.loaded {
		return s, nil
	}
	if err := os.MkdirAll(roomDir, 0o755); err != nil {
		return nil, err
	}
	file := roomPath(name)
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		// Either a fresh install or the atomic-save rename window: fall back
		// to the previous generation kept by save.
		data, err = os.ReadFile(file + ".bak")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(data) == 0 {
		s.st = newState()
		s.loaded = true
		return s, nil
	}
	st := newState()
	if err := json.Unmarshal(data, st); err != nil {
		// The main file is corrupt; try the previous generation before
		// giving up (save keeps one around).
		bak, bakErr := os.ReadFile(file + ".bak")
		if bakErr != nil {
			return nil, err
		}
		if bakErr := json.Unmarshal(bak, st); bakErr != nil {
			return nil, err // both generations unreadable
		}
	}
	if st.Users == nil {
		st.Users = map[string]*RoomUser{}
	}
	if st.NextSeq < 1 {
		st.NextSeq = 1
	}
	s.st = st
	s.loaded = true
	return s, nil
}

// newState returns an empty store with the sequence watermark at 1.
func newState() *roomState {
	return &roomState{Users: map[string]*RoomUser{}, NextSeq: 1}
}

// save serialises the store to disk. Caller must hold roomMu. The write is
// atomic (temp file + rename) with the previous generation kept as .bak —
// same scheme as the bbs store: these nodes run unattended and a crash
// mid-write must not lose the room.
func (s *store) save() error {
	s.prune()
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(roomDir, 0o755); err != nil {
		return err
	}
	file := roomPath(s.name)
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(file, file+".bak"); err != nil && !errors.Is(err, os.ErrNotExist) {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, file)
}

// prune caps history growth: beyond the room's configured max the oldest
// posts are dropped. Caller holds roomMu.
func (s *store) prune() {
	max := maxPostsFor(s.name)
	if len(s.st.Posts) <= max {
		return
	}
	s.st.Posts = s.st.Posts[len(s.st.Posts)-max:]
}

// user returns the participant record for keyHex, creating (and naming) one on
// first contact. Caller holds roomMu.
func (s *roomState) user(keyHex, name string) *RoomUser {
	u, ok := s.Users[keyHex]
	if !ok {
		u = &RoomUser{Key: keyHex}
		s.Users[keyHex] = u
	}
	if name != "" {
		u.Name = name
	}
	return u
}

// unread counts the user's unseen posts: newer than the cursor and not
// authored by them — exactly the set get delivers, so every unread figure
// (list, status, sub.on, the sent confirmation) agrees with what get does.
// Own posts are skipped (and consumed) by get, never counted.
func (s *roomState) unread(u *RoomUser) int {
	n := 0
	for _, p := range s.Posts {
		if p.Seq > u.Cursor && p.AuthorKey != u.Key {
			n++
		}
	}
	return n
}

// wasSeen reports whether the msg id is in the replay-guard ring. Caller holds
// roomMu.
func (s *roomState) wasSeen(id string) bool {
	for _, id2 := range s.Seen {
		if id2 == id {
			return true
		}
	}
	return false
}

// markSeen records the msg id in the replay-guard ring. Caller holds roomMu.
func (s *roomState) markSeen(id string) {
	s.Seen = append(s.Seen, id)
	if len(s.Seen) > seenRingCap {
		s.Seen = s.Seen[len(s.Seen)-seenRingCap:]
	}
}

// msgID builds the replay-guard id for a DM: sender hex + sender-clock unix
// seconds — the same tuple the driver's protocol-level dedup keys on, stable
// across live reception and device-replayed sync.
func msgID(keyHex string, ts time.Time) string {
	return keyHex + "-" + strconv.FormatInt(ts.Unix(), 10)
}
