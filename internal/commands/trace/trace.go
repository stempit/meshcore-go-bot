// Package trace answers .trace with hop count and distance from the last ping,
// and .trace full with every path the message was heard on.
package trace

import (
	"embed"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/stempit/meshcore-go-bot/internal/config"
	"github.com/stempit/meshcore-go-bot/internal/support/i18n"
	"github.com/stempit/meshcore-go-bot/internal/support/registry"
)

//go:embed lang/*.yaml
var langFS embed.FS

var tr = i18n.MustLoad(langFS)

func init() { registry.Register(plugin{}) }

type plugin struct{}

// collector is the ".trace full" path collector, wired at startup (bot.go,
// same injection pattern as room.SetDelivery). nil when not wired — then
// "full" falls back to the bare rendering with a "no other copies" note.
var collector *Collector

// SetCollector wires the path collector.
func SetCollector(c *Collector) { collector = c }

func (plugin) Meta() registry.Meta {
	return registry.Meta{
		Name:      "trace",
		Triggers:  []string{".trace"},
		Match:     registry.Prefix,
		HelpShort: "hop count & distance; .trace full: all paths",
		HelpLong:  ".trace — hop count & distance from last ping; .trace full — all paths heard",
		Group:     "Service",
	}
}

// LocalizedHelp returns the per-language long help for ".trace help".
func (plugin) LocalizedHelp(lang string) string { return tr.T(lang, "help.long") }

func (plugin) Handle(c registry.Ctx) ([]string, error) {
	switch c.Rest {
	case "":
		return []string{formatTechPong(c.Lang, c.PingMeta)}, nil
	case "full":
		return []string{handleFull(c)}, nil
	default:
		return []string{tr.T(c.Lang, "full.usage")}, nil
	}
}

// handleFull collects every path the command message was heard on and renders
// them as one \n-joined message, shortest route first. Each line carries the
// route's delivery delay (time after the first heard copy — flood propagation
// through that route) plus the total repeater-based path distance when it is
// fully computable; the direct self→sender distance is deliberately omitted —
// it is the same for every path and only the route totals differ. Without a
// wired collector (or when nothing but the first copy was heard) it falls
// back to the bare rendering plus a note; disabled by config it just answers
// bare.
func handleFull(c registry.Ctx) string {
	bare := formatTechPong(c.Lang, c.PingMeta)
	if !fullEnabled(traceCfg(c)) {
		return bare
	}
	if collector == nil {
		return bare + "\n" + tr.T(c.Lang, "full.none")
	}
	match, window := buildMatch(c)
	if match == nil {
		return bare + "\n" + tr.T(c.Lang, "full.none")
	}
	entries, busy := collector.Collect(match, window)
	if busy {
		return tr.T(c.Lang, "full.busy")
	}
	if max := maxPaths(traceCfg(c)); len(entries) > max {
		entries = entries[:max]
	}
	if len(entries) == 0 {
		return bare + "\n" + tr.T(c.Lang, "full.none")
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, tr.T(c.Lang, "full.paths"), len(entries), int(window.Seconds()))
	names := showNames(traceCfg(c))
	for _, e := range entries {
		sb.WriteString("\n")
		writePathLine(&sb, c.Lang, c.PingMeta, e, names)
	}
	return sb.String()
}

// fullEnabled reports whether ".trace full" is enabled; absent config means
// enabled.
func fullEnabled(cfg config.TraceConfig) bool {
	return cfg.FullEnabled == nil || *cfg.FullEnabled
}

// showNames reports whether ".trace full" hops render as repeater names;
// absent config means enabled (unknown hops still fall back to the hash).
func showNames(cfg config.TraceConfig) bool {
	return cfg.ShowNames == nil || *cfg.ShowNames
}

