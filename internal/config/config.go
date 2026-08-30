// Package config loads and validates napio's YAML configuration.
//
// Validation is deliberately strict and happens once, at startup: a
// misconfigured gateway that fails immediately with a precise message is far
// friendlier than one that half-works and wakes nothing at 2am.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Default values applied to any server that does not override them.
const (
	DefaultInactivityTimeout = 10 * time.Minute
	DefaultStartTimeout      = 5 * time.Minute
	DefaultStopTimeout       = 2 * time.Minute
	DefaultPollInterval      = 10 * time.Second
	DefaultAddress           = "0.0.0.0"
	DefaultAPIListen         = "127.0.0.1:8378"

	DefaultWakeMinPacketSize = 4
	DefaultWakePackets       = 2
	DefaultWakeWindow        = 5 * time.Second
	DefaultWakeCooldown      = 30 * time.Second

	DefaultRCONFailureTolerance = 3
)

// Config is the root of napio.yaml.
type Config struct {
	LogLevel  string   `yaml:"log_level" json:"log_level"`
	LogFormat string   `yaml:"log_format" json:"log_format"`
	API       API      `yaml:"api" json:"api"`
	Defaults  Defaults `yaml:"defaults" json:"-"`
	Servers   []Server `yaml:"servers" json:"servers"`
}

// API configures the optional HTTP interface. It exists so a web UI can be
// added later without touching the core; it is not required for napio to work.
type API struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Listen  string `yaml:"listen" json:"listen"`
	// Token, when set, is required as "Authorization: Bearer <token>".
	Token string `yaml:"token" json:"-"`
	// Control enables the start/stop endpoints. Read-only when false.
	Control bool `yaml:"control" json:"control"`
}

// Defaults are applied to every server that does not set the field itself.
type Defaults struct {
	InactivityTimeout *Duration `yaml:"inactivity_timeout"`
	StartTimeout      *Duration `yaml:"start_timeout"`
	StopTimeout       *Duration `yaml:"stop_timeout"`
	PollInterval      *Duration `yaml:"poll_interval"`
}

// Server is one Factorio server managed by napio.
type Server struct {
	Name string `yaml:"name" json:"name"`
	// Address and Port are the public UDP endpoint players connect to. napio
	// binds it while the server is asleep and releases it while it runs.
	Address string `yaml:"address" json:"address"`
	Port    int    `yaml:"port" json:"port"`

	InactivityTimeout Duration `yaml:"inactivity_timeout" json:"inactivity_timeout"`
	StartTimeout      Duration `yaml:"start_timeout" json:"start_timeout"`
	StopTimeout       Duration `yaml:"stop_timeout" json:"stop_timeout"`
	PollInterval      Duration `yaml:"poll_interval" json:"poll_interval"`

	// StartOnBoot starts the server when napio starts, instead of waiting for
	// a player. Off by default: the whole point is to not run when unused.
	StartOnBoot bool `yaml:"start_on_boot" json:"start_on_boot"`

	Wake        Wake        `yaml:"wake" json:"wake"`
	RCON        RCON        `yaml:"rcon" json:"-"`
	Backend     Backend     `yaml:"backend" json:"backend"`
	Matchmaking Matchmaking `yaml:"matchmaking" json:"-"`
}

// Matchmaking optionally keeps a sleeping server visible in Factorio's public
// server browser, by publishing the listing the stopped Factorio process can
// no longer publish itself. Off by default: enabling it advertises a server
// publicly, which must always be a deliberate choice.
//
// See docs/MATCHMAKING.md for the protocol and its caveats.
type Matchmaking struct {
	Enabled bool `yaml:"enabled"`

	// DataDir is the Factorio data directory (the one holding config/ and
	// mods/). It is the easiest way to point napio at the same metadata the
	// server itself uses.
	DataDir string `yaml:"data_dir"`
	// ServerSettings and ModsDir override the paths derived from DataDir.
	ServerSettings string `yaml:"server_settings"`
	ModsDir        string `yaml:"mods_dir"`

	// PublicAddress is the "host:port" players actually connect to. napio
	// will not guess it: an advertised address that is wrong is worse than no
	// listing at all.
	PublicAddress string `yaml:"public_address"`

	// Username and Token override the credentials in server-settings.json.
	Username string `yaml:"username"`
	Token    string `yaml:"token"`

	// SleepingNote is appended to the advertised description so players know
	// what to expect before they click Join. Set it to "" to advertise the
	// description unchanged.
	SleepingNote *string `yaml:"sleeping_note"`

	// BaseURL overrides the matching service, for testing against a lab.
	BaseURL string `yaml:"base_url"`
}

