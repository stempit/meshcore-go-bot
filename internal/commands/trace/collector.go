// Collector: ".trace full" support. The firmware pushes a 0x88 raw frame for
// every RF reception — pre-dedup. meshcore-go correlates only the first copy
// with the decoded frame (that is what PingMeta shows); the Collector buffers
// every copy from the OnRawData callback, so ".trace full" can enumerate all
// routes. Flood-routed receptions accumulate a hash per forwarder — one
// distinct path per chain. Direct-routed receptions carry an explicit path
// that each forwarder consumes (strips its own hash) — the copies are stages
// of one delivery, [A,B] → [B] → [], and collapse into a single entry.
package trace

import (
	"bytes"
	"crypto/aes"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stempit/meshcore-go"
)

// Raw packet payload and route types the collector distinguishes (Packet.h
// constants; same values mqttbridge/packet_wire.go documents for the
// analyzer).
const (
	payloadTypeTxtMsg = 0x02 // PAYLOAD_TYPE_TXT_MSG — direct message
	payloadTypeGrpTxt = 0x05 // PAYLOAD_TYPE_GRP_TXT — channel message

	routeTypeFlood          = 0x01 // ROUTE_TYPE_FLOOD — hash accumulates per forwarder
	routeTypeDirect         = 0x02 // ROUTE_TYPE_DIRECT — explicit path
	routeTypeTransportFlood = 0x00 // ROUTE_TYPE_TRANSPORT_FLOOD
	routeTypeTransportDir   = 0x03 // ROUTE_TYPE_TRANSPORT_DIRECT — explicit path + codes
)

const (
	// bufferTTL is how far back the look-back buffer reaches. Copy #1 always
	// precedes the decoded frame by milliseconds, but the worker pool may
	// delay the command handler by a few seconds under load, so the buffer
	// must span more than the correlation queue's 2s TTL.
	bufferTTL = 20 * time.Second
	// bufferCap bounds memory: one reception ≈ one packet ≤ ~200 bytes.
	bufferCap = 64
	// collectTick is the poll interval while a collection window is open.
	collectTick = 150 * time.Millisecond
)

// RawSource is the slice of the meshcore client the collector needs. It is an
// interface so tests can inject a stub instead of a live client.
type RawSource interface {
	GetSelfInfo() *meshcore.SelfInfo
	GetChannelByName(name string) *meshcore.Channel
	GetContacts() map[string]*meshcore.Contact
}

// Collector records raw RF receptions in a short rolling buffer and replays
// the ones matching a given message. Copy #1 always precedes the decoded
// frame, so Collect first scans the look-back buffer, then gathers live
// arrivals until the window closes.
type Collector struct {
	client RawSource

	mu         sync.Mutex
	buf        []meshcore.RawRx
	collecting bool
}

// NewCollector builds a collector over the given client source.
func NewCollector(src RawSource) *Collector {
	return &Collector{client: src}
}

// Observe is the OnRawData callback: park the reception in the buffer. It runs
// on the frame-handler goroutine, so it only locks, appends, and trims.
func (c *Collector) Observe(rx meshcore.RawRx) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, rx)
	// Trim from the front: drop entries past the cap and older than the TTL.
	cut := 0
	for cut < len(c.buf) && (len(c.buf)-cut > bufferCap || time.Since(c.buf[cut].At) > bufferTTL) {
		cut++
	}
	c.buf = c.buf[cut:]
}

// PathEntry is one distinct route the message was heard on.
type PathEntry struct {
	Path      []byte       // accumulated path hash bytes as heard on air
	Hops      int          // hop count; 0 = direct (no repeaters)
	HopCoords [][2]float64 // per-hop lat/lon, zero value when unknown
	Known     []bool       // HopCoords[i] is a real position when Known[i]
	Names     []string     // per-hop repeater name from contacts; "" when unknown
	// Delay is how long after the first heard copy this route's earliest copy
	// arrived — the flood propagation time through the route. 0 for the
	// baseline copy itself. The sender's own RTC is deliberately not used as
	// the baseline: device clock skew would poison absolute comparisons.
	Delay time.Duration
}