// buildMatch derives the raw-packet matcher for the command message plus the
// collection window from config. It returns a nil match when the message
// cannot be correlated on air: a DM without our own key byte (self info not
// synced yet) or a channel without a usable PSK cannot be told apart from
// other traffic, and a wrong-path report is worse than a short one.
func buildMatch(c registry.Ctx) (Match, time.Duration) {
	window := collectWindow(traceCfg(c))
	if c.IsDM() {
		if len(c.SenderKey) == 0 {
			return nil, 0
		}
		selfKey0, ok := collector.SelfKeyByte()
		if !ok {
			return nil, 0
		}
		return DMMatch(selfKey0, c.SenderKey[0], len(c.Text)), window
	}
	psk := collector.ChannelPSK(c.Channel)
	if len(psk) < 16 || c.PingMeta == nil {
		return nil, 0
	}
	return ChannelMatch(psk, uint32(c.PingMeta.Timestamp.Unix()), c.SenderName, c.Text), window
}

// traceCfg reads the Trace config section, tolerating a nil Cfg (tests).
func traceCfg(c registry.Ctx) config.TraceConfig {
	if c.Cfg == nil {
		return config.TraceConfig{}
	}
	return c.Cfg.Trace
}

// collectWindow resolves the ".trace full" listening window from config,
// clamped so a typo cannot stall the worker pool for a minute or blink.
func collectWindow(cfg config.TraceConfig) time.Duration {
	s := cfg.CollectWindowSec
	if s <= 0 {
		s = 15
	}
	return time.Duration(min(30, max(3, s))) * time.Second
}

// maxPaths resolves the reported-path cap from config; 0 → default 8.
func maxPaths(cfg config.TraceConfig) int {
	if cfg.MaxPaths <= 0 {
		return 8
	}
	return cfg.MaxPaths
}

// writePathLine renders one path entry in the bare trace's style: [hash or
// repeater name] (delivery delay) distance — where the delay sits in
// parentheses and the distance carries the bare trace's "+" marker when the
// sender position is unknown and the chain stops at the last hop. Hash sizes
// of 1–4 bytes per hop are supported; distances stay technical (metres below
// 1 km). Names come from the contact list and only replace the hash when
// known and showNames is set.
func writePathLine(sb *strings.Builder, lang string, meta *registry.PingMeta, e PathEntry, names bool) {
	if e.Hops > 0 && len(e.Path)%e.Hops == 0 {
		bph := len(e.Path) / e.Hops
		for i := 0; i < e.Hops; i++ {
			if names && i < len(e.Names) && e.Names[i] != "" {
				fmt.Fprintf(sb, "[%s]", e.Names[i])
			} else {
				fmt.Fprintf(sb, "[%s]", strings.ToUpper(hex.EncodeToString(e.Path[i*bph:(i+1)*bph])))
			}
		}
		sb.WriteString(" ")
	} else if e.Hops == 0 {
		sb.WriteString(tr.T(lang, "full.direct"))
		sb.WriteString(" ")
	}
	if e.Delay == 0 {
		// The baseline: the first copy heard, all delays are relative to it.
		sb.WriteString(tr.T(lang, "full.first"))
	} else {
		fmt.Fprintf(sb, tr.T(lang, "full.sec"), e.Delay.Seconds())
	}
	if km, approx, ok := pathKm(meta, e); ok {
		sb.WriteString(" ")
		if km < 1 {
			fmt.Fprintf(sb, tr.T(lang, "full.m"), km*1000)
		} else {
			fmt.Fprintf(sb, tr.T(lang, "full.km"), km)
		}
		if approx {
			sb.WriteString("+")
		}
	}
}

// pathKm totals the route self → hop1 → … → sender, the same haversine chain
// formatTechPong walks for the first path. With the sender position known the
// total is exact; without it the chain stops at the last hop — a minimum
// estimate rendered with the bare trace's "+" marker. ok is false when the
// chain is broken (an unknown hop) or there is nothing to total (direct
// delivery without sender coords), in which case the line shows no distance.
func pathKm(meta *registry.PingMeta, e PathEntry) (km float64, approx, ok bool) {
	if meta == nil || len(e.HopCoords) != e.Hops || len(e.Known) != e.Hops {
		return 0, false, false
	}
	for _, k := range e.Known {
		if !k {
			return 0, false, false
		}
	}
	total := 0.0
	prevLat, prevLon := meta.SelfLat, meta.SelfLon
	for _, crd := range e.HopCoords {
		total += haversineKm(prevLat, prevLon, crd[0], crd[1])
		prevLat, prevLon = crd[0], crd[1]
	}
	if meta.SenderLat != nil && meta.SenderLon != nil {
		total += haversineKm(prevLat, prevLon, *meta.SenderLat, *meta.SenderLon)
		return total, false, true
	}
	if e.Hops > 0 {
		return total, true, true
	}
	return 0, false, false
}

