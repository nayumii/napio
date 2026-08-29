package config

import (
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