// DefaultSleepingNote is appended to a sleeping server's description. The
// first join attempt wakes the server and usually times out, so players are
// told that up front rather than discovering it.
const DefaultSleepingNote = "[napio: asleep - joining wakes it, retry in ~30s]"

// Note returns the description suffix to advertise.
func (m Matchmaking) Note() string {
	if m.SleepingNote == nil {
		return DefaultSleepingNote
	}
	return *m.SleepingNote
}

// SettingsPath is where the Factorio server settings live.
func (m Matchmaking) SettingsPath() string {
	if m.ServerSettings != "" {
		return m.ServerSettings
	}
	if m.DataDir != "" {
		return filepath.Join(m.DataDir, "config", "server-settings.json")
	}
	return ""
}

// ModsPath is where the mod list lives.
func (m Matchmaking) ModsPath() string {
	if m.ModsDir != "" {
		return m.ModsDir
	}
	if m.DataDir != "" {
		return filepath.Join(m.DataDir, "mods")
	}
	return ""
}

// Wake tunes the "is this a player or a port scanner?" filter. See
// docs/ARCHITECTURE.md §3: napio never parses Factorio packets, it only counts
// them, because a real client retries and a scanner does not.
type Wake struct {
	MinPacketSize int      `yaml:"min_packet_size" json:"min_packet_size"`
	Packets       int      `yaml:"packets" json:"packets"`
	Window        Duration `yaml:"window" json:"window"`
	Cooldown      Duration `yaml:"cooldown" json:"cooldown"`
	// AllowFrom, when non-empty, restricts wake-ups to these CIDRs.
	AllowFrom []string `yaml:"allow_from" json:"allow_from"`
	// Disabled turns off automatic wake-up entirely (API/manual only).
	Disabled bool `yaml:"disabled" json:"disabled"`
}

// RCON configures the Factorio remote console, which napio uses for readiness
// detection, player counting and graceful shutdown. Strongly recommended.
type RCON struct {
	Address  string `yaml:"address"`
	Password string `yaml:"password"`
	// FailureTolerance is how many consecutive failed polls are tolerated
	// before the server is considered gone.
	FailureTolerance int `yaml:"failure_tolerance"`
}

func (r RCON) Enabled() bool { return r.Address != "" }

// Backend selects how the Factorio process is started and stopped.
type Backend struct {
	Type string `yaml:"type" json:"type"`

	// Docker backend.
	Container string `yaml:"container" json:"container,omitempty"`
	Host      string `yaml:"host" json:"-"`

	// Process backend.
	Command    []string `yaml:"command" json:"command,omitempty"`
	WorkingDir string   `yaml:"working_dir" json:"-"`
	Env        []string `yaml:"env" json:"-"`

	// Pelican / Pterodactyl backend. napio talks to the Wings daemon on the
	// node, not to the panel: see internal/backend/wings.go for why.
	WingsURL string `yaml:"wings_url" json:"-"`
	UUID     string `yaml:"uuid" json:"uuid,omitempty"`
	// Token is the node's Wings authentication token. Never serialised.
	Token string `yaml:"token" json:"-"`
	// LogFile is the Factorio log's path relative to the server's own
	// directory. Defaults to factorio-current.log.
	LogFile string `yaml:"log_file" json:"-"`
	// TLSSkipVerify accepts a Wings certificate that does not validate, which
	// is what a node with a self-signed certificate needs.
	TLSSkipVerify bool `yaml:"tls_skip_verify" json:"-"`
}

const (
	BackendDocker  = "docker"
	BackendProcess = "process"
	// BackendPelican and BackendPterodactyl are the same backend: the two
	// panels ship the same Wings daemon and identical routes for everything
	// napio uses. Both spellings are accepted so a configuration reads the way
	// its author thinks about their deployment.
	BackendPelican     = "pelican"
	BackendPterodactyl = "pterodactyl"
)

