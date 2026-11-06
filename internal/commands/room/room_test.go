package room

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stempit/meshcore-go"
	"github.com/stempit/meshcore-go-bot/internal/config"
	"github.com/stempit/meshcore-go-bot/internal/support/registry"
)

// TestMain loads a minimal config so pushPost's shrink.Fit has limits to read
// (same approach as the shrink package tests; config.Load is once-guarded and
// the real bot loads it in main before anything runs).
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "roomcfg")
	path := filepath.Join(dir, "config.yaml")
	yaml := `
Shrink:
  default: truncate
  max_bytes: 140
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		panic(err)
	}
	if err := config.Load(path); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// aliceKey/bobKey are raw public-key byte strings as the DM path delivers them
// (Ctx.SenderKey carries the bytes, not hex).
var (
	aliceKey = string([]byte{0xaa, 0x01, 0x02, 0x03, 0x04, 0x05})
	bobKey   = string([]byte{0xbb, 0x01, 0x02, 0x03, 0x04, 0x05})
)

type push struct {
	raw  string
	text string
}

// testEnv redirects the store dir to a temp dir, resets the config to the
// normalized implicit single room ("main"), and captures pushed DMs; it
// returns a push-sink reader to install per test.
func testEnv(t *testing.T) func() []push {
	t.Helper()
	dir := t.TempDir()
	oldDir, oldStores := roomDir, stores
	conf0 := conf
	roomDir, stores = dir, map[string]*store{}
	conf = cfg{}
	if err := normalize(&conf); err != nil {
		t.Fatalf("normalize zero config: %v", err)
	}
	t.Cleanup(func() {
		roomDir, stores = oldDir, oldStores
		conf = conf0
	})

	var pushes []push
	oldLookup, oldSend := lookupContact, sendDM
	lookupContact = func(rawKey string) *meshcore.Contact {
		return &meshcore.Contact{PublicKey: []byte(rawKey), Name: "c-" + rawKey[:1]}
	}
	sendDM = func(contact *meshcore.Contact, text string, _ bool) {
		pushes = append(pushes, push{raw: string(contact.PublicKey), text: text})
	}
	t.Cleanup(func() { lookupContact, sendDM = oldLookup, oldSend })
	return func() []push { return pushes }
}

// setConf mutates the test config in place and re-runs the validation the
// startup Configure path applies.
func setConf(t *testing.T, mutate func(*cfg)) {
	t.Helper()
	mutate(&conf)
	if err := normalize(&conf); err != nil {
		t.Fatalf("normalize config: %v", err)
	}
}

// dmCtx builds the Ctx registry.Dispatch would hand Handle for a DM, with Rest
// pre-filled the way restAfterTrigger fills it for the ".room" trigger.
func dmCtx(text, rawKey, name string, ts time.Time) registry.Ctx {
	ctx := registry.NewCtx(text, "", rawKey, name, &registry.PingMeta{Timestamp: ts}, nil)
	const trig = ".room"
	if len(ctx.Trimmed) >= len(trig) {
		ctx.Rest = strings.TrimSpace(ctx.Trimmed[len(trig):])
	}
	return ctx
}

func send(t *testing.T, text, rawKey, name string, ts time.Time) []string {
	t.Helper()
	replies, err := plugin{}.Handle(dmCtx(text, rawKey, name, ts))
	if err != nil {
		t.Fatalf("Handle(%q) error: %v", text, err)
	}
	return replies
}

func TestSendPushesToSubscribersOnly(t *testing.T) {
	sink := testEnv(t)

	send(t, ".room on", bobKey, "bob", time.Unix(1000, 0))
	send(t, ".room on", aliceKey, "alice", time.Unix(1001, 0)) // subscribed too
	replies := send(t, ".room send hello room", aliceKey, "alice", time.Unix(1002, 0))

	if len(replies) != 1 || !strings.Contains(replies[0], "[1]") {
		t.Fatalf("send reply = %v, want confirmation with seq 1", replies)
	}
	got := sink()
	if len(got) != 1 { // bob only: the author must not be self-pushed
		t.Fatalf("pushes = %v, want exactly one (to bob)", got)
	}
	if !strings.Contains(got[0].text, "alice") || !strings.Contains(got[0].text, "hello room") {
		t.Fatalf("pushed text = %q, want author + post text", got[0].text)
	}
	if strings.HasPrefix(got[0].text, "[main]") || strings.HasPrefix(got[0].text, "[secret]") {
		t.Fatalf("default-room push = %q, want no room prefix", got[0].text)
	}
	if got[0].raw != rawKeyOf(keyOf(bobKey)) {
		t.Fatalf("pushed raw key = %q, want bob's raw key", got[0].raw)
	}
}

func TestMultiRoomIsolationAndPushPrefix(t *testing.T) {
	sink := testEnv(t)
	setConf(t, func(c *cfg) { c.Rooms = append(c.Rooms, roomDecl{Name: "secret"}) })

	// bob subscribes in the non-default room only: main unread accumulates,
	// secret posts are pushed with the room prefix.
	send(t, ".room secret on", bobKey, "bob", time.Unix(1000, 0))
	send(t, ".room send main post", aliceKey, "alice", time.Unix(1001, 0))
	send(t, ".room secret send secret post", aliceKey, "alice", time.Unix(1002, 0))

	got := sink()
	if len(got) != 1 {
		t.Fatalf("pushes = %v, want exactly one (secret to bob)", got)
	}
	if !strings.HasPrefix(got[0].text, "[secret] ") {
		t.Fatalf("non-default push = %q, want [secret] prefix", got[0].text)
	}
	if !strings.Contains(got[0].text, "secret post") {
		t.Fatalf("non-default push = %q, want the post text", got[0].text)
	}

	// get is room-scoped: main still unread, secret consumed by the push.
	replies := send(t, ".room get", bobKey, "bob", time.Unix(1003, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "main post") {
		t.Fatalf("main get = %v, want the main post", replies)
	}
	replies = send(t, ".room secret get", bobKey, "bob", time.Unix(1004, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "No unread") {
		t.Fatalf("secret get = %v, want consumed by push", replies)
	}

	// who is room-scoped: bob fetched main once (enrolled) but never
	// subscribed there, while secret shows him listening.
	replies = send(t, ".room who", aliceKey, "alice", time.Unix(1005, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "none listening") {
		t.Fatalf("main who = %v, want bob present but not listening", replies)
	}
	replies = send(t, ".room secret who", aliceKey, "alice", time.Unix(1006, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "bob") {
		t.Fatalf("secret who = %v, want bob listening", replies)
	}

	// per-room store files on disk.
	for _, name := range []string{"main", "secret"} {
		if _, err := os.Stat(roomPath(name)); err != nil {
			t.Fatalf("store file %s missing: %v", name, err)
		}
	}
}

func TestRoomNameVsVerb(t *testing.T) {
	testEnv(t)
	setConf(t, func(c *cfg) { c.Rooms = append(c.Rooms, roomDecl{Name: "secret"}) })

	// verbs still own the first token: this posts to the default room.
	replies := send(t, ".room send first", aliceKey, "alice", time.Unix(2000, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "[1]") {
		t.Fatalf("verb-first routing = %v", replies)
	}

	// bare room name = status of THAT room: secret is empty while main has
	// the post above, so the empty-room invite proves the routing.
	replies = send(t, ".room secret", aliceKey, "alice", time.Unix(2001, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "empty") {
		t.Fatalf("room status = %v, want secret's empty invite", replies)
	}

	// confirmed send into a named room names it.
	replies = send(t, ".room secret send hello", aliceKey, "alice", time.Unix(2002, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "in secret") {
		t.Fatalf("named send = %v, want 'in secret' confirmation", replies)
	}

	// undeclared but valid name → explicit hint.
	replies = send(t, ".room lounge send hi", aliceKey, "alice", time.Unix(2003, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "No such room") {
		t.Fatalf("undeclared room = %v, want no-such-room", replies)
	}

	// garbage token → the generic unknown reply.
	replies = send(t, ".room !! send hi", aliceKey, "alice", time.Unix(2004, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "Unknown") {
		t.Fatalf("garbage token = %v, want unknown", replies)
	}

	// room lookup is case-insensitive.
	replies = send(t, ".room SECRET on", bobKey, "bob", time.Unix(2005, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "Subscribed") {
		t.Fatalf("uppercase room = %v, want subscribed", replies)
	}
}

func TestList(t *testing.T) {
	testEnv(t)
	setConf(t, func(c *cfg) { c.Rooms = append(c.Rooms, roomDecl{Name: "secret"}) })

	send(t, ".room send m1", aliceKey, "alice", time.Unix(3000, 0))
	send(t, ".room secret on", bobKey, "bob", time.Unix(3001, 0))
	send(t, ".room secret send s1", aliceKey, "alice", time.Unix(3002, 0))

	replies := send(t, ".room list", bobKey, "bob", time.Unix(3003, 0))
	if len(replies) != 3 {
		t.Fatalf("list = %v, want header + two rooms", replies)
	}
	if !strings.Contains(replies[1], "main") || !strings.Contains(replies[1], "1") {
		t.Fatalf("main line = %q, want one unread", replies[1])
	}
	if !strings.Contains(replies[2], "secret") || !strings.Contains(replies[2], "on") {
		t.Fatalf("secret line = %q, want subscribed", replies[2])
	}
}

func TestGetAdvancesCursorAndReportsMore(t *testing.T) {
	testEnv(t)

	// bob never subscribes: unread accumulates for him while alice posts.
	for i, ts := range []int64{2001, 2002, 2003} {
		send(t, ".room send m"+string(rune('a'+i)), aliceKey, "alice", time.Unix(ts, 0))
	}

	replies := send(t, ".room get 2", bobKey, "bob", time.Unix(3000, 0))
	if len(replies) != 3 { // two posts + the "+N more" hint
		t.Fatalf("get 2 replies = %v, want 2 posts + hint", replies)
	}
	if !strings.Contains(replies[2], "1") {
		t.Fatalf("hint = %q, want one remaining", replies[2])
	}

	replies = send(t, ".room get", bobKey, "bob", time.Unix(3001, 0))
	if len(replies) != 1 || strings.Contains(replies[0], "more") {
		t.Fatalf("get replies = %v, want the last post without hint", replies)
	}
	replies = send(t, ".room get", bobKey, "bob", time.Unix(3002, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "No unread") {
		t.Fatalf("drained get = %v, want no-unread", replies)
	}
}

func TestLastDoesNotTouchCursor(t *testing.T) {
	testEnv(t)

	send(t, ".room send one", aliceKey, "alice", time.Unix(4001, 0))
	send(t, ".room send two", aliceKey, "alice", time.Unix(4002, 0))

	replies := send(t, ".room last", bobKey, "bob", time.Unix(4003, 0))
	if len(replies) != 2 {
		t.Fatalf("last = %v, want both posts", replies)
	}
	replies = send(t, ".room get", bobKey, "bob", time.Unix(4004, 0))
	if len(replies) != 2 {
		t.Fatalf("get after last = %v, want both still unread", replies)
	}
}

func TestStatusOnOffWho(t *testing.T) {
	testEnv(t)

	replies := send(t, ".room", aliceKey, "alice", time.Unix(5000, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "empty") {
		t.Fatalf("status on empty room = %v", replies)
	}

	send(t, ".room send first", aliceKey, "alice", time.Unix(5001, 0))
	send(t, ".room on", bobKey, "bob", time.Unix(5002, 0))

	replies = send(t, ".room", bobKey, "bob", time.Unix(5003, 0))
	if len(replies) != 2 || !strings.Contains(replies[0], "on") || !strings.Contains(replies[0], "1") {
		t.Fatalf("status = %v, want subscribed, 1 unread", replies)
	}
	if !strings.Contains(replies[1], "first") {
		t.Fatalf("status preview = %q, want last post", replies[1])
	}

	replies = send(t, ".room off", bobKey, "bob", time.Unix(5004, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "Unsubscribed") {
		t.Fatalf("off = %v", replies)
	}

	send(t, ".room on", bobKey, "bob", time.Unix(5005, 0))
	replies = send(t, ".room who", aliceKey, "alice", time.Unix(5006, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "bob") {
		t.Fatalf("who = %v, want bob listening", replies)
	}
}

func TestGetSkipsOwnPostsAfterGap(t *testing.T) {
	testEnv(t)

	send(t, ".room send a1", aliceKey, "alice", time.Unix(9600, 0))
	send(t, ".room send b1", bobKey, "bob", time.Unix(9601, 0))
	send(t, ".room send b2", bobKey, "bob", time.Unix(9602, 0))
	// alice posts while having bob's two posts unread: the gap must survive.
	replies := send(t, ".room send a2", aliceKey, "alice", time.Unix(9603, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "[4]") {
		t.Fatalf("send = %v, want posted #4", replies)
	}

	replies = send(t, ".room get", aliceKey, "alice", time.Unix(9604, 0))
	if len(replies) != 2 {
		t.Fatalf("get = %v, want bob's two posts without alice's own", replies)
	}
	for _, r := range replies {
		if strings.Contains(r, "alice") || strings.Contains(r, "a2") {
			t.Fatalf("get leaked the author's own post: %q", r)
		}
	}
}

func TestGates(t *testing.T) {
	testEnv(t)

	replies, err := plugin{}.Handle(registry.NewCtx(".room send hi", "chan", aliceKey, "alice", nil, nil))
	if err != nil || len(replies) != 1 || !strings.Contains(replies[0], "DM") {
		t.Fatalf("channel use = %v, %v, want DM hint", replies, err)
	}
	replies = send(t, ".room send hi", "", "anon", time.Unix(6000, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "advert") {
		t.Fatalf("empty key = %v, want need-key", replies)
	}
	// "bogus" has a valid room-name shape → the specific no-such-room hint
	// (the generic Unknown reply is covered by TestRoomNameVsVerb).
	replies = send(t, ".room bogus verb", aliceKey, "alice", time.Unix(6001, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "No such room") {
		t.Fatalf("undeclared room = %v", replies)
	}
	replies = send(t, ".room send", aliceKey, "alice", time.Unix(6002, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "Usage") {
		t.Fatalf("send without text = %v", replies)
	}
}

func TestClosedRoomSilentToNonMembers(t *testing.T) {
	testEnv(t)
	setConf(t, func(c *cfg) {
		c.Rooms = []roomDecl{{Name: "main", Closed: true, Allow: []string{"aa01"}}}
	})

	replies := send(t, ".room send hi", aliceKey, "alice", time.Unix(7000, 0))
	if len(replies) != 1 || !strings.Contains(replies[0], "[1]") {
		t.Fatalf("member send = %v, want posted", replies)
	}
	replies, err := plugin{}.Handle(dmCtx(".room who", bobKey, "bob", time.Unix(7001, 0)))
	if err != nil || replies != nil {
		t.Fatalf("non-member = %v, %v, want silent", replies, err)
	}
}

func TestReplayGuard(t *testing.T) {
	testEnv(t)
	ts := time.Unix(8000, 0)

	first := send(t, ".room send once", aliceKey, "alice", ts)
	if len(first) != 1 {
		t.Fatalf("first send = %v", first)
	}
	second := send(t, ".room send once", aliceKey, "alice", ts)
	if second != nil {
		t.Fatalf("replayed send = %v, want silent skip", second)
	}
	replies := send(t, ".room last", aliceKey, "alice", time.Unix(8001, 0))
	if len(replies) != 1 {
		t.Fatalf("posts = %v, want exactly one stored", replies)
	}
}

func TestReplayGuardPerRoom(t *testing.T) {
	testEnv(t)
	setConf(t, func(c *cfg) { c.Rooms = append(c.Rooms, roomDecl{Name: "secret"}) })
	ts := time.Unix(8100, 0)

	send(t, ".room send once", aliceKey, "alice", ts)
	replay := send(t, ".room send once", aliceKey, "alice", ts)
	if replay != nil {
		t.Fatalf("replayed default-room send = %v, want silent skip", replay)
	}
	// The same sender+timestamp in another room is a different command and
	// must post normally.
	replies := send(t, ".room secret send once", aliceKey, "alice", ts)
	if len(replies) != 1 || !strings.Contains(replies[0], "[1]") {
		t.Fatalf("same id in another room = %v, want posted #1", replies)
	}
}

func TestPruneAndReload(t *testing.T) {
	testEnv(t)
	setConf(t, func(c *cfg) { c.MaxPosts = 3 })

	for i, ts := range []int64{9001, 9002, 9003, 9004, 9005} {
		send(t, ".room send p"+string(rune('a'+i)), aliceKey, "alice", time.Unix(ts, 0))
	}

	roomMu.Lock()
	s, err := loadStore(defaultRoom())
	roomMu.Unlock()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(s.st.Posts) != 3 || s.st.Posts[0].Text != "pc" || s.st.NextSeq != 6 {
		t.Fatalf("store after prune/reload = %d posts, oldest %q, next %d",
			len(s.st.Posts), s.st.Posts[0].Text, s.st.NextSeq)
	}
}

func TestBakRecovery(t *testing.T) {
	testEnv(t)

	// Two saves: save keeps the PREVIOUS generation as .bak, so after two
	// posts the .bak holds "one" while the main file holds "one"+"two".
	send(t, ".room send one", aliceKey, "alice", time.Unix(9500, 0))
	send(t, ".room send two", aliceKey, "alice", time.Unix(9501, 0))
	if err := os.WriteFile(roomPath(defaultRoom()), []byte("{corrupt"), 0o644); err != nil {
		t.Fatalf("corrupt main file: %v", err)
	}

	roomMu.Lock()
	delete(stores, defaultRoom())
	s, err := loadStore(defaultRoom())
	roomMu.Unlock()
	if err != nil {
		t.Fatalf("load after corrupt: %v", err)
	}
	if len(s.st.Posts) != 1 || s.st.Posts[0].Text != "one" {
		t.Fatalf("recovered posts = %+v, want .bak generation", s.st.Posts)
	}
}

func TestRoundTripJSON(t *testing.T) {
	testEnv(t)

	send(t, ".room on", bobKey, "bob", time.Unix(9700, 0))
	send(t, ".room send hi", aliceKey, "alice", time.Unix(9701, 0))

	data, err := os.ReadFile(roomPath(defaultRoom()))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var st roomState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("store is not valid JSON: %v", err)
	}
	if st.NextSeq != 2 || len(st.Posts) != 1 || !st.Users[keyOf(bobKey)].Subscribed {
		t.Fatalf("round-trip store = seq %d, %d posts, bob subscribed %v",
			st.NextSeq, len(st.Posts), st.Users[keyOf(bobKey)].Subscribed)
	}
}

func TestBatchArgCapsAndDefaults(t *testing.T) {
	testEnv(t)
	if got := batchArg("", getBatch()); got != getBatch() {
		t.Fatalf("batchArg(empty) = %d, want default", got)
	}
	if got := batchArg("5", getBatch()); got != 5 {
		t.Fatalf("batchArg(5) = %d, want 5", got)
	}
	if got := batchArg("bogus", getBatch()); got != getBatch() {
		t.Fatalf("batchArg(garbage) = %d, want default", got)
	}
	conf.MaxBatch = 7
	if got := batchArg("99", 99); got != 7 {
		t.Fatalf("batchArg over cap = %d, want capped to 7", got)
	}
}

func TestNormalize(t *testing.T) {
	// reserved word can never be a room name
	err := normalize(&cfg{Rooms: []roomDecl{{Name: "send"}}})
	if err == nil || !strings.Contains(err.Error(), "command word") {
		t.Fatalf("reserved name error = %v", err)
	}
	// duplicates rejected
	err = normalize(&cfg{Rooms: []roomDecl{{Name: "a"}, {Name: "a"}}})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate error = %v", err)
	}
	// closed needs allow keys
	err = normalize(&cfg{Rooms: []roomDecl{{Name: "a", Closed: true}}})
	if err == nil || !strings.Contains(err.Error(), "allow") {
		t.Fatalf("closed-without-allow error = %v", err)
	}
	// charset/length guard
	err = normalize(&cfg{Rooms: []roomDecl{{Name: "has space"}}})
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("bad charset error = %v", err)
	}
	// implicit default room appended, names normalized
	c := &cfg{Rooms: []roomDecl{{Name: " Secret "}}}
	if err := normalize(c); err != nil {
		t.Fatalf("normalize good config: %v", err)
	}
	if c.Default != "main" || len(c.Rooms) != 2 || c.Rooms[0].Name != "secret" || c.Rooms[1].Name != "main" {
		t.Fatalf("normalized config = default %q, rooms %+v", c.Default, c.Rooms)
	}
	// unlisted default appended too
	c = &cfg{Default: "lounge"}
	if err := normalize(c); err != nil {
		t.Fatalf("normalize lounge config: %v", err)
	}
	if len(c.Rooms) != 1 || c.Rooms[0].Name != "lounge" {
		t.Fatalf("lounge config rooms = %+v", c.Rooms)
	}
}

func TestDispatchRouting(t *testing.T) {
	testEnv(t)
	conf.Enabled = true

	// Dispatch needs the shared config (redirect gate); Handle-direct tests
	// above run with a nil Cfg.
	ctx := dmCtx(".room last", aliceKey, "alice", time.Unix(9900, 0))
	ctx.Cfg = config.Get()
	replies, name, err := registry.Dispatch(ctx)
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if name != "room" {
		t.Fatalf("Dispatch name = %q, want room", name)
	}
	if len(replies) != 1 || !strings.Contains(replies[0], "empty") {
		t.Fatalf("Dispatch replies = %v", replies)
	}
}

func TestHelpIntercept(t *testing.T) {
	testEnv(t)
	conf.Enabled = true

	// ".room help" must resolve through LocalizedHelp — the gate requires a
	// non-empty Meta.HelpLong first.
	ctx := dmCtx(".room help", aliceKey, "alice", time.Unix(9950, 0))
	ctx.Cfg = config.Get()
	replies, name, err := registry.Dispatch(ctx)
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if name != "room" || len(replies) == 0 || !strings.Contains(replies[0], ".room send") {
		t.Fatalf("help = %q, %v, %v — want localized long help", name, replies, err)
	}
}
