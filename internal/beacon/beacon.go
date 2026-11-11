// Package beacon periodically makes the connected MeshCore device emit its own
// advert: flood adverts (propagated mesh-wide by repeaters) and/or direct
// adverts (single hop, heard by direct neighbors only), each on its own
// configurable period. The advertised identity is whatever the device already
// has stored; the beacon only triggers the device's SendSelfAdvert. Like the
// on-demand .advert command, sends bypass the dispatcher queue — adverts are
// not text.
package beacon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/stempit/meshcore-go-bot/internal/logging"
	"gopkg.in/yaml.v3"
)

var log = logging.For("beacon")

// strictInt is a YAML integer that refuses non-integer scalars. Plain int
// fields are not enough: yaml.v3 silently truncates floats into them
// (0.5 decodes to 0, which would silently turn an intended advert mode off),
// so the period fields use this type to fail loudly instead.
type strictInt int

func (i *strictInt) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag != "!!int" {
		return fmt.Errorf("expected a whole number, got %q", node.Value)
	}
	var v int
	if err := node.Decode(&v); err != nil {
		return err
	}
	*i = strictInt(v)
	return nil
}

// Config is decoded from the top-level "Advert" config section. Periods are
// whole numbers: the smallest positive flood period is 1 hour and the smallest
// positive direct period is 1 minute (integer units), and 0 turns the mode
// off. The beacon as a whole is off unless Enabled.
type Config struct {
	Enabled            bool      `yaml:"enabled"`
	FloodIntervalHours strictInt `yaml:"flood_interval_hours"` // flood advert period in hours; 0 = off
	DirectIntervalMin  strictInt `yaml:"direct_interval_min"`  // direct advert period in minutes; 0 = off
}

// Validate checks the periods, but only when the beacon is enabled. A disabled
// beacon — including a missing section, which decodes to the zero value — is
// always valid. Minimum periods are enforced by the integer units themselves
// (any nonzero whole number of hours is ≥ 1 hour, of minutes ≥ 1 minute), so
// only negativity and the all-off case need checking here.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.FloodIntervalHours < 0 || c.DirectIntervalMin < 0 {
		return fmt.Errorf("Advert: intervals must not be negative (flood_interval_hours=%d, direct_interval_min=%d)",
			c.FloodIntervalHours, c.DirectIntervalMin)
	}
	if c.FloodIntervalHours == 0 && c.DirectIntervalMin == 0 {
		return fmt.Errorf("Advert: enabled but both flood_interval_hours and direct_interval_min are 0")
	}
	return nil
}

// Sender is the part of the MeshCore client the beacon needs. It is an
// interface so tests can stub the radio.
type Sender interface {
	SendSelfAdvert(flood bool) error
	IsConnected() bool
}

// Beacon periodically triggers the device's own advert. Construct once and
// start with go Run(ctx); it blocks until ctx is done.
type Beacon struct {
	client Sender
	cfg    Config
}

// New returns a Beacon for the given client and config. Call Config.Validate
// before enabling; Run treats a zero period as "mode off".
func New(client Sender, cfg Config) *Beacon {
	return &Beacon{client: client, cfg: cfg}
}

// period is one enabled advert mode: flood or direct, on its own cadence.
type period struct {
	flood bool
	every time.Duration
}

// resolvePeriods maps the enabled config values onto advert periods: the
// hours field drives flood adverts, the minutes field drives direct ones.
func resolvePeriods(cfg Config) []period {
	var periods []period
	if h := cfg.FloodIntervalHours; h > 0 {
		periods = append(periods, period{flood: true, every: time.Duration(h) * time.Hour})
	}
	if m := cfg.DirectIntervalMin; m > 0 {
		periods = append(periods, period{flood: false, every: time.Duration(m) * time.Minute})
	}
	return periods
}

// Run runs the beacon's periods. Tickers fire first after one full period, so
// nothing is sent at startup.
func (b *Beacon) Run(ctx context.Context) {
	b.runPeriods(ctx, resolvePeriods(b.cfg))
}

// runPeriods blocks until ctx is done, emitting an advert per period on each
// period's ticker.
func (b *Beacon) runPeriods(ctx context.Context, periods []period) {
	var wg sync.WaitGroup
	for _, p := range periods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(p.every)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					b.send(p.flood)
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	wg.Wait()
}

// send emits one advert. A disconnected device is a skip, not an error: the
// periodic-stats goroutine in main owns reconnection.
func (b *Beacon) send(flood bool) {
	if !b.client.IsConnected() {
		log().Warn("Advert skipped, device disconnected", "mode", mode(flood))
		return
	}
	if err := b.client.SendSelfAdvert(flood); err != nil {
		log().Error("Advert send failed", "mode", mode(flood), "error", err)
		return
	}
	log().Info("Advert sent", "mode", mode(flood))
}

func mode(flood bool) string {
	if flood {
		return "flood"
	}
	return "direct"
}
