package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that accepts either a Go duration string
// ("10m", "30s") or a plain number of seconds ("600") in YAML.
//
// Plain seconds are supported because that is what most game-server configs
// use, and because it is what people copy out of forum posts.
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("must be a duration such as \"10m\" or a number of seconds: %w", err)
	}
	parsed, err := ParseDuration(raw)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.String() + `"`), nil
}

// ParseDuration accepts "10m", "600" (seconds) or "600s".
func ParseDuration(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if seconds, err := parseSeconds(raw); err == nil {
		return seconds, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: use a duration such as \"10m\" or a number of seconds", raw)
	}
	return parsed, nil
}

func parseSeconds(raw string) (time.Duration, error) {
	var n int64
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, err
	}
	// Reject "10m" being read as 10: only accept if the whole string was digits.
	if fmt.Sprintf("%d", n) != raw {
		return 0, fmt.Errorf("not a plain number")
	}
	return time.Duration(n) * time.Second, nil
}
