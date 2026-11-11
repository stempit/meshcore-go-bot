package beacon

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// stubSender records SendSelfAdvert calls and reports a fixed connection
// state. When sent is non-nil, every successful send delivers the flood flag
// on it so tests can await sends instead of sleeping.
type stubSender struct {
	mu        sync.Mutex
	calls     []bool
	connected bool
	sent      chan bool
}

func (s *stubSender) SendSelfAdvert(flood bool) error {
	s.mu.Lock()
	s.calls = append(s.calls, flood)
	s.mu.Unlock()
	if s.sent != nil {
		s.sent <- flood
	}
	return nil
}

func (s *stubSender) IsConnected() bool { return s.connected }

func (s *stubSender) sentFlags() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.calls...)
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"both modes", Config{Enabled: true, FloodIntervalHours: 6, DirectIntervalMin: 30}, false},
		{"flood only at minimum", Config{Enabled: true, FloodIntervalHours: 1}, false},
		{"direct only at minimum", Config{Enabled: true, DirectIntervalMin: 1}, false},
		{"enabled but nothing to send", Config{Enabled: true}, true},
		{"negative flood", Config{Enabled: true, FloodIntervalHours: -1, DirectIntervalMin: 30}, true},
		{"negative direct", Config{Enabled: true, FloodIntervalHours: 6, DirectIntervalMin: -5}, true},
		{"disabled always valid", Config{}, false},
		{"disabled with zero periods", Config{FloodIntervalHours: 0, DirectIntervalMin: 0}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate(%+v) error = %v, wantErr %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

func TestSendConnected(t *testing.T) {
	s := &stubSender{connected: true}
	b := New(s, Config{Enabled: true})

	b.send(true)
	b.send(false)

	flags := s.sentFlags()
	if len(flags) != 2 || flags[0] != true || flags[1] != false {
		t.Errorf("sent flags = %v, want [true false]", flags)
	}
}

func TestSendDisconnectedSkips(t *testing.T) {
	s := &stubSender{connected: false}
	b := New(s, Config{Enabled: true})

	b.send(true)

	if flags := s.sentFlags(); len(flags) != 0 {
		t.Errorf("sent flags = %v, want none while disconnected", flags)
	}
}

func TestResolvePeriods(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want []period
	}{
		{"both modes", Config{FloodIntervalHours: 6, DirectIntervalMin: 30},
			[]period{{flood: true, every: 6 * time.Hour}, {flood: false, every: 30 * time.Minute}}},
		{"flood only", Config{FloodIntervalHours: 1},
			[]period{{flood: true, every: 1 * time.Hour}}},
		{"direct only", Config{DirectIntervalMin: 1},
			[]period{{flood: false, every: 1 * time.Minute}}},
		{"nothing enabled", Config{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePeriods(tt.cfg)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("resolvePeriods(%+v) = %v, want %v", tt.cfg, got, tt.want)
			}
		})
	}
}

func TestStrictIntDecode(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Config
		wantErr bool
	}{
		{"whole numbers", "enabled: true\nflood_interval_hours: 6\ndirect_interval_min: 30\n",
			Config{Enabled: true, FloodIntervalHours: 6, DirectIntervalMin: 30}, false},
		{"negative numbers decode, Validate rejects", "enabled: true\nflood_interval_hours: -1\n",
			Config{Enabled: true, FloodIntervalHours: -1}, false},
		{"fractional rejected, not truncated", "enabled: true\nflood_interval_hours: 0.5\n",
			Config{}, true},
		{"fractional above one rejected too", "enabled: true\ndirect_interval_min: 6.9\n",
			Config{}, true},
		{"string rejected", "enabled: true\nflood_interval_hours: \"6\"\n",
			Config{}, true},
		{"non-numeric rejected", "enabled: true\ndirect_interval_min: abc\n",
			Config{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Config
			err := yaml.Unmarshal([]byte(tt.in), &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Unmarshal(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("Unmarshal(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRunFiresBothModesAndStops(t *testing.T) {
	s := &stubSender{connected: true, sent: make(chan bool, 8)}
	b := New(s, Config{Enabled: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		// Tiny periods constructed directly: Run's minimum is 1h, but the
		// scheduling logic under test is period-independent.
		b.runPeriods(ctx, []period{{flood: true, every: 20 * time.Millisecond}, {flood: false, every: 20 * time.Millisecond}})
		close(done)
	}()

	got := map[bool]int{}
	deadline := time.After(5 * time.Second)
	for got[true] == 0 || got[false] == 0 {
		select {
		case flood := <-s.sent:
			got[flood]++
		case <-deadline:
			t.Fatalf("timed out waiting for both modes; got %v", got)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPeriods did not return after ctx cancel")
	}
}
