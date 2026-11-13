package trace

import (
	"crypto/aes"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stempit/meshcore-go"
)

// stubSource is a RawSource over fixed self info, channels and contacts.
type stubSource struct {
	self     *meshcore.SelfInfo
	channels map[string]*meshcore.Channel
	contacts map[string]*meshcore.Contact
}

func (s stubSource) GetSelfInfo() *meshcore.SelfInfo             { return s.self }
func (s stubSource) GetChannelByName(n string) *meshcore.Channel { return s.channels[n] }
func (s stubSource) GetContacts() map[string]*meshcore.Contact   { return s.contacts }

// buildTXTMSG builds a raw TXT_MSG RF packet: [header=direct|TXT_MSG][path_len]
// [path…][dest_hash][src_hash][mac(2)][ciphertext…]. The DM matcher only sees
// the hashes and the ciphertext length, so the ciphertext is filler.
func buildTXTMSG(dest, src byte, path []byte, ctLen int) []byte {
	raw := []byte{0x0A, byte(len(path))}
	raw = append(raw, path...)
	raw = append(raw, dest, src, 0x01, 0x02)
	return append(raw, make([]byte, ctLen)...)
}

// buildGRP builds a real GRP_TXT RF packet the way the firmware does
// (meshcore-go's buildGRPTXTPacket): AES-128-ECB over
// [ts(4 LE)][flags(1)][sender: text] with the channel PSK, payload
// [channel_hash][mac(2)][ciphertext…], flood header, given path.
func buildGRP(psk []byte, ts uint32, msg string, path []byte) []byte {
	plain := make([]byte, 5+len(msg))
	binary.LittleEndian.PutUint32(plain[0:4], ts)
	plain[4] = 0x00
	copy(plain[5:], msg)
	if pad := aes.BlockSize - (len(plain) % aes.BlockSize); pad != aes.BlockSize {
		plain = append(plain, make([]byte, pad)...)
	}
	block, err := aes.NewCipher(psk[:16])
	if err != nil {
		panic(err)
	}
	ct := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aes.BlockSize {
		block.Encrypt(ct[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}
	sum := sha256.Sum256(psk[:16])
	raw := []byte{0x15, byte(len(path))}
	raw = append(raw, path...)
	raw = append(raw, sum[0], 0x01, 0x02)
	return append(raw, ct...)
}

// TestCollectDMPathsAcrossCopies pins the DM collection rule: every TXT_MSG
// copy addressed to us by the traced sender is collected, duplicates of the
// same path collapse, direct copies collapse into one 0-hop entry, and
// unrelated traffic (other sender, other dest, wrong length) is filtered.
func TestCollectDMPathsAcrossCopies(t *testing.T) {
	textLen := len(".trace full")
	ct := 16                     // pad16(5 + 11): plain DM plaintext [ts][flags][text]
	signed := pad16(9 + textLen) // pad16(20): signed plaintext adds sender_prefix(4)
	src := stubSource{
		self: &meshcore.SelfInfo{PublicKey: []byte{0x42}},
		contacts: map[string]*meshcore.Contact{
			"aa": {PublicKey: []byte{0xAA}, Latitude: f64(1), Longitude: f64(0)},
			"bb": {PublicKey: []byte{0xBB}, Latitude: f64(2), Longitude: f64(0)},
		},
	}
	col := NewCollector(src)

	// Explicit arrival times pin the delay computation: the [AA] copy is the
	// earliest (baseline), later routes show their propagation delay, and a
	// duplicate [AA] copy does not move the baseline.
	base := time.Now()
	col.Observe(meshcore.RawRx{Data: buildTXTMSG(0x42, 0x11, []byte{0xAA}, ct), At: base})
	col.Observe(meshcore.RawRx{Data: buildTXTMSG(0x42, 0x11, []byte{0xAA, 0xBB}, ct), At: base.Add(time.Second)})
	col.Observe(meshcore.RawRx{Data: buildTXTMSG(0x42, 0x11, []byte{0xAA}, ct), At: base.Add(300 * time.Millisecond)})      // duplicate path
	col.Observe(meshcore.RawRx{Data: buildTXTMSG(0x42, 0x11, nil, ct), At: base.Add(2 * time.Second)})                      // direct copy
	col.Observe(meshcore.RawRx{Data: buildTXTMSG(0x42, 0x99, []byte{0xCC}, ct), At: base})                                  // other sender
	col.Observe(meshcore.RawRx{Data: buildTXTMSG(0x77, 0x11, []byte{0xDD}, ct), At: base})                                  // other dest
	col.Observe(meshcore.RawRx{Data: buildTXTMSG(0x42, 0x11, []byte{0xEE}, 48), At: base})                                  // wrong ct length (neither plain nor signed)
	col.Observe(meshcore.RawRx{Data: buildTXTMSG(0x42, 0x11, []byte{0xFF}, signed), At: base.Add(1500 * time.Millisecond)}) // signed-DM length (ts+flags+sender_prefix+text)
	col.Observe(meshcore.RawRx{Data: buildGRP(make([]byte, 16), 1, "Alice: hi", []byte{0x11}), At: base})                   // not a DM

	entries, busy := col.Collect(DMMatch(0x42, 0x11, textLen), 300*time.Millisecond)
	if busy {
		t.Fatal("unexpected busy")
	}
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(entries), entries)
	}
	// Sorted by hops: direct, then the one-hop routes in arrival order, then
	// the two-hop route. Delays are relative to the earliest arrival ([AA]).
	if entries[0].Hops != 0 || len(entries[0].Path) != 0 || entries[0].Delay != 2*time.Second {
		t.Errorf("entry 0 = %+v, want the direct entry with 2s delay", entries[0])
	}
	if string(entries[1].Path) != "\xaa" || entries[1].Hops != 1 || entries[1].Delay != 0 {
		t.Errorf("entry 1 = %+v, want the [AA] route as the 0-delay baseline", entries[1])
	}
	if len(entries[1].HopCoords) != 1 || !entries[1].Known[0] || entries[1].HopCoords[0] != [2]float64{1, 0} {
		t.Errorf("entry 1 coords = %+v / %+v, want R1's position", entries[1].HopCoords, entries[1].Known)
	}
	if string(entries[2].Path) != "\xff" || entries[2].Hops != 1 || entries[2].Delay != 1500*time.Millisecond {
		t.Errorf("entry 2 = %+v, want the signed-DM [FF] route with 1.5s delay", entries[2])
	}
	if string(entries[3].Path) != "\xaa\xbb" || entries[3].Hops != 2 || entries[3].Delay != time.Second {
		t.Errorf("entry 3 = %+v, want the [AA][BB] route with 1s delay", entries[3])
	}
}

