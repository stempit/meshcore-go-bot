package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/stempit/meshcore-go"
	"github.com/stempit/meshcore-go-bot/internal/beacon"
	"github.com/stempit/meshcore-go-bot/internal/bridge/alerts"
	"github.com/stempit/meshcore-go-bot/internal/bridge/mqttbridge"
	"github.com/stempit/meshcore-go-bot/internal/bridge/tgbridge"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/allcommands" // self-registering command plugins
	"github.com/stempit/meshcore-go-bot/internal/commands/room"
	"github.com/stempit/meshcore-go-bot/internal/commands/ticket"
	"github.com/stempit/meshcore-go-bot/internal/commands/trace"
	"github.com/stempit/meshcore-go-bot/internal/config"
	"github.com/stempit/meshcore-go-bot/internal/dispatch"
	"github.com/stempit/meshcore-go-bot/internal/greeter"
	"github.com/stempit/meshcore-go-bot/internal/logging"
	"github.com/stempit/meshcore-go-bot/internal/migrator"
	"github.com/stempit/meshcore-go-bot/internal/proxy"
	"github.com/stempit/meshcore-go-bot/internal/shrink"
	"github.com/stempit/meshcore-go-bot/internal/support/games"
	"github.com/stempit/meshcore-go-bot/internal/support/helpers"
	"github.com/stempit/meshcore-go-bot/internal/support/historylog"
	"github.com/stempit/meshcore-go-bot/internal/support/registry"
)

var log = logging.For("bot")

// processStoredMessages controls whether messages stored on the device before
// bot startup are fed into the processing pipeline. Set to false to skip them.
const processStoredMessages = true

// bridgeCfg decodes one top-level config section into target, the same
// two-pass YAML the command plugins use. A malformed section is fatal; a
// missing one leaves the caller's zero value (the feature stays off).
func bridgeCfg(key string, target any) {
	if err := config.Sections().Section(key, target); err != nil {
		log().Error("Failed to decode config section", "section", key, "error", err)
		os.Exit(1)
	}
}

// tcpDialAddr accepts a bare host and appends the companion firmware's
// default TCP port; "host:port" passes through unchanged.
func tcpDialAddr(addr string) string {
	if strings.Contains(addr, ":") {
		return addr
	}
	return addr + ":5000"
}

