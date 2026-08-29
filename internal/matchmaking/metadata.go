package matchmaking

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ServerSettings is the part of Factorio's server-settings.json that a listing
// needs. napio reads the very same file the server reads, so the advertised
// name, description, tags and player limit cannot drift from reality.
type ServerSettings struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Tags         []string `json:"tags"`
	MaxPlayers   int      `json:"max_players"`
	GamePassword string   `json:"game_password"`
	Username     string   `json:"username"`
	Token        string   `json:"token"`
	Visibility   struct {
		Public bool `json:"public"`
		LAN    bool `json:"lan"`
	} `json:"visibility"`
}

// LoadServerSettings reads a Factorio server-settings.json.
func LoadServerSettings(path string) (*ServerSettings, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading server settings: %w", err)
	}
	var out ServerSettings
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if out.Name == "" {
		return nil, fmt.Errorf("%s: the server has no name, so there is nothing honest to advertise", path)
	}
	return &out, nil
}

// modList mirrors Factorio's mods/mod-list.json.
type modList struct {
	Mods []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	} `json:"mods"`
}

var modArchiveRe = regexp.MustCompile(`^(.+)_(\d+\.\d+\.\d+)\.zip$`)

// LoadMods reports the enabled mods exactly as the server would load them.
//
// Versions come from the mod archives in the directory. Mods with no archive
// are the base game and its DLC, which a real server advertises at the game's
// own version — that is what was observed on the wire, so that is what napio
// reproduces.
func LoadMods(modsDir, gameVersion string) ([]Mod, error) {
	raw, err := os.ReadFile(filepath.Join(modsDir, "mod-list.json"))
	if err != nil {
		return nil, fmt.Errorf("reading mod list: %w", err)
	}
	var list modList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parsing mod-list.json: %w", err)
	}

	versions := map[string]string{}
	entries, err := os.ReadDir(modsDir)
	if err != nil {
		return nil, fmt.Errorf("reading mods directory: %w", err)
	}
	for _, entry := range entries {
		if m := modArchiveRe.FindStringSubmatch(entry.Name()); m != nil {
			versions[m[1]] = m[2]
		}
	}

	var mods []Mod
	for _, entry := range list.Mods {
		if !entry.Enabled {
			continue
		}
		version, ok := versions[entry.Name]
		if !ok {
			version = gameVersion
		}
		mods = append(mods, Mod{Name: entry.Name, Version: version})
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })
	return mods, nil
}

// versionBannerRe matches the line every Factorio server prints on startup:
//
//	0.000 2026-08-29 13:59:00; Factorio 2.0.77 (build 84539, linux64, headless, space-age)
var versionBannerRe = regexp.MustCompile(
	`Factorio (\d+\.\d+\.\d+) \(build (\d+), ([a-z0-9]+), ([a-z]+)`)

// ParseVersionBanner extracts the exact build from a server log.
//
// This is the only trustworthy source for build_version and platform while the
// server is off: RCON reports the game version but not the build. Reading it
// from the server's own log means napio advertises the build that is actually
// installed, and refuses to advertise anything when it has never seen one.
func ParseVersionBanner(log string) (Version, bool) {
	matches := versionBannerRe.FindAllStringSubmatch(log, -1)
	if len(matches) == 0 {
		return Version{}, false
	}
	m := matches[len(matches)-1] // the most recent run wins
	build, err := strconv.Atoi(m[2])
	if err != nil {
		return Version{}, false
	}
	return Version{
		GameVersion:  m[1],
		BuildVersion: build,
		Platform:     m[3],
		BuildMode:    m[4],
	}, true
}

// BuildGame assembles the listing from sources that can all be pointed at.
func BuildGame(settings *ServerSettings, version Version, mods []Mod, hostAddress string, port int, note string) Game {
	description := settings.Description
	if note != "" {
		if description != "" {
			description += " "
		}
		description += note
	}
	return Game{
		Name:        settings.Name,
		Description: description,
		Tags:        settings.Tags,
		Version:     version,
		MaxPlayers:  settings.MaxPlayers,
		HasPassword: settings.GamePassword != "",
		HostAddress: hostAddress,
		HostPort:    port,
		Mods:        mods,
		Headless:    true,
	}
}

// SplitHostPort returns the port half of an "address:port" string.
func SplitHostPort(address string) (int, error) {
	idx := strings.LastIndex(address, ":")
	if idx < 0 {
		return 0, fmt.Errorf("public_address %q must be host:port", address)
	}
	port, err := strconv.Atoi(address[idx+1:])
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("public_address %q has no valid port", address)
	}
	return port, nil
}