// Match reports whether a raw packet is a copy of the message being traced.
// It receives the ParseRawPacket split so DM and channel rules differ freely.
type Match func(ptype byte, path []byte, hops int, payload []byte) bool

// Collect returns every distinct route the message was heard on: matched
// receptions already in the look-back buffer plus live arrivals for the
// window's remainder. Route families behave differently:
//
//   - Direct-routed copies (explicit path) are stages of one delivery —
//     MeshCore consumes the path hop by hop, each forwarder stripping its own
//     hash, so [A,B] → [B] → [] is one route heard once per hop, not three.
//     They collapse into a single entry showing the route as sent; Delay is
//     the end-to-end delivery time (final stage − first stage).
//   - Flood-routed copies accumulate hashes per forwarder, so each distinct
//     path is a genuinely different route; Delay is that route's arrival
//     relative to the earliest flood copy.
//
// Entries come back sorted by hop count. busy is true (with no entries) when
// another collection is already running — Handle replies "busy" instead of
// stacking a second window-length hold on the worker pool.
func (c *Collector) Collect(match Match, window time.Duration) (entries []PathEntry, busy bool) {
	c.mu.Lock()
	if c.collecting {
		c.mu.Unlock()
		return nil, true
	}
	c.collecting = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.collecting = false
		c.mu.Unlock()
	}()

	// Contacts change rarely; resolve coordinates against one snapshot per
	// collection instead of per match, and always outside the buffer lock so
	// Observe (frame-handler goroutine) never waits behind contact lookups.
	var contacts map[string]*meshcore.Contact
	if c.client != nil {
		contacts = c.client.GetContacts()
	}

	// Gather every matched reception whole — grouping by route family happens
	// once after the window, not per tick. The buffer is re-scanned each tick,
	// so receptions dedup by their exact identity (the stored arrival time is
	// fixed per buffer entry): only genuinely new receptions get recorded.
	type rxKey struct {
		route byte
		path  string
		at    time.Time
	}
	seen := make(map[rxKey]bool)
	var copies []rxCopy
	deadline := time.Now().Add(window)
	for {
		c.mu.Lock()
		for _, rx := range c.buf {
			ptype, path, hops, payload, ok := meshcore.ParseRawPacket(rx.Data)
			if !ok || !match(ptype, path, hops, payload) {
				continue
			}
			route := rx.Data[0] & 0x03
			key := rxKey{route: route, path: string(path), at: rx.At}
			if seen[key] {
				continue
			}
			seen[key] = true
			copies = append(copies, rxCopy{
				route: route,
				path:  append([]byte(nil), path...),
				hops:  hops,
				at:    rx.At,
			})
		}
		c.mu.Unlock()

		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(collectTick)
	}

	found := groupCopies(copies)
	for i := range found {
		found[i].HopCoords, found[i].Known, found[i].Names = resolveHops(found[i].Path, found[i].Hops, contacts)
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Hops < found[j].Hops })
	return found, false
}

// rxCopy is one matched raw reception, kept whole until the window closes so
// direct-route stages can be grouped into their delivery chain.
type rxCopy struct {
	route byte
	path  []byte
	hops  int
	at    time.Time
}

// directChain accumulates the heard stages of one direct-routed delivery.
type directChain struct {
	full      []byte    // longest heard path = the route as sent
	hops      int       // hash count of the full path
	minHops   int       // shallowest heard stage (0 = final delivery)
	first     time.Time // earliest stage arrival
	delivered time.Time // earliest arrival of the shallowest stage
}

