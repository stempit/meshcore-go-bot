# meshcore-go-bot

A MeshCore mesh-radio bot. Bridges a MeshCore device to Telegram, MQTT brokers, RSS feeds, and an LLM. Responds to `.ping`, `.trace`, `.last`, `.llm`, and `.news` commands over mesh radio.

## Build & run

```bash
go build ./...                    # build everything
go build ./cmd/bot                # build the main bot

go run ./cmd/bot                  # run the bot (reads config.yaml in CWD)

go vet ./...                      # static analysis
go test ./...                     # unit tests (shrink; driver/runtime tests
                                  # live in the meshcore-go and
                                  # meshcore-kit repos, voice-bridge
                                  # tests in meshcore-mcp-gateway)
go test -tags manual ./internal/commands/llm   # manual LLM prompt/model harness
                                               # (real API calls, not part of go test ./...)
```

Manual radio testing uses the `examples/basic` harness in the
[meshcore-go](https://github.com/stempit/meshcore-go) repo; manual LLM
prompt/model testing uses the `-tags manual` harness in
`internal/commands/llm`. The log-tail TTS announcer lives in its own repo:
[file-to-speech](https://github.com/stempit/file-to-speech) (local checkout
at `../file-to-speech`).

## Architecture

```
cmd/bot/bot.go          – main entry point; wires everything together
internal/
  dispatch/             – inbound worker pool + serialized outbound sender;
                          processDM/processChannelMessage parse .advert, then call registry.Dispatch
  commands/             – self-registering command plugins (one folder each; no loose files)
    allcommands/        – aggregator: blank-imports every plugin so they self-register
    ping/ trace/ last/ news/ llm/ weather/ path/ top/ stat/ eightball/
    ticket/ help/ agent/ bbs/ dice/ num/ hang/ words/ poker/  – one command per folder
    advert/             – .advert parser; handled in the dispatch layer (needs the
                          client's SendSelfAdvert), so it is not a registry plugin
  support/              – shared framework + support layer sitting above commands/
    registry/           – the command framework: Ctx, PingMeta, Meta, Command, Register, Dispatch, Help
    games/              – shared support layer for the game plugins (scope keying,
                          historylog leaderboard, round-result Announcer, NormalizeRu)
    helpers/            – small cross-command utilities (ClipRunes, HistoryDir)
    historylog/         – per-conversation append-only log files
  greeter/              – greets newly seen nodes; owns KnownKeys persistence + GreetMessage
  config/               – YAML config loader (singleton via sync.Once) + per-plugin Sectioner
  shrink/               – config policy layer over the fit package (meshcore-kit):
                          variant resolution per send path + the LLM compressor
  bridge/               – external integrations
    llm/                – Ollama and OpenRouter LLM providers
    mqttbridge/         – MQTT publishing to custom broker + Let's Mesh analyzer
    tgbridge/           – Telegram bot bridge (long-poll)
    alerts/             – polls one or more hazard-alert sources (bplarussia.ru FPV/UAV
                          incident cards, МЧС RSS feeds) and announces new items to a mesh
                          channel; no meshcore/dispatch dependency, delivery via an injected
                          OnEvent callback (same shape as tgbridge)
    agentclient/        – universal AI agent client; Agent.type picks the adapter:
                          "openai" (OpenAI-compatible chat completions — nanobot,
                          LiteLLM, vLLM, …) or "picoclaw" (legacy Pico Protocol
                          WebSocket); used by .agent
```

Sibling module repos (local checkouts next to this repo, wired via `replace`
in go.mod until published):
- **meshcore-go** — the MeshCore protocol driver (`meshcore.` client; BLE/serial).
- **meshcore-kit** — the shared app runtime (`companion.` connect/sync/inbox
  service, used by the gateways) and the pure message fitter (`fit.`; `shrink`
  here is its policy wrapper).

The MeshCore client (`meshcore.MeshCoreClient` from meshcore-go) is the core dependency. Everything else depends on it.

Message flow: MeshCore device → `client.IncomingMessages()` channel → `processDM` / `processChannelMessage` in `internal/dispatch` → `registry.Dispatch` → first matching plugin's `Handle` → reply sent back over radio.

## Configuration

Copy `config.example.yaml` to `config.yaml` and fill in secrets. The file is loaded once at startup; restart to pick up changes.

Key environment variables:
- `OPENROUTER_API_KEY` – required when `LLM.type: openrouter`
- `OLLAMA_HOST` – Ollama base URL (default: `http://localhost:11434`)

**Note:** the yaml key for LLM shrink retries is `shrink_max_retries` (fixed from the original typo `shrink_max_reties`).

**Language (i18n):** the top-level `Language` key (`en` | `ru`; empty → `en`) sets the bot-wide UI language for command replies and help. It is global — one language for the whole bot, switch + restart. User-facing strings live in each command's own `lang/en.yaml` + `lang/ru.yaml`, embedded via `//go:embed lang/*.yaml` and loaded into a package-level `tr = i18n.MustLoad(langFS)` (`internal/support/i18n`). `en` is the base/fallback language: a missing key falls back to `en`, then to the key literal, so nothing panics. Handlers look strings up by key on `ctx.Lang` (`tr.T(ctx.Lang, "key", args…)` for strings, `tr.TList` for random pools like ping/8ball); placeholders are positional `%s`/`%d`, so translations must keep argument order. Long help is localized opt-in via `LocalizedHelp(lang) string` (falls back to `Meta().HelpLong`); the `.help` index group labels/footer come from the registry's own `lang/*.yaml`. Code paths without a `Ctx` (self-firing round timers in `words`/`vote`) resolve the global language via a package `…Lang()` helper reading `config.Get().Language`. **Word-bank languages:** `hang` and `words` (the anagram game) each embed both a Russian and an English word list (`words_ru.txt`/`words_en.txt`, `words_anagram_ru.txt`/`words_en.txt`) and pick one at startup based on `Language` (`ru` → Russian, otherwise English) — same restart-to-switch rule as the rest of the bot's i18n. `hang` has no other config, so it implements `registry.Configurable` (`Enabled()` always `true`) purely to get a post-config-load hook for this. Adding a third UI language = drop a `<code>.yaml` into each `lang/` folder; no code changes.

**Channel redirect:** the `Redirect` section steers flood-prone commands off busy channels — when a rule's command arrives on a listed channel, the bot replies with the rule's notice instead of running the command (DMs exempt). The gate lives in `registry.Dispatch` (before the handler runs, so no state mutates) with a per-channel cooldown on the notice itself. Rules match by canonical command name (`Meta().Name`).

**Hazard alerts:** the `Alerts` section configures `internal/bridge/alerts` with a list of independent `sources`, each opt-in via its own `enabled` flag (default false) and with its own `type` (`html_cards` for bplarussia.ru-style incident pages, `rss` for standard RSS feeds), `url`, target `channel` (must already exist on the MeshCore device), `poll_interval_sec`, and `max_per_poll`. An optional `title_prefix` filters an RSS/HTML source down to items whose title starts with it — e.g. the shipped `mchs` source uses `title_prefix: "Внимание"` to keep only genuine МЧС hazard warnings and skip routine daily reports. An optional `postfix` is appended as-is to the announced message text (include your own separator, e.g. `" #БПЛА"`). Each source silently seeds its own seen-set from whatever's already there on first run (so history isn't dumped into the channel), then announces only new items — title only, no description, to stay within the mesh's tight per-message byte limit — capped at `max_per_poll` per poll to avoid flooding after downtime. Dedup state lives in `log/alerts/<source name>.txt`.

