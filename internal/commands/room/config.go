package room

import (
	"fmt"
	"strings"

	"github.com/stempit/meshcore-go-bot/internal/config"
)

// cfg is the .room config section (yaml key "room"). It is opt-in via Enabled
// (same convention as poker/words). The int fields are defaulted at their use
// sites when zero.
type cfg struct {
	Enabled  bool     `yaml:"enabled"`   // opt-in: .room stays off unless true
	Closed   bool     `yaml:"closed"`    // restrict to `allow` keys; default false (open room)
	Allow    []string `yaml:"allow"`     // lowercase-hex pubkey prefixes when closed
	MaxPosts int      `yaml:"max_posts"` // history ring size, default 200
	GetBatch int      `yaml:"get_batch"` // ".room get" default batch, default 10
	MaxBatch int      `yaml:"max_batch"` // ".room get n" hard cap, default 20
}

var conf cfg

func (plugin) ConfigKey() string { return "room" }

// Configure decodes the section and validates ranges. It runs once at startup.
func (plugin) Configure(s config.Sectioner) error {
	if err := s.Section("room", &conf); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		v    int
	}{
		{"max_posts", conf.MaxPosts},
		{"get_batch", conf.GetBatch},
		{"max_batch", conf.MaxBatch},
	} {
		if f.v < 0 {
			return fmt.Errorf("room: %s must be >= 0", f.name)
		}
	}
	for i, k := range conf.Allow {
		conf.Allow[i] = strings.ToLower(strings.TrimSpace(k))
	}
	if conf.Closed && len(conf.Allow) == 0 {
		return fmt.Errorf("room: closed needs at least one allow key, otherwise nobody could use the room")
	}
	return nil
}

func (plugin) Enabled() bool { return conf.Enabled }

// maxPosts returns the configured history ring size (default 200).
func maxPosts() int {
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

// isOpen reports whether keyHex may use the room: everyone unless closed, and
// a closed room matches its allow list by lowercase-hex prefix.
func isOpen(keyHex string) bool {
	if !conf.Closed {
		return true
	}
	for _, k := range conf.Allow {
		if k != "" && strings.HasPrefix(keyHex, k) {
			return true
		}
	}
	return false
}