// TestCollectChannelDecryptMatch pins the channel collection rule: only copies
// of the exact message (same channel, same timestamp, same text) match;
// another message on the same channel and another channel's traffic do not.
func TestCollectChannelDecryptMatch(t *testing.T) {
	psk := make([]byte, 16)
	psk[0] = 0x2A
	other := make([]byte, 16)
	other[0] = 0x7E
	const ts = uint32(1700000000)
	src := stubSource{channels: map[string]*meshcore.Channel{
		"chat": {Index: 3, Name: "chat", PSK: psk},
	}}
	col := NewCollector(src)

	base := time.Now()
	col.Observe(meshcore.RawRx{Data: buildGRP(psk, ts, "Alice: .trace full", []byte{0xAA}), At: base})
	col.Observe(meshcore.RawRx{Data: buildGRP(psk, ts, "Alice: .trace full", []byte{0xAA, 0xBB}), At: base.Add(2 * time.Second)})
	col.Observe(meshcore.RawRx{Data: buildGRP(psk, ts+5, "Alice: later hello", []byte{0xCC}), At: base})
	col.Observe(meshcore.RawRx{Data: buildGRP(other, ts, "Alice: .trace full", []byte{0xDD}), At: base})

	entries, busy := col.Collect(ChannelMatch(psk, ts, "Alice", ".trace full"), 300*time.Millisecond)
	if busy {
		t.Fatal("unexpected busy")
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	if string(entries[0].Path) != "\xaa" || entries[0].Delay != 0 {
		t.Fatalf("entry 0 = %+v, want the [AA] baseline route", entries[0])
	}
	if string(entries[1].Path) != "\xaa\xbb" || entries[1].Delay != 2*time.Second {
		t.Fatalf("entry 1 = %+v, want the [AA][BB] route with 2s delay", entries[1])
	}
}

// TestCollectIgnoresStaleReceptions pins the look-back rule: receptions older
// than the buffer TTL are dropped, so a previous message's burst cannot bleed
// into the next collection.
func TestCollectIgnoresStaleReceptions(t *testing.T) {
	col := NewCollector(stubSource{self: &meshcore.SelfInfo{PublicKey: []byte{0x42}}})
	col.Observe(meshcore.RawRx{Data: buildTXTMSG(0x42, 0x11, []byte{0xAA}, 16), At: time.Now().Add(-25 * time.Second)})
	col.Observe(meshcore.RawRx{Data: buildTXTMSG(0x42, 0x11, []byte{0xBB}, 16), At: time.Now()})

	entries, _ := col.Collect(DMMatch(0x42, 0x11, len(".trace full")), 300*time.Millisecond)
	if len(entries) != 1 || string(entries[0].Path) != "\xbb" {
		t.Fatalf("entries = %+v, want only the fresh [BB] copy", entries)
	}
}

// TestCollectBusyGuard pins the single-collection rule: a second Collect while
// one window is open reports busy instead of stacking a second hold.
func TestCollectBusyGuard(t *testing.T) {
	col := NewCollector(stubSource{self: &meshcore.SelfInfo{PublicKey: []byte{0x42}}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, busy := col.Collect(func(byte, []byte, int, []byte) bool { return false }, time.Second)
		if busy {
			t.Error("first Collect must not see itself as busy")
		}
	}()
	time.Sleep(100 * time.Millisecond)
	entries, busy := col.Collect(func(byte, []byte, int, []byte) bool { return false }, 300*time.Millisecond)
	if !busy || entries != nil {
		t.Fatalf("second Collect during an open window: entries=%v busy=%v, want busy", entries, busy)
	}
	<-done
}

func f64(v float64) *float64 { return &v }
