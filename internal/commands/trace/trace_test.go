package trace

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stempit/meshcore-go"
	"github.com/stempit/meshcore-go-bot/internal/config"
	"github.com/stempit/meshcore-go-bot/internal/support/registry"
)

// TestHandleRouting pins the trigger/arg split: bare .trace keeps the classic
// output, "full" collects, anything else gets the usage line.
func TestHandleRouting(t *testing.T) {
	t.Cleanup(func() { collector = nil })
	collector = nil // ensure the fallback path runs even if wiring ran earlier

	p := plugin{}
	pm := &registry.PingMeta{PathLength: 0xFF}

	out, err := p.Handle(ctxWith(pm, ""))
	if err != nil || out[0] != "Trace direct" {
		t.Fatalf("bare .trace = %q (err %v), want the classic output", out, err)
	}
	out, err = p.Handle(ctxWith(pm, "full"))
	if err != nil {
		t.Fatalf(".trace full error: %v", err)
	}
	if !strings.Contains(out[0], "Trace direct") || !strings.Contains(out[0], "no other copies heard") {
		t.Fatalf(".trace full without collector = %q, want bare output + no-copies note", out[0])
	}
	out, _ = p.Handle(ctxWith(pm, "xyz"))
	if out[0] != ".trace [full]" {
		t.Fatalf("unknown arg = %q, want usage", out[0])
	}
}

// TestHandleFullRendersPaths exercises the whole ".trace full" flow against a
// wired collector: copies fed before the command (copy #1 precedes the decoded
// frame) and one arriving mid-window are all reported, sorted by hops, each
// with its total path distance. Runs one real (minimum-clamped) window.
func TestHandleFullRendersPaths(t *testing.T) {
	pskless := stubSource{
		self: &meshcore.SelfInfo{PublicKey: []byte{0x42}},
		contacts: map[string]*meshcore.Contact{
			"aa": {PublicKey: []byte{0xAA}, Latitude: f64(1), Longitude: f64(0)},
			"bb": {PublicKey: []byte{0xBB}, Latitude: f64(2), Longitude: f64(0)},
		},
	}
	col := NewCollector(pskless)
	t.Cleanup(func() { collector = nil })
	collector = col

	// Copy #1 and #2: already in the buffer when Handle runs, both at the
	// same instant — tied for first-heard, so both render the (*) baseline.
	base := time.Now()
	col.Observe(meshcore.RawRx{Data: buildFloodTXTMSG(0x42, 0x11, []byte{0xAA}, 16), At: base})
	col.Observe(meshcore.RawRx{Data: buildFloodTXTMSG(0x42, 0x11, []byte{0xAA, 0xBB}, 16), At: base})
	// Copy #3: lands mid-window, like a longer flood route arriving late.
	go func() {
		time.Sleep(500 * time.Millisecond)
		col.Observe(meshcore.RawRx{Data: buildFloodTXTMSG(0x42, 0x11, nil, 16), At: time.Now()})
	}()

	pm := &registry.PingMeta{
		PathLength: 1,
		PathBytes:  []byte{0xAA},
		Timestamp:  time.Now(),
		SenderLat:  f64(3), SenderLon: f64(0),
		SelfLat: 0, SelfLon: 0,
	}
	cfg := &config.Config{Trace: config.TraceConfig{CollectWindowSec: 3}}
	c := ctxWith(pm, "full")
	c.SenderKey = string([]byte{0x11})
	c.Cfg = cfg

	out, err := plugin{}.Handle(c)
	if err != nil {
		t.Fatalf("handleFull error: %v", err)
	}
	// Equidistant hops: every leg is 1° of latitude, so each route totals the
	// same chain self→hop(s)→sender; compute it the way the renderer does.
	// Delays: the two pre-fed copies tie for the baseline (*); the mid-window
	// direct copy lands ~0.5s late, so its duration is matched loosely.
	leg := haversineKm(0, 0, 1, 0)
	one := fmt.Sprintf("[AA] (*) %.1fkm", leg+2*leg)
	two := fmt.Sprintf("[AA][BB] (*) %.1fkm", 3*leg)
	lines := strings.Split(out[0], "\n")
	if len(lines) != 4 {
		t.Fatalf("reply = %q, want 4 lines (header + 3 paths)", out[0])
	}
	if lines[0] != "3 paths in 3s" {
		t.Errorf("header = %q, want %q", lines[0], "3 paths in 3s")
	}
	if !regexp.MustCompile(`\[direct\] \(0\.\d+s\) 333\.6km`).MatchString(lines[1]) {
		t.Errorf("direct line = %q, want delay + exact distance", lines[1])
	}
	if lines[2] != one {
		t.Errorf("[AA] line = %q, want %q", lines[2], one)
	}
	if lines[3] != two {
		t.Errorf("[AA][BB] line = %q, want %q", lines[3], two)
	}
}

