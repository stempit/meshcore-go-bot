// Package shrink centralizes fitting bot replies into the MeshCore 140-byte
// message limit. It is the config-driven policy layer over the fit package
// (meshcore-kit): variants, limits and the lossless squeeze come from
// the Shrink config section (with per-send-path overrides), and the "llm"
// variant compresses via the configured provider (internal/bridge/llm)
// before falling back to fit's splitter. The pure fitting algorithms live in
// github.com/stempit/meshcore-kit/fit.
package shrink

import (
	"github.com/stempit/meshcore-go-bot/internal/config"
	"github.com/stempit/meshcore-kit/fit"
)

func valid(v string) bool {
	switch fit.Variant(v) {
	case fit.Truncate, fit.Split, fit.LLM, fit.Skip:
		return true
	}
	return false
}

// resolveVariant picks the variant for name: per-name override, then the
// global default, then Truncate as a safe last resort. Invalid values are
// skipped.
func resolveVariant(name string) fit.Variant {
	s := config.Get().Shrink
	if v, ok := s.Overrides[name]; ok && valid(v) {
		return fit.Variant(v)
	}
	if valid(s.Default) {
		return fit.Variant(s.Default)
	}
	return fit.Truncate
}

// HelpName is the shrink variant key for help output: the ".help" index and
// every per-command long help reply. Help is structural content — a command
// reference the sender asked for verbatim — so it is always split and never
// LLM-compressed, regardless of Shrink config.
const HelpName = "help"

// RoomName is the shrink variant key for the .room plugin: posts are
// user-authored content. They are always split, never LLM-compressed — an
// LLM rewriting someone's message is a fidelity and privacy failure, and a
// network round-trip inside the room's store lock would stall every command.
const RoomName = "room"

// OptionsFor resolves the fit options for a named send path from the Shrink
// config section, variant overrides included.
func OptionsFor(name string) fit.Options {
	s := config.Get().Shrink
	variant := resolveVariant(name)
	if name == HelpName || name == RoomName {
		variant = fit.Split // verbatim content survives, whatever the config says
	}
	o := fit.Options{
		MaxBytes: s.MaxBytes,
		Ellipsis: s.Ellipsis,
		CyrToLat: s.CyrToLat,
		Variant:  variant,
	}
	if o.Variant == fit.LLM {
		o.Compress = llmCompressor
	}
	return o
}

// Fit turns a handler's logical messages into mesh-ready wire messages: it fits
// each element of msgs via the variant configured for name, flattens the result,
// and prepends prefix to the first wire message. Nothing returned exceeds the
// configured max. prefix is "" when there is none (DMs, bot-originated posts).
func Fit(name, prefix string, msgs []string) []string {
	return fit.Fit(OptionsFor(name), prefix, msgs)
}

// ValidateConfig logs a warning for any unknown variant in the Shrink config.
// Unknown values are ignored at runtime (they fall through to the default).
func ValidateConfig() {
	s := config.Get().Shrink
	if s.Default != "" && !valid(s.Default) {
		log().Warn("shrink: unknown default variant, using truncate", "value", s.Default)
	}
	for name, v := range s.Overrides {
		if !valid(v) {
			log().Warn("shrink: unknown override variant, ignoring", "name", name, "value", v)
		}
	}
}
