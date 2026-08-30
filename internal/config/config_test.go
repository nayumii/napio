package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalConfig = `
servers:
  - name: my-factorio-server
    address: 0.0.0.0
    port: 34197
    inactivity_timeout: 600
    rcon:
      address: 127.0.0.1:27015
      password: secret
    backend:
      type: docker
      container: factorio
`

func TestParseMinimalConfigAndDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("expected one server, got %d", len(cfg.Servers))
	}
	s := cfg.Servers[0]
	if s.InactivityTimeout.Duration() != 10*time.Minute {
		t.Errorf("inactivity_timeout: got %s, want 10m", s.InactivityTimeout)
	}
	if s.StartTimeout.Duration() != DefaultStartTimeout {
		t.Errorf("start_timeout default not applied: %s", s.StartTimeout)
	}
	if s.Wake.Packets != DefaultWakePackets {
		t.Errorf("wake.packets default not applied: %d", s.Wake.Packets)
	}
	if s.UDPAddr() != "0.0.0.0:34197" {
		t.Errorf("UDPAddr: got %s", s.UDPAddr())
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level default not applied: %s", cfg.LogLevel)
	}
}

func TestDefaultsSectionAppliesToServers(t *testing.T) {
	cfg, err := Parse([]byte(`
defaults:
  inactivity_timeout: 30m
  poll_interval: 5s
servers:
  - name: a
    port: 34197
    backend: {type: docker, container: a}
  - name: b
    port: 34198
    inactivity_timeout: 60
    backend: {type: docker, container: b}
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Servers[0].InactivityTimeout.Duration(); got != 30*time.Minute {
		t.Errorf("server a should inherit the default: got %s", got)
	}
	if got := cfg.Servers[1].InactivityTimeout.Duration(); got != time.Minute {
		t.Errorf("server b should override the default: got %s", got)
	}
	if got := cfg.Servers[1].PollInterval.Duration(); got != 5*time.Second {
		t.Errorf("poll_interval should be inherited: got %s", got)
	}
}

func TestDurationAcceptsSecondsAndDurations(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"600", 10 * time.Minute},
		{"10m", 10 * time.Minute},
		{"90s", 90 * time.Second},
		{"1h30m", 90 * time.Minute},
	} {
		got, err := ParseDuration(tc.in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %s, want %s", tc.in, got, tc.want)
		}
	}
	if _, err := ParseDuration("soon"); err == nil {
		t.Error("expected an error for a nonsense duration")
	}
}

func TestSecretsComeFromTheEnvironment(t *testing.T) {
	t.Setenv("NAPIO_TEST_RCON", "hunter2")
	cfg, err := Parse([]byte(`
servers:
  - name: a
    port: 34197
    rcon: {address: "127.0.0.1:27015", password: "${NAPIO_TEST_RCON}"}
    backend: {type: docker, container: a}
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Servers[0].RCON.Password; got != "hunter2" {
		t.Errorf("password: got %q", got)
	}
}

func TestMissingEnvironmentVariableIsAnError(t *testing.T) {
	_, err := Parse([]byte(`
servers:
  - name: a
    port: 34197
    rcon: {address: "127.0.0.1:27015", password: "${NAPIO_DEFINITELY_UNSET_VAR}"}
    backend: {type: docker, container: a}
`))
	if err == nil || !strings.Contains(err.Error(), "NAPIO_DEFINITELY_UNSET_VAR") {
		t.Fatalf("expected an error naming the missing variable, got %v", err)
	}
}

func TestInvalidConfigurations(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "no servers",
			yaml: "servers: []",
			want: "at least one server",
		},
		{
			name: "missing name",
			yaml: "servers:\n  - port: 34197\n    backend: {type: docker, container: a}",
			want: "name: must not be empty",
		},
		{
			name: "duplicate name",
			yaml: "servers:\n  - {name: a, port: 34197, backend: {type: docker, container: a}}\n  - {name: a, port: 34198, backend: {type: docker, container: b}}",
			want: "duplicate server name",
		},
		{
			name: "duplicate endpoint",
			yaml: "servers:\n  - {name: a, port: 34197, backend: {type: docker, container: a}}\n  - {name: b, port: 34197, backend: {type: docker, container: b}}",
			want: "already used by server a",
		},
		{
			name: "port out of range",
			yaml: "servers:\n  - {name: a, port: 99999, backend: {type: docker, container: a}}",
			want: "out of range",
		},
		{
			name: "missing port",
			yaml: "servers:\n  - {name: a, backend: {type: docker, container: a}}",
			want: "out of range",
		},
		{
			name: "bad bind address",
			yaml: "servers:\n  - {name: a, address: not-an-ip, port: 34197, backend: {type: docker, container: a}}",
			want: "not a valid IP address",
		},
		{
			name: "unknown backend",
			yaml: "servers:\n  - {name: a, port: 34197, backend: {type: kubernetes}}",
			want: "backend.type",
		},
		{
			name: "docker without container",
			yaml: "servers:\n  - {name: a, port: 34197, backend: {type: docker}}",
			want: "backend.container is required",
		},
		{
			name: "process without command",
			yaml: "servers:\n  - {name: a, port: 34197, backend: {type: process}}",
			want: "backend.command is required",
		},
		{
			name: "rcon without password",
			yaml: "servers:\n  - {name: a, port: 34197, rcon: {address: \"127.0.0.1:27015\"}, backend: {type: docker, container: a}}",
			want: "rcon.password is required",
		},
		{
			name: "poll interval longer than the timeout",
			yaml: "servers:\n  - {name: a, port: 34197, inactivity_timeout: 10, poll_interval: 60, backend: {type: docker, container: a}}",
			want: "must be shorter than inactivity_timeout",
		},
		{
			name: "bad allow_from",
			yaml: "servers:\n  - {name: a, port: 34197, wake: {allow_from: [\"192.168.0.1\"]}, backend: {type: docker, container: a}}",
			want: "is not a CIDR",
		},
		{
			name: "unknown field",
			yaml: "servers:\n  - {name: a, port: 34197, inactivity_timeut: 10, backend: {type: docker, container: a}}",
			want: "field inactivity_timeut not found",
		},
		{
			name: "bad log level",
			yaml: "log_level: chatty\nservers:\n  - {name: a, port: 34197, backend: {type: docker, container: a}}",
			want: "log_level",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestExampleConfigurationIsValid(t *testing.T) {
	t.Setenv("FACTORIO_RCON_PASSWORD", "test")
	t.Setenv("NAPIO_API_TOKEN", "test")
	if _, err := Load("../../examples/napio.yaml"); err != nil {
		t.Fatalf("the shipped example configuration must be valid: %v", err)
	}
}

func TestMatchmakingIsOffByDefault(t *testing.T) {
	cfg, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Servers[0].Matchmaking.Enabled {
		t.Fatal("enabling matchmaking advertises a server publicly; it must never happen by accident")
	}
}

func TestMatchmakingValidation(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "server-settings.json")
	if err := os.WriteFile(settings, []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	base := `
servers:
  - name: a
    port: 34197
    backend: {type: docker, container: a}
    matchmaking:
      enabled: true
`
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing public address",
			yaml: base + "      server_settings: " + settings + "\n",
			want: "public_address is required",
		},
		{
			name: "malformed public address",
			yaml: base + "      server_settings: " + settings + "\n      public_address: \"203.0.113.7\"\n",
			want: "must be host:port",
		},
		{
			name: "missing settings path",
			yaml: base + "      public_address: \"203.0.113.7:34197\"\n",
			want: "needs data_dir",
		},
		{
			name: "unreadable settings",
			yaml: base + "      public_address: \"203.0.113.7:34197\"\n      server_settings: /nope/server-settings.json\n",
			want: "cannot be read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.yaml)); err == nil {
				t.Fatal("expected an error")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	valid := base + "      public_address: \"203.0.113.7:34197\"\n      data_dir: " + dir + "\n"
	// data_dir derives config/server-settings.json, which does not exist here.
	if _, err := Parse([]byte(valid)); err == nil {
		t.Fatal("data_dir pointing at a directory without config/server-settings.json should fail")
	}

	good := base + "      public_address: \"203.0.113.7:34197\"\n      server_settings: " + settings + "\n"
	cfg, err := Parse([]byte(good))
	if err != nil {
		t.Fatalf("a complete matchmaking config should be valid: %v", err)
	}
	if got := cfg.Servers[0].Matchmaking.Note(); got != DefaultSleepingNote {
		t.Errorf("default sleeping note: got %q", got)
	}
}

func TestMatchmakingTokenComesFromTheEnvironment(t *testing.T) {
	t.Setenv("NAPIO_TEST_FACTORIO_TOKEN", "tok-from-env")
	dir := t.TempDir()
	settings := filepath.Join(dir, "server-settings.json")
	if err := os.WriteFile(settings, []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte(`
servers:
  - name: a
    port: 34197
    backend: {type: docker, container: a}
    matchmaking:
      enabled: true
      public_address: "203.0.113.7:34197"
      server_settings: ` + settings + `
      token: "${NAPIO_TEST_FACTORIO_TOKEN}"
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Servers[0].Matchmaking.Token != "tok-from-env" {
		t.Fatalf("token: got %q", cfg.Servers[0].Matchmaking.Token)
	}
}

func TestConfigJSONNeverExposesSecrets(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "server-settings.json")
	if err := os.WriteFile(settings, []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte(`
api: {enabled: true, token: "api-secret"}
servers:
  - name: a
    port: 34197
    rcon: {address: "127.0.0.1:27015", password: "rcon-secret"}
    backend: {type: docker, container: a}
    matchmaking:
      enabled: true
      public_address: "203.0.113.7:34197"
      server_settings: ` + settings + `
      token: "mm-secret"
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"api-secret", "rcon-secret", "mm-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("%s leaked into the serialised configuration: %s", secret, encoded)
		}
	}
}

const pelicanConfig = `
servers:
  - name: my-factorio-server
    port: 34197
    rcon:
      address: 127.0.0.1:27015
      password: secret
    backend:
      type: pelican
      wings_url: https://node.example.com:8080
      uuid: d3aac109-e5a0-4331-b03e-c8cec1cbfb0d
      token: node-token
`

func TestPelicanBackendParses(t *testing.T) {
	cfg, err := Parse([]byte(pelicanConfig))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b := cfg.Servers[0].Backend
	if b.Type != BackendPelican {
		t.Errorf("backend.type: got %q", b.Type)
	}
	if b.WingsURL != "https://node.example.com:8080" {
		t.Errorf("wings_url: got %q", b.WingsURL)
	}
	if b.UUID != "d3aac109-e5a0-4331-b03e-c8cec1cbfb0d" {
		t.Errorf("uuid: got %q", b.UUID)
	}
	if b.Token != "node-token" {
		t.Errorf("token: got %q", b.Token)
	}
}

func TestPterodactylIsSpeltDifferentlyAndWorksTheSame(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(pelicanConfig, "type: pelican", "type: pterodactyl", 1)))
	if err != nil {
		t.Fatalf("pterodactyl must be accepted as a spelling of the same backend: %v", err)
	}
	if cfg.Servers[0].Backend.Type != BackendPterodactyl {
		t.Errorf("backend.type: got %q", cfg.Servers[0].Backend.Type)
	}
}

func TestPelicanTokenComesFromTheEnvironment(t *testing.T) {
	t.Setenv("NAPIO_TEST_WINGS_TOKEN", "from-the-environment")
	cfg, err := Parse([]byte(strings.Replace(pelicanConfig,
		"token: node-token", `token: "${NAPIO_TEST_WINGS_TOKEN}"`, 1)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Servers[0].Backend.Token; got != "from-the-environment" {
		t.Errorf("backend.token was not expanded: %q", got)
	}
}

func TestPelicanNodeTokenNeverReachesTheAPI(t *testing.T) {
	cfg, err := Parse([]byte(pelicanConfig))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"node-token", "node.example.com"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("%q leaked into the serialised configuration: %s", secret, encoded)
		}
	}
}

func TestInvalidPelicanConfigurations(t *testing.T) {
	base := "servers:\n  - {name: a, port: 34197, backend: {type: pelican, %s}}"
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "short id instead of the uuid",
			body: "wings_url: 'https://n:8080', uuid: 1a7ce997, token: t",
			want: "short id from the panel URL",
		},
		{
			name: "not a uuid at all",
			body: "wings_url: 'https://n:8080', uuid: my-server, token: t",
			want: "is not a UUID",
		},
		{
			name: "missing uuid",
			body: "wings_url: 'https://n:8080', token: t",
			want: "backend.uuid is required",
		},
		{
			name: "missing token",
			body: "wings_url: 'https://n:8080', uuid: d3aac109-e5a0-4331-b03e-c8cec1cbfb0d",
			want: "config.yml",
		},
		{
			name: "missing wings_url",
			body: "uuid: d3aac109-e5a0-4331-b03e-c8cec1cbfb0d, token: t",
			want: "backend.wings_url is required",
		},
		{
			name: "wings_url without a scheme",
			body: "wings_url: 'node.example.com:8080', uuid: d3aac109-e5a0-4331-b03e-c8cec1cbfb0d, token: t",
			want: "must be a URL",
		},
		{
			name: "container belongs to the docker backend",
			body: "wings_url: 'https://n:8080', uuid: d3aac109-e5a0-4331-b03e-c8cec1cbfb0d, token: t, container: factorio",
			want: "backend.container is not used",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(fmt.Sprintf(base, tc.body)))
			if err == nil {
				t.Fatal("expected the configuration to be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestPelicanKeysAreRefusedOnOtherBackends(t *testing.T) {
	// KnownFields catches a misspelt key, but not a correctly spelt one that
	// this backend will never read.
	_, err := Parse([]byte("servers:\n  - {name: a, port: 34197, backend: {type: docker, container: a, wings_url: 'https://n:8080'}}"))
	if err == nil || !strings.Contains(err.Error(), "backend.wings_url is not used by the docker backend") {
		t.Fatalf("expected wings_url to be refused on the docker backend, got %v", err)
	}
}
