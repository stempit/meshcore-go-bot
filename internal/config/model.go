package config

type TraceConfig struct {
	UseConfigCoords bool    `yaml:"use_config_coords"`
	Lat             float64 `yaml:"lat"`
	Lon             float64 `yaml:"lon"`
	// FullEnabled gates the ".trace full" all-paths collection. nil/absent →
	// enabled; set false to answer ".trace full" with the bare trace.
	FullEnabled *bool `yaml:"full_enabled"`
	// CollectWindowSec is how long ".trace full" keeps listening for further
	// copies of the command message (flood propagation takes seconds on long
	// paths). Clamped to [3, 30] at use site; 0 → default 15.
	CollectWindowSec int `yaml:"collect_window_sec"`
	// MaxPaths caps how many distinct paths ".trace full" reports; each path is
	// a reply line. 0 → default 8.
	MaxPaths int `yaml:"max_paths"`
	// ShowNames renders ".trace full" hops as repeater names from the contact
	// list instead of key hashes; hops not in contacts fall back to the hash.
	// nil/absent → enabled.
	ShowNames *bool `yaml:"show_names"`
}

type SecurityListConfig struct {
	Whitelist []string `yaml:"whitelist"`
	Blacklist []string `yaml:"blacklist"`
}

type SecurityConfig struct {
	Channels SecurityListConfig `yaml:"channels"`
	Direct   SecurityListConfig `yaml:"direct"`
}

// RedirectRule steers a set of commands away from busy channels: when a listed
// command arrives on a listed channel, the bot replies with Message instead of
// running the command. DMs are never redirected.
type RedirectRule struct {
	On       []string `yaml:"on"`       // channels where the rule applies; empty = all channels
	Commands []string `yaml:"commands"` // canonical command names (Meta().Name), e.g. "ping", "dice"
	Message  string   `yaml:"message"`  // notice sent instead of running the command
}

// RedirectConfig declutters flood-prone channels by pointing common commands at
// dedicated channels. Cooldown throttles the notice itself so repeated commands
// do not turn the redirect into new flood.
type RedirectConfig struct {
	Enabled  bool           `yaml:"enabled"`
	Cooldown int            `yaml:"cooldown_seconds"` // min seconds between notices per channel+command; 0 = no throttle
	Rules    []RedirectRule `yaml:"rules"`
}

// ShrinkLLMConfig configures the "llm" shrink variant. Type/Model are optional
// overrides that inherit from the top-level LLM section when empty, so a light
// model can shrink while a big model answers.
type ShrinkLLMConfig struct {
	Type        string  `yaml:"type"`
	Model       string  `yaml:"model"`
	Prompt      string  `yaml:"prompt"`
	Temperature float64 `yaml:"temperature"`
	MaxRetries  int     `yaml:"max_retries"`
}

// ShrinkConfig is the unified mesh-fit configuration. Default is the global
// variant; Overrides selects a variant per command/source name.
type ShrinkConfig struct {
	Default   string            `yaml:"default"`    // truncate|split|llm|skip
	MaxBytes  int               `yaml:"max_bytes"`  // hard mesh limit, default 140
	Ellipsis  string            `yaml:"ellipsis"`   // appended by truncate
	CyrToLat  bool              `yaml:"cyr_to_lat"` // lossless Cyrillic→Latin homoglyph squeeze
	Overrides map[string]string `yaml:"overrides"`  // name -> variant
	LLM       ShrinkLLMConfig   `yaml:"llm"`
}

type ConnectionConfig struct {
	// Type selects the transport: "bt" (Bluetooth LE, default), "serial"
	// (USB/UART) or "tcp" (alias "wifi": Wi-Fi/Ethernet companion firmware).
	Type string `yaml:"type"`
	// SerialPort is the OS device path used when Type is "serial"
	// (e.g. "/dev/ttyUSB0", "/dev/tty.usbmodem14101", "COM3").
	SerialPort string `yaml:"serial_port"`
	// SerialBaud sets the baud rate for the serial transport (default 115200).
	SerialBaud int `yaml:"serial_baud"`
	// TCPAddress is the device address used when Type is "tcp" — the Wi-Fi
	// or Ethernet companion firmware serves the same protocol on TCP.
	// Either "host:port" or a bare host, which gets the default port 5000.
	TCPAddress string `yaml:"tcp_address"`
	// DeviceName selects the BLE device by exact advertised name (e.g.
	// "MeshCore-AB12") when Type is "bt". Empty scans any MeshCore device
	// and picks the strongest signal. The portable identifier on macOS,
	// where the address is a per-machine CoreBluetooth UUID.
	DeviceName string `yaml:"device_name"`
	// DeviceAddress selects the BLE device by platform address when Type
	// is "bt" (MAC on Linux/Windows, CoreBluetooth UUID on macOS).
	// Combine with DeviceName or use alone.
	DeviceAddress string `yaml:"device_address"`
	// ChannelTransmissions is the MAXIMUM number of times each channel reply is
	// transmitted (the same packet, deduplicated by receivers) to improve
	// delivery against silent RF drops. The bot stops early once it hears the
	// mesh rebroadcast the message, so fewer transmissions are sent on a healthy
	// network. This is our own node sending the packet, not the network "repeat"
	// by repeater nodes. 0 or unset defaults to 3; 1 sends a single transmission.
	ChannelTransmissions int `yaml:"channel_transmissions"`
}

type ProxyConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"` // e.g. "socks5://user:pass@host:1080"
}

// MigrationConfig controls the "switch to 2-byte hash" nudge: when a received
// advert or public-channel message reveals a node still using 1-byte routing
// hashes, the bot posts a reminder on the channel, at most once per CooldownSec
// per node.
type MigrationConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Message     string `yaml:"message"`      // default "switch to 2-byte please"
	CooldownSec int    `yaml:"cooldown_sec"` // default 86400 (24h)
}

type Config struct {
	Language   string           `yaml:"Language"` // bot-wide UI language ("en" | "ru"); empty → i18n.Base ("en")
	Connection ConnectionConfig `yaml:"Connection"`
	Proxy      ProxyConfig      `yaml:"Proxy"`
	Trace      TraceConfig      `yaml:"Trace"`
	Security   SecurityConfig   `yaml:"Security"`
	Redirect   RedirectConfig   `yaml:"Redirect"`
	Migration  MigrationConfig  `yaml:"Migration"`
	Shrink     ShrinkConfig     `yaml:"Shrink"`
	LLM        struct {
		Enabled           bool    `yaml:"enabled"`
		Type              string  `yaml:"type"`
		Model             string  `yaml:"model"`
		SystemPrompt      string  `yaml:"system_prompt"`
		SystemTemperature float64 `yaml:"system_temperature"`
	} `yaml:"LLM"`
	// The MQTT, Telegram and Alerts sections are decoded by cmd/bot straight
	// into the owning bridge's config types via Sections().Section — the same
	// two-pass mechanism the command plugins use — so this package does not
	// depend on the feature packages.
}
