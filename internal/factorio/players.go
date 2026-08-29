// Package factorio contains the only Factorio-specific knowledge in napio:
// how to tell that a server has finished loading, how many players are on it,
// and how to shut it down without losing the save.
package factorio

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PlayersOnlineCommand is a plain console command. napio deliberately never
// uses /c or silent-command: running Lua would flag the save as
// command-modified and disable achievements on the player's server.
const PlayersOnlineCommand = "/players online"

var playersHeaderRe = regexp.MustCompile(`^[^(]*\((\d+)\)\s*:?\s*$`)

// Players is the result of a player-count probe.
type Players struct {
	Count int      `json:"count"`
	Names []string `json:"names"`
}

// ParsePlayersOnline parses the reply to "/players online".
//
// Observed on Factorio 2.0.77:
//
//	"Online players (0):\n"
//	"Online players (2):\n  Alice (online)\n  Bob (online)\n"
//
// The count comes from the header, which is the stable part; names are
// best-effort and are never used for control decisions.
func ParsePlayersOnline(reply string) (Players, error) {
	lines := strings.Split(strings.TrimRight(reply, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return Players{}, fmt.Errorf("unexpected empty reply to %q", PlayersOnlineCommand)
	}
	m := playersHeaderRe.FindStringSubmatch(strings.TrimSpace(lines[0]))
	if m == nil {
		return Players{}, fmt.Errorf("unrecognised reply to %q: %q", PlayersOnlineCommand, reply)
	}
	count, err := strconv.Atoi(m[1])
	if err != nil {
		return Players{}, fmt.Errorf("unrecognised player count in %q: %w", lines[0], err)
	}

	var names []string
	for _, line := range lines[1:] {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		name = strings.TrimSuffix(name, " (online)")
		names = append(names, strings.TrimSpace(name))
	}
	return Players{Count: count, Names: names}, nil
}
