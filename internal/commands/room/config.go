package room

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/stempit/meshcore-go-bot/internal/config"
)

// cfg is the .room config section (yaml key "room"). It is opt-in via Enabled
// (same convention as poker/words). The int fields are defaulted at their use
// sites when zero.
type cfg struct {
	Enabled  bool       `yaml:"enabled"`   // opt-in: .room stays off unless true
	Default  string     `yaml:"default"`   // room bare ".room ..." commands target; default "main"
	MaxPosts int        `yaml:"max_posts"` // history ring fallback, default 200
	GetBatch int        `yaml:"get_batch"` // ".room get" default batch, default 10
	MaxBatch int        `yaml:"max_batch"` // ".room get n" hard cap, default 20
	Rooms    []roomDecl `yaml:"rooms"`     // declared rooms; empty → one implicit open room
}

// roomDecl declares one room. The default room need not be listed — it is
// appended implicitly as an open room unless listed here.
type roomDecl struct {
	Name     string   `yaml:"name"`
	Closed   bool     `yaml:"closed"`    // restrict to `allow` keys; default false (open)
	Allow    []string `yaml:"allow"`     // lowercase-hex pubkey prefixes when closed
	MaxPosts int      `yaml:"max_posts"` // per-room ring override; 0 = fallback
}

var conf cfg

func (plugin) ConfigKey() string { return "room" }

// Configure decodes the section and validates it. It runs once at startup.
func (plugin) Configure(s config.Sectioner) error {
	if err := s.Section("room", &conf); err != nil {
		return err
	}
	return normalize(&conf)
}

// roomNameRe limits room names to the shape the parser can route unambiguously
// (lowercase word characters; the parser matches case-insensitively).
var roomNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,12}$`)

// reservedVerbs are the command words that always own the first token; a room
// can never be named one of these.
var reservedVerbs = map[string]bool{
	"status": true, "get": true, "send": true, "on": true, "off": true,
	"last": true, "who": true, "list": true, "help": true,
}

// normalize trims/normalizes the config in place and validates it: room names
// are unique, parseable and not reserved; closed rooms carry an allow list;
// the default room always resolves (an unlisted one is appended as open).
func normalize(c *cfg) error {
	for _, f := range []struct {
		name string
		v    int
	}{
		{"max_posts", c.MaxPosts},
		{"get_batch", c.GetBatch},
		{"max_batch", c.MaxBatch},
	} {
		if f.v < 0 {
			return fmt.Errorf("room: %s must be >= 0", f.name)
		}
	}
	c.Default = strings.ToLower(strings.TrimSpace(c.Default))
	if c.Default == "" {
		c.Default = "main"
	}

	seen := map[string]bool{}
	for i := range c.Rooms {
		r := &c.Rooms[i]
		r.Name = strings.ToLower(strings.TrimSpace(r.Name))
		if !roomNameRe.MatchString(r.Name) {
			return fmt.Errorf("room: rooms[%d]: name %q must match [a-z0-9_-]{1,12}", i, r.Name)
		}
		if reservedVerbs[r.Name] {
			return fmt.Errorf("room: rooms[%d]: %q is a command word and cannot be a room name", i, r.Name)
		}
		if seen[r.Name] {
			return fmt.Errorf("room: rooms[%d]: duplicate name %q", i, r.Name)
		}
		seen[r.Name] = true
		for j, k := range r.Allow {
			r.Allow[j] = strings.ToLower(strings.TrimSpace(k))
			if r.Allow[j] == "" {
				return fmt.Errorf("room: %s: empty allow entry", r.Name)
			}
		}
		if r.Closed && len(r.Allow) == 0 {
			return fmt.Errorf("room: %s: closed needs at least one allow key, otherwise nobody could use it", r.Name)
		}
	}
	if !seen[c.Default] {
		// Bare ".room" commands need a target: append the unlisted default as
		// an open room so a minimal config just works. It must pass the same
		// checks as a listed room — this name also becomes a filename.
		if !roomNameRe.MatchString(c.Default) || reservedVerbs[c.Default] {
			return fmt.Errorf("room: default %q must match [a-z0-9_-]{1,12} and must not be a command word", c.Default)
		}
		c.Rooms = append(c.Rooms, roomDecl{Name: c.Default})
	}
	return nil
}

func (plugin) Enabled() bool { return conf.Enabled }

// defaultRoom returns the room bare ".room" commands target.
func defaultRoom() string { return conf.Default }

// roomNames lists declared rooms in config order (default included).
func roomNames() []string {
	names := make([]string, 0, len(conf.Rooms))
	for _, r := range conf.Rooms {
		names = append(names, r.Name)
	}
	return names
}

// findRoom returns the declaration of a declared room, nil when undeclared.
func findRoom(name string) *roomDecl {
	for i := range conf.Rooms {
		if conf.Rooms[i].Name == name {
			return &conf.Rooms[i]
		}
	}
	return nil
}

// roomExists reports whether name is a declared room.
func roomExists(name string) bool { return findRoom(name) != nil }

// maxPostsFor returns the room's history ring size (override or fallback,
// default 200).
func maxPostsFor(name string) int {
	if r := findRoom(name); r != nil && r.MaxPosts > 0 {
		return r.MaxPosts
	}
	if conf.MaxPosts > 0 {
		return conf.MaxPosts
	}
	return 200
}

// getBatch returns the default ".room get" batch (default 10).
func getBatch() int {
	if conf.GetBatch > 0 {
		return conf.GetBatch
	}
	return 10
}

// maxBatch returns the ".room get n" hard cap (default 20).
func maxBatch() int {
	if conf.MaxBatch > 0 {
		return conf.MaxBatch
	}
	return 20
}

// isOpenRoom reports whether keyHex may use the named room: everyone unless
// it is closed, and a closed room matches its allow list by lowercase-hex
// prefix. Undeclared rooms admit nobody.
func isOpenRoom(name, keyHex string) bool {
	r := findRoom(name)
	if r == nil {
		return false
	}
	if !r.Closed {
		return true
	}
	for _, k := range r.Allow {
		if k != "" && strings.HasPrefix(keyHex, k) {
			return true
		}
	}
	return false
}

// isRoomName reports whether s has the shape of a room name (used for error
// wording: a valid-looking but undeclared name gets a different hint than
// garbage).
func isRoomName(s string) bool {
	return roomNameRe.MatchString(s)
}