// TestHandleFullDistanceOmittedWhenUnknown pins the rule: a path whose hop
// positions are not all known shows the delay only — no partial distance.
func TestHandleFullDistanceOmittedWhenUnknown(t *testing.T) {
	src := stubSource{
		self: &meshcore.SelfInfo{PublicKey: []byte{0x42}},
		contacts: map[string]*meshcore.Contact{
			"aa": {PublicKey: []byte{0xAA}}, // no GPS
		},
	}
	col := NewCollector(src)
	t.Cleanup(func() { collector = nil })
	collector = col
	col.Observe(meshcore.RawRx{Data: buildFloodTXTMSG(0x42, 0x11, []byte{0xAA, 0xBB}, 16), At: time.Now()})

	pm := &registry.PingMeta{SenderLat: f64(3), SenderLon: f64(0)}
	cfg := &config.Config{Trace: config.TraceConfig{CollectWindowSec: 3}}
	c := ctxWith(pm, "full")
	c.SenderKey = string([]byte{0x11})
	c.Cfg = cfg

	out, _ := plugin{}.Handle(c)
	if !strings.HasSuffix(out[0], "[AA][BB] (*)") {
		t.Fatalf("reply = %q, want delay-only line for the unknown-coords path", out[0])
	}
	if strings.Contains(out[0], "km") {
		t.Fatalf("reply = %q, want no distance suffix", out[0])
	}
}

// TestHandleFullApproxDistance pins the bare-trace approximation rule: with
// the sender position unknown but every hop known, the chain stops at the
// last hop and the line carries the "+" minimum-estimate marker.
func TestHandleFullApproxDistance(t *testing.T) {
	src := stubSource{
		self: &meshcore.SelfInfo{PublicKey: []byte{0x42}},
		contacts: map[string]*meshcore.Contact{
			"aa": {PublicKey: []byte{0xAA}, Latitude: f64(1), Longitude: f64(0)},
			"bb": {PublicKey: []byte{0xBB}, Latitude: f64(2), Longitude: f64(0)},
		},
	}
	col := NewCollector(src)
	t.Cleanup(func() { collector = nil })
	collector = col
	col.Observe(meshcore.RawRx{Data: buildFloodTXTMSG(0x42, 0x11, []byte{0xAA, 0xBB}, 16), At: time.Now()})

	pm := &registry.PingMeta{SenderLat: nil, SenderLon: nil} // sender position unknown
	cfg := &config.Config{Trace: config.TraceConfig{CollectWindowSec: 3}}
	c := ctxWith(pm, "full")
	c.SenderKey = string([]byte{0x11})
	c.Cfg = cfg

	out, _ := plugin{}.Handle(c)
	// Chain self→AA→BB: two 1° legs, no sender leg, "+" marker.
	leg := haversineKm(0, 0, 1, 0)
	want := fmt.Sprintf("[AA][BB] (*) %.1fkm+", 2*leg)
	if !strings.HasSuffix(out[0], want) {
		t.Fatalf("reply = %q, want approx line %q", out[0], want)
	}
}

