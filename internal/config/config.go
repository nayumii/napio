// Package config loads and validates napio's YAML configuration.
//
// Validation is deliberately strict and happens once, at startup: a
// misconfigured gateway that fails immediately with a precise message is far
// friendlier than one that half-works and wakes nothing at 2am.
package config

import (
	"fmt"
	"net"
	"os"
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

	Wake    Wake    `yaml:"wake" json:"wake"`
	RCON    RCON    `yaml:"rcon" json:"-"`
	Backend Backend `yaml:"backend" json:"backend"`
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
}

const (
	BackendDocker  = "docker"
	BackendProcess = "process"
)

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

		switch s.Backend.Type {
		case BackendDocker:
			if s.Backend.Container == "" {
				add("%s: backend.container is required for the docker backend", where)
			}
			if len(s.Backend.Command) > 0 {
				add("%s: backend.command is not used by the docker backend", where)
			}
		case BackendProcess:
			if len(s.Backend.Command) == 0 {
				add("%s: backend.command is required for the process backend", where)
			}
			if s.Backend.Container != "" {
				add("%s: backend.container is not used by the process backend", where)
			}
		default:
			add("%s: backend.type %q is not one of %q, %q", where, s.Backend.Type, BackendDocker, BackendProcess)
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
}

// UDPAddr is the endpoint napio guards while the server is asleep.
func (s Server) UDPAddr() string { return fmt.Sprintf("%s:%d", s.Address, s.Port) }