func main() {
	// config
	if err := config.Load("config.yaml"); err != nil {
		log().Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	// Per-plugin config: each registered command decodes its own section from
	// the single config file (two-pass YAML). Fail loud on invalid values.
	if err := registry.ConfigureAll(config.Sections()); err != nil {
		log().Error("Failed to configure command plugins", "error", err)
		os.Exit(1)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	shrink.ValidateConfig()

	proxyCfg := config.Get().Proxy
	if err := proxy.Init(proxyCfg.Enabled, proxyCfg.URL); err != nil {
		log().Error("Failed to initialize proxy", "error", err)
		os.Exit(1)
	}
	if proxyCfg.Enabled {
		// Redact credentials: the documented URL form carries user:pass.
		if u, err := url.Parse(proxyCfg.URL); err == nil {
			log().Info("Proxy enabled", "url", u.Redacted())
		} else {
			log().Info("Proxy enabled")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// history logger
	histlog := historylog.New(helpers.HistoryDir)
	defer histlog.Close()

	// meshcore client — BLE (default), USB serial or TCP (Wi-Fi/Ethernet)
	// depending on config
	var client *meshcore.MeshCoreClient
	conn := config.Get().Connection
	switch conn.Type {
	case "serial":
		port := conn.SerialPort
		if port == "" {
			log().Error("Connection.serial_port must be set when Connection.type is \"serial\"")
			os.Exit(1)
		}
		baud := conn.SerialBaud
		if baud == 0 {
			baud = 115200
		}
		client = meshcore.CreateSerialClient(port, baud)
	case "tcp", "wifi":
		if conn.TCPAddress == "" {
			log().Error("Connection.tcp_address must be set when Connection.type is \"tcp\"")
			os.Exit(1)
		}
		client = meshcore.CreateTCPClient(tcpDialAddr(conn.TCPAddress))
	default:
		var ble []meshcore.BLEOption
		if conn.DeviceName != "" {
			ble = append(ble, meshcore.WithDeviceName(conn.DeviceName))
		}
		if conn.DeviceAddress != "" {
			ble = append(ble, meshcore.WithDeviceAddress(conn.DeviceAddress))
		}
		client = meshcore.CreateClient(ble...)
	}
	defer client.Disconnect()

	// log history in client
	client.SetHistoryLogger(histlog)

	// mqtt init — optional. The bridge is constructed here, in the composition
	// root, and attached to the client as a packet publisher; the meshcore
	// driver itself has no dependency on the MQTT package.
	var mqttCfg mqttbridge.MQTTConfig
	bridgeCfg("MQTT", &mqttCfg)
	if bridge := startMQTTBridge(client, &mqttCfg); bridge != nil {
		defer bridge.Stop()
	}
	client.RequestAllStats()

	// ─── Message engine ───────────────────────────────────────────────────────
	// The dispatcher owns the inbound worker pool and the serialized outbound
	// sender. Subsystems (greeter, Telegram) are wired in before it starts.
	d := dispatch.New(client)

	// The .words game pushes round results from a timer goroutine; give the games
	// package a way to resolve a scope back to a channel/contact and enqueue.
	games.SetAnnouncer(d.AnnounceToScope)

	// The .room service pushes new posts to subscribers as DMs; inject the
	// contact lookup and the dispatcher's DM sender the same way.
	room.SetDelivery(
		func(rawKey string) *meshcore.Contact {
			if rawKey == "" { // undecodable store key: an empty prefix matches every contact
				return nil
			}
			return client.FindContactByKeyPrefix([]byte(rawKey)[:min(6, len(rawKey))])
		},
		d.SendDirectMessage)

	// The .trace full command enumerates every path the message was heard on:
	// feed raw receptions into the collector and hand it to the plugin.
	traceCollector := trace.NewCollector(client)
	client.OnRawData(traceCollector.Observe)
	trace.SetCollector(traceCollector)

	// ─── Greet new nodes + hash migration nudge ───────────────────────────────

	knownKeys, err := greeter.NewKnownKeys("log/known.txt")
	if err != nil {
		log().Error("Failed to load known keys", "error", err)
		os.Exit(1)
	}

	if err := ticket.InitTicketStore("log/tickets.txt"); err != nil {
		log().Error("Failed to load ticket store", "error", err)
		os.Exit(1)
	}

	migCfg := config.Get().Migration
	migMsg := migCfg.Message
	if migMsg == "" {
		migMsg = "switch to 2-byte please"
	}
	migCooldown := time.Duration(migCfg.CooldownSec) * time.Second
	if migCooldown <= 0 {
		migCooldown = 24 * time.Hour
	}
	notifier := migrator.NewNotifier(migCfg.Enabled, migMsg, migCooldown)

	g := greeter.New(client, knownKeys, d.SendChannelMessage)
	g.Register()

	mig := migrator.New(notifier, client, d.SendChannelMessage, d.SendDirectMessage)
	d.AddObserver(mig)

	// ─── Telegram bridge ──────────────────────────────────────────────────────

	var tgb *tgbridge.Bridge
	var tgCfg tgbridge.TelegramConfig
	bridgeCfg("Telegram", &tgCfg)
	if tgCfg.Enabled {
		tgb, err = tgbridge.New(tgCfg, slog.Default(), proxy.HTTPClient())
		if err != nil {
			log().Error("Failed to start Telegram bridge", "error", err)
		} else {
			tgb.OnMessage(func(meshChannelName, senderName, text string) {
				tgChannel := client.GetChannelByName(meshChannelName)
				if tgChannel == nil {
					log().Warn("Telegram bridge: meshcore channel not found", "name", meshChannelName)
					return
				}
				// The "[TG:sender] " prefix lands on the first wire message only;
				// the "telegram" variant (config) decides split vs llm shrink.
				prefix := fmt.Sprintf("[TG:%s] ", senderName)
				for _, part := range shrink.Fit("telegram", prefix, []string{text}) {
					d.Enqueue(dispatch.Outgoing{Channel: tgChannel, Text: part, Ack: false})
				}
			})
			go tgb.Start()
			defer tgb.Stop()
		}
	}
	d.SetTelegram(tgb)

	// ─── FPV/UAV alert bridge ──────────────────────────────────────────────────

	var alertsCfg alerts.Config
	bridgeCfg("Alerts", &alertsCfg)
	if len(alertsCfg.Sources) > 0 {
		ab, err := alerts.New(alertsCfg, slog.Default(), proxy.HTTPClient())
		if err != nil {
			log().Error("Failed to start alerts bridge", "error", err)
		} else {
			ab.OnEvent(func(sourceName, channelName, postfix string, ev alerts.Event) {
				ch := client.GetChannelByName(channelName)
				if ch == nil {
					log().Warn("Alerts bridge: meshcore channel not found", "source", sourceName, "name", channelName)
					return
				}
				text := ev.Title
				if postfix != "" {
					text += postfix
				}
				for _, part := range shrink.Fit("alerts", "", []string{text}) {
					d.Enqueue(dispatch.Outgoing{Channel: ch, Text: part, Ack: false})
				}
			})
			go ab.Start()
			defer ab.Stop()
		}
	}

	// Start the engine now that all subsystems are wired in.
	d.Run(ctx)

	// ─── Sync stored messages ─────────────────────────────────────────────────
	// The BLE driver pushes every received message into client.IncomingMessages()
	// AND into the accumulator maps (GetAllMessages / GetAllChannelMessages).
	// We wait for sync to finish, then drain the client channel (so the live
	// forwarder only sees truly new arrivals), log what was stored, and
	// optionally re-queue stored messages through our own pipeline once.

	syncDone := make(chan struct{})
	client.OnNoMoreMessages(func() { close(syncDone) })
	if err := client.SyncNextMessage(); err != nil {
		log().Error("Failed to sync stored messages", "error", err)
		close(syncDone)
	}

	select {
	case <-syncDone:
		log().Info("Device sync complete")
	case <-time.After(15 * time.Second):
		log().Warn("Device sync timed out, proceeding with messages received so far")
	}
	// Replace callback so late "no more messages" responses during live
	// operation don't panic on the already-closed syncDone channel.
	client.OnNoMoreMessages(func() {})

	// Give the BLE device time to settle after the sync loop before we start
	// sending replies. Without this pause the first write times out.
	time.Sleep(2 * time.Second)

	// Drain the client's incoming channel: messages buffered there during sync
	// are already captured in the accumulator maps; we'll re-queue selectively below.
drainLoop:
	for {
		select {
		case <-client.IncomingMessages():
		default:
			break drainLoop
		}
	}

	// Log and optionally process stored messages exactly once.
	for _, m := range client.GetAllMessages() {
		key := m.SenderKey
		if len(key) > 6 {
			key = key[:6]
		}
		log().Info("Stored DM", "sender", fmt.Sprintf("%x", key), "text", m.Text)
		if processStoredMessages {
			d.Submit(m)
		}
	}
	for _, m := range client.GetAllChannelMessages() {
		log().Info("Stored channel message", "channel", m.ChannelIndex, "sender", m.SenderName, "text", m.Text)
		if processStoredMessages {
			d.Submit(m)
		}
	}

	// ─── Live message forwarder goroutine ─────────────────────────────────────
	// All messages from here on are new live arrivals.

	// The driver recreates incomingCh on every reconnect (startWith) but leaves
	// the old one open (markDisconnected), so a plain `range` parks forever on
	// the stale channel after the first reconnect and the pipeline silently
	// stalls. Re-acquire the channel on every tick: a stale channel is swapped
	// for the new one within one tick, and a closed one (a reconnect whose
	// handshake failed makes the driver Disconnect) nils out — a nil channel
	// never fires in a select — until the next tick picks up the replacement.
	// Messages arriving in between buffer in the driver's incomingCh.
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		ch := client.IncomingMessages()
		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					ch = nil
					continue
				}
				d.Submit(msg)
			case <-tick.C:
				ch = client.IncomingMessages()
			case <-ctx.Done():
				return
			}
		}
	}()

	// ─── Periodic advert beacon ───────────────────────────────────────────────
	// Makes the device emit its own advert on configurable periods (flood in
	// hours, direct in minutes). The first send happens after one full period,
	// not at startup. Optional: a missing or disabled section keeps it off.

	var advertCfg beacon.Config
	bridgeCfg("Advert", &advertCfg)
	if advertCfg.Enabled {
		if err := advertCfg.Validate(); err != nil {
			log().Error("Invalid Advert config", "error", err)
			os.Exit(1)
		}
		go beacon.New(client, advertCfg).Run(ctx)
		log().Info("Advert beacon enabled", "flood_every_hours", advertCfg.FloodIntervalHours, "direct_every_min", advertCfg.DirectIntervalMin)
	}

	// ─── Initial stats ────────────────────────────────────────────────────────

	if err := client.RequestAllStats(); err != nil {
		log().Error("Failed to request initial stats", "error", err)
	} else {
		time.Sleep(500 * time.Millisecond)
		fmt.Println(client.FormatStats())
	}

	// ─── Periodic stats (keeps MQTT up to date, detects disconnection) ────────

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := client.RequestAllStats(); err != nil {
					log().Error("Failed to request periodic stats", "error", err)
					if !client.IsConnected() {
						log().Warn("Connection lost, reconnecting...")
						time.Sleep(5 * time.Second)
						if err := client.Reconnect(); err != nil {
							log().Error("Reconnect failed", "error", err)
						} else {
							log().Info("Reconnected to device")
						}
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	log().Info("Bot running. Press Ctrl+C to exit.")
	<-ctx.Done()
	log().Info("Shutting down...")
}

// startMQTTBridge constructs the MQTT bridge, wires device-info/stats forwarding,
// and attaches it to the client as a packet publisher. MQTT is optional: if the
// bridge fails to start, it logs and returns nil so startup continues without it.
func startMQTTBridge(client *meshcore.MeshCoreClient, cfg *mqttbridge.MQTTConfig) *mqttbridge.MQTTBridge {
	bridge := mqttbridge.NewMQTTBridge(cfg)
	bridge.SetLogger(slog.Default())
	bridge.SetCallbacks(mqttbridge.MQTTCallbacks{
		OnConnected:    func(broker string) { log().Info("MQTT connected", "broker", broker) },
		OnDisconnected: func(broker string) { log().Info("MQTT disconnected", "broker", broker) },
	})
	if err := bridge.Start(); err != nil {
		log().Error("Failed to start MQTT bridge", "error", err)
		return nil
	}

	// Seed current device info, then keep the bridge updated as self-info and
	// stats arrive from the device.
	if info := client.GetSelfInfo(); info != nil {
		bridge.SetDeviceInfo(mqttbridge.DeviceInfoFromMesh(info))
	}
	client.OnSelfInfo(func(info *meshcore.SelfInfo) { bridge.SetDeviceInfo(mqttbridge.DeviceInfoFromMesh(info)) })
	client.OnStats(func(stats *meshcore.CompanionRadioStats) {
		bridge.SetDeviceStats(mqttbridge.DeviceStatsFromMesh(stats))
	})

	client.SetPacketPublisher(bridge)
	return bridge
}