## Key conventions

- **All log messages in English.** Use `log/slog` with structured key-value pairs.
- **No `log.Fatal` inside packages.** Return errors to callers; only `os.Exit` in `main`.
- **No `goto`.** Use labeled `break` for loop exits.
- **Signal handling via `signal.NotifyContext`.** Pass `ctx.Done()` to goroutines instead of a bare `done` channel.
- **Command prefix stripping:** use `strings.TrimPrefix` + `strings.TrimSpace`, not `strings.ReplaceAll`.
- **`bytes.Equal` from stdlib**, not a hand-rolled helper.
- Config is accessed via `config.Get()` everywhere; it is a pointer to a value struct — treat it as read-only.

## Runtime file layout

```
config.yaml
log/
  history/
    channels/<name>.txt   – channel message history (used by .last)
    dm/<name>.txt         – direct-message history
  known.txt               – greeted node public keys (hex, one per line)
  news/<feed>.txt         – RSS items written by the separate newswire app (https://github.com/Stempit/newswire); read by .news
  alerts/<name>.txt       – ids of alert items already seen/announced, one file per configured source
```

## Packages that need care

- **meshcore-go `client.go`** (~2300 lines) – BLE protocol state machine. The `sentConfirmCh` channel is a single shared slot; only one send can be in flight at a time, so concurrent senders must serialise their sends.
- **`internal/bridge/mqttbridge/bridge.go`** (~1700 lines) – manages multiple MQTT connections (custom broker + EU/US analyzers) with JWT auth (Ed25519). Changes here affect network-visible state.
- **`internal/config/model.go`** – holds only **infra** config (Language, Connection, Proxy, Trace, Security, Redirect, Migration, Shrink, LLM). Adding a field requires a YAML tag; check for typos. **Per-command** config lives in the plugin folder instead: the plugin implements `registry.Configurable` and decodes its own section via `config.Sectioner` (two-pass YAML over the same single `config.yaml`). The **bridge** configs (MQTT, Telegram, Alerts) are decoded by `cmd/bot` straight into the owning bridge's types via the same Sectioner — `internal/config` does not import the feature packages.