// groupCopies splits receptions by route family and builds the report entries:
// one per direct-route chain, one per distinct flood path.
func groupCopies(copies []rxCopy) []PathEntry {
	var found []PathEntry

	// Direct-routed: stages of the same chain share suffix paths — each
	// forwarder strips its own hash, so a stage's path is a suffix of the
	// route as sent. Longest-first ordering makes the full route found first.
	var direct []rxCopy
	var flood []rxCopy
	for _, cp := range copies {
		if cp.route == routeTypeDirect || cp.route == routeTypeTransportDir {
			direct = append(direct, cp)
		} else {
			flood = append(flood, cp)
		}
	}
	sort.SliceStable(direct, func(i, j int) bool {
		if direct[i].hops != direct[j].hops {
			return direct[i].hops > direct[j].hops
		}
		return direct[i].at.Before(direct[j].at)
	})
	var chains []*directChain
	for _, cp := range direct {
		joined := false
		for _, ch := range chains {
			if !bytes.HasSuffix(ch.full, cp.path) {
				continue
			}
			joined = true
			if cp.at.Before(ch.first) {
				ch.first = cp.at
			}
			if cp.hops < ch.minHops {
				ch.minHops, ch.delivered = cp.hops, cp.at
			} else if cp.hops == ch.minHops && cp.at.Before(ch.delivered) {
				ch.delivered = cp.at
			}
			break
		}
		if !joined {
			chains = append(chains, &directChain{full: cp.path, hops: cp.hops, minHops: cp.hops, first: cp.at, delivered: cp.at})
		}
	}
	for _, ch := range chains {
		found = append(found, PathEntry{Path: ch.full, Hops: ch.hops, Delay: ch.delivered.Sub(ch.first)})
	}

	// Flood-routed: each distinct path is a route of its own; keep the
	// earliest arrival per path and measure delays from the earliest flood
	// copy (the flood family's own baseline).
	if len(flood) > 0 {
		floodStart := len(found)
		seen := make(map[string]int) // path key -> index into arrivals
		var arrivals []time.Time
		for _, cp := range flood {
			key := string(cp.path) // empty key collapses all in-range copies into one
			if idx, dup := seen[key]; dup {
				if cp.at.Before(arrivals[idx]) {
					arrivals[idx] = cp.at
				}
				continue
			}
			seen[key] = len(arrivals)
			found = append(found, PathEntry{Path: cp.path, Hops: cp.hops})
			arrivals = append(arrivals, cp.at)
		}
		baseline := slices.MinFunc(arrivals, time.Time.Compare)
		for i := range arrivals {
			found[floodStart+i].Delay = arrivals[i].Sub(baseline)
		}
	}

	return found
}

// resolveHops resolves each hop of a path against the contact list — lat/lon
// so the path can carry its own distance, and the repeater's name for the
// ".trace full" lines. The prefix-match + GPS logic mirrors
// dispatch.resolveHopCoords, generalized to 1–4 bytes per hop. Name is "" for
// hops that are not in contacts (the line falls back to the key hash).
func resolveHops(path []byte, hops int, contacts map[string]*meshcore.Contact) (coord [][2]float64, known []bool, names []string) {
	if hops <= 0 || len(path) == 0 || len(path)%hops != 0 {
		return nil, nil, nil
	}
	bph := len(path) / hops
	coord = make([][2]float64, hops)
	known = make([]bool, hops)
	names = make([]string, hops)
	for i := 0; i < hops; i++ {
		chunk := path[i*bph : (i+1)*bph]
		for _, ct := range contacts {
			if len(ct.PublicKey) >= bph && bytes.Equal(ct.PublicKey[:bph], chunk) {
				names[i] = ct.Name
				if ct.Latitude != nil && ct.Longitude != nil {
					coord[i] = [2]float64{*ct.Latitude, *ct.Longitude}
					known[i] = true
				}
				break
			}
		}
	}
	return coord, known, names
}

// SelfKeyByte returns the first byte of the device's public key — the dest
// hash the firmware writes into every TXT_MSG packet addressed to us.
func (c *Collector) SelfKeyByte() (byte, bool) {
	info := c.client.GetSelfInfo()
	if info == nil || len(info.PublicKey) == 0 {
		return 0, false
	}
	return info.PublicKey[0], true
}