// TestHandleFullHopNames pins the name rendering rule: hops resolve to
// repeater names from contacts by default, fall back to the hash per hop when
// the repeater is unknown, and revert to hashes entirely when show_names is
// disabled.
func TestHandleFullHopNames(t *testing.T) {
	src := stubSource{
		self: &meshcore.SelfInfo{PublicKey: []byte{0x42}},
		contacts: map[string]*meshcore.Contact{
			"aa": {PublicKey: []byte{0xAA}, Name: "R1", Latitude: f64(1), Longitude: f64(0)},
			"bb": {PublicKey: []byte{0xBB}, Latitude: f64(2), Longitude: f64(0)}, // unnamed repeater
		},
	}
	col := NewCollector(src)
	t.Cleanup(func() { collector = nil })
	collector = col
	col.Observe(meshcore.RawRx{Data: buildFloodTXTMSG(0x42, 0x11, []byte{0xAA, 0xBB}, 16), At: time.Now()})

	pm := &registry.PingMeta{SenderLat: nil, SenderLon: nil}
	leg := haversineKm(0, 0, 1, 0)

	// Default: names on; the unnamed hop falls back to its hash.
	c := ctxWith(pm, "full")
	c.SenderKey = string([]byte{0x11})
	c.Cfg = &config.Config{Trace: config.TraceConfig{CollectWindowSec: 3}}
	out, _ := plugin{}.Handle(c)
	if want := fmt.Sprintf("[R1][BB] (*) %.1fkm+", 2*leg); !strings.HasSuffix(out[0], want) {
		t.Fatalf("reply = %q, want named line %q", out[0], want)
	}

	// show_names=false: hashes for every hop.
	off := false
	c.Cfg = &config.Config{Trace: config.TraceConfig{CollectWindowSec: 3, ShowNames: &off}}
	out, _ = plugin{}.Handle(c)
	if want := fmt.Sprintf("[AA][BB] (*) %.1fkm+", 2*leg); !strings.HasSuffix(out[0], want) {
		t.Fatalf("reply = %q, want hash line %q with show_names off", out[0], want)
	}
}

// TestShowNamesDefault pins the gate default: absent config means names.
func TestShowNamesDefault(t *testing.T) {
	if !showNames(config.TraceConfig{}) {
		t.Fatal("absent show_names must default to enabled")
	}
}

// TestHandleFullDisabled pins the config gate: full_enabled=false answers
// ".trace full" with the bare trace — no collection, no note.
func TestHandleFullDisabled(t *testing.T) {
	src := stubSource{self: &meshcore.SelfInfo{PublicKey: []byte{0x42}}}
	col := NewCollector(src)
	t.Cleanup(func() { collector = nil })
	collector = col
	col.Observe(meshcore.RawRx{Data: buildFloodTXTMSG(0x42, 0x11, []byte{0xAA}, 16), At: time.Now()})

	disabled := false
	pm := &registry.PingMeta{PathLength: 0xFF}
	cfg := &config.Config{Trace: config.TraceConfig{FullEnabled: &disabled}}
	c := ctxWith(pm, "full")
	c.SenderKey = string([]byte{0x11})
	c.Cfg = cfg

	out, _ := plugin{}.Handle(c)
	if out[0] != "Trace direct" {
		t.Fatalf("disabled reply = %q, want the bare trace", out[0])
	}
}

// TestFullEnabledDefault pins the gate default: absent config means enabled.
func TestFullEnabledDefault(t *testing.T) {
	if !fullEnabled(config.TraceConfig{}) {
		t.Fatal("absent full_enabled must default to enabled")
	}
	on := true
	if !fullEnabled(config.TraceConfig{FullEnabled: &on}) {
		t.Fatal("full_enabled=true must stay enabled")
	}
}

// TestWindowAndCapClamps pins the config defaults and clamps.
func TestWindowAndCapClamps(t *testing.T) {
	for cfg, want := range map[config.TraceConfig]time.Duration{
		{}:                      15 * time.Second,
		{CollectWindowSec: 1}:   3 * time.Second,
		{CollectWindowSec: 100}: 30 * time.Second,
		{CollectWindowSec: 15}:  15 * time.Second,
	} {
		if got := collectWindow(cfg); got != want {
			t.Errorf("collectWindow(%+v) = %v, want %v", cfg, got, want)
		}
	}
	for cfg, want := range map[config.TraceConfig]int{
		{}:            8,
		{MaxPaths: 3}: 3,
	} {
		if got := maxPaths(cfg); got != want {
			t.Errorf("maxPaths(%+v) = %d, want %d", cfg, got, want)
		}
	}
}

// ctxWith builds a DM-style Ctx for the trace plugin; the caller sets
// SenderKey/Cfg when the test needs them.
func ctxWith(pm *registry.PingMeta, rest string) registry.Ctx {
	c := registry.NewCtx(".trace "+rest, "", "", "Alice", pm, nil)
	c.Rest = rest
	return c
}