## What the commands do

Each command is a self-registering plugin in its own folder; the table's **Plugin** column is the package path. `Meta().Name` (shown after the `→`) is the shrink-variant key, which is decoupled from the trigger.

| Command | Plugin | Notes |
|---------|--------|-------|
| `ping` / `пинг` / `.ping` | `commands/ping` → `ping` | Random pong string |
| `.trace` | `commands/trace` → `trace` | Haversine distance + hop IDs from `registry.PingMeta` |
| `.last [n] [channel]` | `commands/last` → `last` | Last *n* lines from `historylog` (max 7) |
| `.llm <prompt>` | `commands/llm` → `llm` | LLM query; auto-shrinks response to ≤130 chars |
| `.news [feed] [n] [offset]` | `commands/news` → `news` | Lists feeds or returns recent RSS items from `log/news/`, populated by the [newswire](https://github.com/Stempit/newswire) poller |
| `.agent <prompt>` / `@[BotName]` mention | `commands/agent` → `agent` | `Matcher` interceptor (Priority 20, above bbs): mention beats BBS. Sends prompt to the user's AI agent via `agentclient` (`Agent.type`: openai = nanobot & friends, picoclaw = legacy WS); auto-shrinks response |
| `.2ch` (+ all input while in BBS mode) | `commands/bbs` → `bbs` | Stateful `Matcher` interceptor (Priority 10): forum/BBS mode until `.exit` |
| `.ticket [random]` | `commands/ticket` → `ticket` | Stable random "callsign" per sender (persisted); not in `.help` index |
| `.help` | `commands/help` → `help` | Grouped command index from registered `Meta` (`Group`/`HelpShort`); per-command help is `.<cmd> help` (`HelpLong`), intercepted in `registry.Dispatch` |
| `.8ball <question>` | `commands/eightball` → `8ball` | Magic 8-Ball; random Russian yes/no/maybe answer |
| `.w [tomorrow] [city]` | `commands/weather` → `weather` | Current or next-day weather from wttr.in (`Weather` config section) |
| `.path [<from> to <to>]` | `commands/path` → `path` | Network summary or best route between repeaters (`Stats` config section) |
| `.top` | `commands/top` → `top` | Top-5 channel authors by message count (scans last `Top.scan_count`, default 100); channels only, "not available" in DMs |
| `.stat` | `commands/stat` → `stat` | Channel traffic in last 24h: message count, peak hour, unique users; channels only, "not available" in DMs |
| `.dice [NdM]` | `commands/dice` → `dice` | Roll dice (`NdM`/`M`, default `1d6`). Sub-verbs `start`/`roll`/`end`/`status` run a per-channel roll-off (highest total wins; one roll per player; stays open until anyone runs `.dice end`); session verbs channel-only |
| `.num [start/<n>/status/end/top]` | `commands/num` → `num` | Guess-the-number (bot picks 1..max, default 100; `больше`/`меньше` hints). Works in channels (shared) and DMs (private); `.num top` is a 30-day win leaderboard. See [internal/commands/num/README.md](internal/commands/num/README.md) |
| `.hang [start/<буква>/<слово>/status/end/top]` | `commands/hang` → `hang` | Hangman from an embedded word bank (`words_ru.txt`/`words_en.txt`, picked by `Language`; 6 lives). Channels are collaborative, DMs solo; `.hang top` is a 30-day leaderboard. See [internal/commands/hang/README.md](internal/commands/hang/README.md) |
| `.words [start/<слово>/status/end/top]` | `commands/words` → `words` | Anagram ("Слова из слова"): timed round, build words from one long base word's letters; `score = len²`. Opt-in (`words.enabled`), embedded dict (`words_anagram_ru.txt` for `Language: ru`, `words_en.txt` otherwise). Self-firing timer pushes results via the injected `games.Announcer`. Channels collaborative-competitive, DMs solo; `.words top` is a 30-day leaderboard. See [internal/commands/words/README.md](internal/commands/words/README.md) |
| `.vote [start <q> \| a \| b / <n> / status / end]` | `commands/vote` → `vote` | Group poll: open with a question and pipe-separated options, one changeable anonymous vote per sender (keyed by `SenderKey`), numbered results. Channels only. Opt-in (`vote.enabled`); auto-closes after `vote.round_sec` (default 180s), pushing results via the injected `games.Announcer`, or via `.vote end`. No leaderboard |
| `.poker [start/join/leave/end/top]` | `commands/poker` → `poker` | Fixed-limit Hold'em on a channel (2–6): the channel gets dealer commentary only, betting actions go via DM (`Matcher` intercept for seated players; priced dynamic turn prompts). Opt-in (`poker.enabled`); blinds 5/10, 90s turn timeout (auto check/fold), 20s deal pause, 15-min idle close, `.poker end` opener-only. `.poker top` is a 30-day leaderboard. See [internal/commands/poker/README.md](internal/commands/poker/README.md) |
| `.room [status/get/send/on/off/last/who]` | `commands/room` → `room` | Room-server analog on DMs: post (`send`), read history (`get`/`last`), subscribe to new-post DM pushes (`on`/`off`); per-user unread cursors, history ring in `log/room/room.json` (atomic save + `.bak`). DMs only; pushes fan out via the injected `room.SetDelivery` lookup+sender wired in `cmd/bot`. Opt-in (`room.enabled`); `closed` + `allow` restricts by key prefix |

New commands are self-registering plugins: create `internal/commands/<name>/` implementing `registry.Command` (+ `Configurable`/`Matcher` if needed) and add one blank import to `internal/commands/allcommands`. There is no central switch or help list to edit — `.help` and routing are generated from each plugin's `Meta`. See [docs/NEW_COMMAND.md](docs/NEW_COMMAND.md) and the framework in [internal/support/registry/README.md](internal/support/registry/README.md).

The `internal/support/games` package is a **shared support layer** (scope keying, the `historylog`-backed leaderboards under `log/games/<game>/`, the round-result `Announcer`, and `NormalizeRu`/`NormalizeEn`); each game's own logic lives in its `internal/commands/<game>/` plugin and imports `games` for those helpers. Win records and leaderboards (`.num top` / `.hang top` / `.words top`) are keyed by game scope — see [internal/support/games/README.md](internal/support/games/README.md).