// uuidRe matches the full server UUID Wings identifies a server by.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// shortIDRe matches the short id in a panel URL, which is the wrong one and
// the one everybody reaches for first.
var shortIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}$`)

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Load reads, defaults and validates a configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return Parse(raw)
}

// Parse behaves like Load but takes the file contents directly.
func Parse(raw []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // typos in the config are errors, not silence
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.expandSecrets(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogFormat == "" {
		c.LogFormat = "text"
	}
	if c.API.Listen == "" {
		c.API.Listen = DefaultAPIListen
	}

	pick := func(override *Duration, set *Duration, fallback time.Duration) Duration {
		if *set != 0 {
			return *set
		}
		if override != nil && *override != 0 {
			return *override
		}
		return Duration(fallback)
	}

	for i := range c.Servers {
		s := &c.Servers[i]
		if s.Address == "" {
			s.Address = DefaultAddress
		}
		s.InactivityTimeout = pick(c.Defaults.InactivityTimeout, &s.InactivityTimeout, DefaultInactivityTimeout)
		s.StartTimeout = pick(c.Defaults.StartTimeout, &s.StartTimeout, DefaultStartTimeout)
		s.StopTimeout = pick(c.Defaults.StopTimeout, &s.StopTimeout, DefaultStopTimeout)
		s.PollInterval = pick(c.Defaults.PollInterval, &s.PollInterval, DefaultPollInterval)

		if s.Wake.MinPacketSize == 0 {
			s.Wake.MinPacketSize = DefaultWakeMinPacketSize
		}
		if s.Wake.Packets == 0 {
			s.Wake.Packets = DefaultWakePackets
		}
		if s.Wake.Window == 0 {
			s.Wake.Window = Duration(DefaultWakeWindow)
		}
		if s.Wake.Cooldown == 0 {
			s.Wake.Cooldown = Duration(DefaultWakeCooldown)
		}
		if s.RCON.FailureTolerance == 0 {
			s.RCON.FailureTolerance = DefaultRCONFailureTolerance
		}
		if s.Backend.Type == "" {
			s.Backend.Type = BackendDocker
		}
	}
}

// expandSecrets resolves ${VAR} references in the fields that are expected to
// hold secrets, so passwords need not be committed to the config file.
func (c *Config) expandSecrets() error {
	var err error
	c.API.Token, err = expandEnv(c.API.Token, "api.token")
	if err != nil {
		return err
	}
	for i := range c.Servers {
		s := &c.Servers[i]
		s.RCON.Password, err = expandEnv(s.RCON.Password, "servers["+s.Name+"].rcon.password")
		if err != nil {
			return err
		}
		s.Matchmaking.Token, err = expandEnv(s.Matchmaking.Token, "servers["+s.Name+"].matchmaking.token")
		if err != nil {
			return err
		}
		s.Backend.Token, err = expandEnv(s.Backend.Token, "servers["+s.Name+"].backend.token")
		if err != nil {
			return err
		}
	}
	return nil
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnv(value, field string) (string, error) {
	var missing []string
	out := envRe.ReplaceAllStringFunc(value, func(match string) string {
		name := match[2 : len(match)-1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return ""
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("%s references unset environment variable(s): %s", field, strings.Join(missing, ", "))
	}
	return out, nil
}

// Validate reports every problem it can find, so a broken config takes one
// round-trip to fix rather than five.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("log_level: %q is not one of debug, info, warn, error", c.LogLevel)
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		add("log_format: %q is not one of text, json", c.LogFormat)
	}
	if c.API.Enabled {
		if _, _, err := net.SplitHostPort(c.API.Listen); err != nil {
			add("api.listen: %q is not a host:port address", c.API.Listen)
		}
	}
	if len(c.Servers) == 0 {
		add("servers: at least one server must be configured")
	}

	seenName := map[string]bool{}
	seenAddr := map[string]string{}
	for i, s := range c.Servers {
		where := fmt.Sprintf("servers[%d]", i)
		if s.Name == "" {
			add("%s.name: must not be empty", where)
		} else {
			where = "server " + s.Name
			if !nameRe.MatchString(s.Name) {
				add("%s: name must contain only letters, digits, '.', '_' and '-'", where)
			}
			if seenName[s.Name] {
				add("%s: duplicate server name", where)
			}
			seenName[s.Name] = true
		}
		if s.Port < 1 || s.Port > 65535 {
			add("%s: port %d is out of range 1-65535", where, s.Port)
		}
		if net.ParseIP(s.Address) == nil {
			add("%s: address %q is not a valid IP address to bind", where, s.Address)
		}
		addr := fmt.Sprintf("%s:%d", s.Address, s.Port)
		if other, ok := seenAddr[addr]; ok {
			add("%s: UDP endpoint %s is already used by server %s", where, addr, other)
		}
		seenAddr[addr] = s.Name

		if s.InactivityTimeout <= 0 {
			add("%s: inactivity_timeout must be positive", where)
		}
		if s.StartTimeout <= 0 {
			add("%s: start_timeout must be positive", where)
		}
		if s.StopTimeout <= 0 {
			add("%s: stop_timeout must be positive", where)
		}
		if s.PollInterval <= 0 {
			add("%s: poll_interval must be positive", where)
		}
		if s.PollInterval >= s.InactivityTimeout {
			add("%s: poll_interval (%s) must be shorter than inactivity_timeout (%s)", where, s.PollInterval, s.InactivityTimeout)
		}
		if s.Wake.Packets < 1 {
			add("%s: wake.packets must be at least 1", where)
		}
		if s.Wake.MinPacketSize < 0 {
			add("%s: wake.min_packet_size must not be negative", where)
		}
		for _, cidr := range s.Wake.AllowFrom {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				add("%s: wake.allow_from entry %q is not a CIDR such as 192.168.0.0/16", where, cidr)
			}
		}
		if s.RCON.Address != "" {
			if _, _, err := net.SplitHostPort(s.RCON.Address); err != nil {
				add("%s: rcon.address %q is not a host:port address", where, s.RCON.Address)
			}
			if s.RCON.Password == "" {
				add("%s: rcon.password is required when rcon.address is set", where)
			}
		}

		if s.Matchmaking.Enabled {
			if s.Matchmaking.PublicAddress == "" {
				add("%s: matchmaking.public_address is required: napio will not guess the address it advertises", where)
			} else if host, port, err := net.SplitHostPort(s.Matchmaking.PublicAddress); err != nil || host == "" || port == "" {
				add("%s: matchmaking.public_address %q must be host:port, for example \"203.0.113.7:34197\"", where, s.Matchmaking.PublicAddress)
			}
			settings := s.Matchmaking.SettingsPath()
			if settings == "" {
				add("%s: matchmaking needs data_dir (or server_settings) so it can read the server's real name, tags and player limit", where)
			} else if _, err := os.Stat(settings); err != nil {
				add("%s: matchmaking.server_settings %s cannot be read: %v", where, settings, err)
			}
		}

		// Keys belonging to another backend are reported rather than ignored:
		// KnownFields catches a misspelt key, but not a correctly spelt one
		// that this backend will never read.
		unusedPelican := func(kind string) {
			for key, set := range map[string]bool{
				"wings_url":       s.Backend.WingsURL != "",
				"uuid":            s.Backend.UUID != "",
				"token":           s.Backend.Token != "",
				"log_file":        s.Backend.LogFile != "",
				"tls_skip_verify": s.Backend.TLSSkipVerify,
			} {
				if set {
					add("%s: backend.%s is not used by the %s backend", where, key, kind)
				}
			}
		}

		switch s.Backend.Type {
		case BackendDocker:
			if s.Backend.Container == "" {
				add("%s: backend.container is required for the docker backend", where)
			}
			if len(s.Backend.Command) > 0 {
				add("%s: backend.command is not used by the docker backend", where)
			}
			unusedPelican(BackendDocker)
		case BackendProcess:
			if len(s.Backend.Command) == 0 {
				add("%s: backend.command is required for the process backend", where)
			}
			if s.Backend.Container != "" {
				add("%s: backend.container is not used by the process backend", where)
			}
			unusedPelican(BackendProcess)
		case BackendPelican, BackendPterodactyl:
			kind := s.Backend.Type
			if s.Backend.WingsURL == "" {
				add("%s: backend.wings_url is required for the %s backend, for example "+
					"\"https://node.example.com:8080\"", where, kind)
			} else if u, err := url.Parse(s.Backend.WingsURL); err != nil || u.Host == "" ||
				(u.Scheme != "http" && u.Scheme != "https") {
				add("%s: backend.wings_url %q must be a URL such as \"https://node.example.com:8080\"",
					where, s.Backend.WingsURL)
			}
			switch {
			case s.Backend.UUID == "":
				add("%s: backend.uuid is required for the %s backend", where, kind)
			case shortIDRe.MatchString(s.Backend.UUID):
				add("%s: backend.uuid %q is the short id from the panel URL; Wings identifies a server "+
					"by its full UUID, which the panel shows on the server's settings page",
					where, s.Backend.UUID)
			case !uuidRe.MatchString(s.Backend.UUID):
				add("%s: backend.uuid %q is not a UUID such as "+
					"\"d3aac109-e5a0-4331-b03e-c8cec1cbfb0d\"", where, s.Backend.UUID)
			}
			if s.Backend.Token == "" {
				add("%s: backend.token is required for the %s backend: it is the \"token\" value in the "+
					"node's Wings config.yml", where, kind)
			}
			if s.Backend.Container != "" {
				add("%s: backend.container is not used by the %s backend", where, kind)
			}
			if len(s.Backend.Command) > 0 {
				add("%s: backend.command is not used by the %s backend", where, kind)
			}
		default:
			add("%s: backend.type %q is not one of %q, %q, %q, %q", where, s.Backend.Type,
				BackendDocker, BackendProcess, BackendPelican, BackendPterodactyl)
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
}

// UDPAddr is the endpoint napio guards while the server is asleep.
func (s Server) UDPAddr() string { return fmt.Sprintf("%s:%d", s.Address, s.Port) }