// haversineKm returns the great-circle distance in km between two lat/lon points.
func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return r * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func formatTechPong(lang string, meta *registry.PingMeta) string {
	var sb strings.Builder

	// 0xFF means direct delivery (no repeaters)
	if meta.PathLength == 0xFF {
		sb.WriteString(tr.T(lang, "direct"))
	} else {
		sb.WriteString(tr.T(lang, "hops", meta.PathLength))
		if len(meta.PathBytes) > 0 && meta.PathLength > 0 {
			totalBytes := len(meta.PathBytes)
			hops := meta.PathLength
			// detect 1-byte or 2-byte per hop
			var bytesPerHop int
			if totalBytes == hops {
				bytesPerHop = 1
			} else if totalBytes == hops*2 {
				bytesPerHop = 2
			}
			if bytesPerHop > 0 {
				sb.WriteString(": ")
				for i := 0; i < hops; i++ {
					chunk := meta.PathBytes[i*bytesPerHop : (i+1)*bytesPerHop]
					fmt.Fprintf(&sb, "[%s]", strings.ToUpper(hex.EncodeToString(chunk)))
				}
			}
		}
	}

	if !meta.Timestamp.IsZero() {
		pingTime := time.Since(meta.Timestamp)
		if pingTime > 0 && pingTime < 24*time.Hour {
			fmt.Fprintf(&sb, " (%.0fs)", pingTime.Seconds())
		}
	}

	selfKnown := meta.SelfLat != 0 || meta.SelfLon != 0
	senderKnown := meta.SenderLat != nil && meta.SenderLon != nil

	// If sender coordinates are unknown, fall back to the first repeater's coordinates.
	// In that case we append "+" to the distance unit to signal it's a minimum estimate.
	var endLat, endLon float64
	approx := false
	if senderKnown {
		endLat, endLon = *meta.SenderLat, *meta.SenderLon
	} else if len(meta.HopCoords) > 0 && len(meta.HopKnown) > 0 && meta.HopKnown[0] {
		endLat, endLon = meta.HopCoords[0][0], meta.HopCoords[0][1]
		approx = true
	}

	if selfKnown && (senderKnown || approx) {
		directKm := haversineKm(meta.SelfLat, meta.SelfLon, endLat, endLon)
		unit := "km"
		unitM := "m"
		if approx {
			unit = "km+"
			unitM = "m+"
		}

		// Try to compute total path distance if all intermediate hops have known positions.
		allHopsKnown := senderKnown && len(meta.HopKnown) == len(meta.HopCoords) && len(meta.HopCoords) == meta.PathLength
		if allHopsKnown {
			for _, known := range meta.HopKnown {
				if !known {
					allHopsKnown = false
					break
				}
			}
		}

		if allHopsKnown && meta.PathLength > 0 {
			// Path direction: self → hops[0] → hops[1] → ... → sender
			totalKm := 0.0
			prevLat, prevLon := meta.SelfLat, meta.SelfLon
			for _, coord := range meta.HopCoords {
				totalKm += haversineKm(prevLat, prevLon, coord[0], coord[1])
				prevLat, prevLon = coord[0], coord[1]
			}
			totalKm += haversineKm(prevLat, prevLon, *meta.SenderLat, *meta.SenderLon)
			if directKm < 1 {
				fmt.Fprintf(&sb, " %.0f%s->%.0f%s", directKm*1000, unitM, totalKm*1000, unitM)
			} else {
				fmt.Fprintf(&sb, " %.1f%s->%.1f%s", directKm, unit, totalKm, unit)
			}
		} else {
			if directKm < 1 {
				fmt.Fprintf(&sb, " %.0f%s", directKm*1000, unitM)
			} else {
				fmt.Fprintf(&sb, " %.1f%s", directKm, unit)
			}
		}
	}

	return sb.String()
}