// ChannelPSK returns the PSK of the named channel; nil when the channel is
// unknown. Channel ciphertext decrypts with PSK[:16] (AES-128-ECB).
func (c *Collector) ChannelPSK(name string) []byte {
	ch := c.client.GetChannelByName(name)
	if ch == nil {
		return nil
	}
	return ch.PSK
}

// DMMatch builds a Match for the TXT_MSG copies of a direct message. TXT_MSG
// payload layout: [dest_hash(1)][src_hash(1)][mac(2)][ciphertext…], where
// dest_hash is our own key's first byte and src_hash the sender's. The
// ciphertext length must equal the padded plaintext — [ts(4)][flags(1)][text]
// for plain DMs, or [ts(4)][flags(1)][sender_prefix(4)][text] for signed ones
// (the +4 mirrors the signed extra the companion frame carries, frames.go
// parseContactMessageV3) — which filters unrelated traffic from the same
// sender. Known limitation: a second DM from the same sender inside the
// window also matches, which is benign (a different message from the same
// source travels the same routes).
func DMMatch(selfKey0, senderKey0 byte, textLen int) Match {
	plain := pad16(5 + textLen)
	signed := pad16(9 + textLen)
	return func(ptype byte, _ []byte, _ int, payload []byte) bool {
		// payload: [dest_hash(1)][src_hash(1)][mac(2)][ciphertext…]
		if ptype != payloadTypeTxtMsg || len(payload) < 4 {
			return false
		}
		if payload[0] != selfKey0 || payload[1] != senderKey0 {
			return false
		}
		ct := len(payload) - 4
		return ct == plain || ct == signed
	}
}

// ChannelMatch builds a Match for the GRP_TXT copies of a channel message.
// GRP_TXT payload layout: [channel_hash(1)][mac(2)][ciphertext…], AES-128-ECB
// with the channel PSK, plaintext [ts(4 LE)][flags(1)][sender: text]. The
// channel hash prefilter rejects other channels' traffic cheaply; the
// decryption then matches the exact message (sender timestamp + text), so
// unrelated messages on the same channel inside the window are filtered out.
func ChannelMatch(psk []byte, ts uint32, senderName, text string) Match {
	if len(psk) < 16 {
		// No usable PSK: nothing can be correlated. buildMatch never calls
		// this without one; the guard keeps the matcher honest anyway.
		return func(byte, []byte, int, []byte) bool { return false }
	}
	sum := sha256.Sum256(psk[:16])
	chanHash := sum[0]
	want := senderName + ": " + text
	return func(ptype byte, _ []byte, _ int, payload []byte) bool {
		if ptype != payloadTypeGrpTxt || len(payload) < 3 || payload[0] != chanHash {
			return false
		}
		ct := payload[3:]
		if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
			return false
		}
		plain := aesECBDecrypt(psk[:16], ct)
		if len(plain) < 5 || binary.LittleEndian.Uint32(plain[:4]) != ts {
			return false
		}
		// Prefix match so non-zero firmware padding cannot reject a true copy.
		return strings.HasPrefix(string(plain[5:]), want)
	}
}

// aesECBDecrypt decrypts whole blocks with AES-128-ECB — the receive-side
// counterpart of meshcore-go's buildGRPTXTPacket encryption.
func aesECBDecrypt(key, ct []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil
	}
	pt := make([]byte, len(ct))
	for i := 0; i < len(ct); i += aes.BlockSize {
		block.Decrypt(pt[i:i+aes.BlockSize], ct[i:i+aes.BlockSize])
	}
	return pt
}

// pad16 returns the AES-padded length of a plaintext of n bytes.
func pad16(n int) int {
	if r := n % aes.BlockSize; r != 0 {
		return n + aes.BlockSize - r
	}
	return n
}
