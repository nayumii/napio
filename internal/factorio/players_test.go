package factorio

import "testing"

func TestParsePlayersOnline(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		count int
		names []string
	}{
		{
			// Captured from a live Factorio 2.0.77 headless server.
			name:  "empty server",
			reply: "Online players (0):\n",
			count: 0,
		},
		{
			name:  "two players",
			reply: "Online players (2):\n  Alice (online)\n  Bob (online)\n",
			count: 2,
			names: []string{"Alice", "Bob"},
		},
		{
			name:  "no trailing newline",
			reply: "Online players (1):\n  Solo (online)",
			count: 1,
			names: []string{"Solo"},
		},
		{
			name:  "all players variant",
			reply: "Players (3):\n  Alice (online)\n  Bob\n  Carol\n",
			count: 3,
			names: []string{"Alice", "Bob", "Carol"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePlayersOnline(tc.reply)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Count != tc.count {
				t.Errorf("count: got %d, want %d", got.Count, tc.count)
			}
			if len(got.Names) != len(tc.names) {
				t.Fatalf("names: got %v, want %v", got.Names, tc.names)
			}
			for i := range tc.names {
				if got.Names[i] != tc.names[i] {
					t.Errorf("names[%d]: got %q, want %q", i, got.Names[i], tc.names[i])
				}
			}
		})
	}
}

func TestParsePlayersOnlineRejectsGarbage(t *testing.T) {
	// A misparse must be an error, never a silent zero: "zero players" is the
	// signal that shuts a server down.
	for _, reply := range []string{"", "\n", "Unknown command", "Online players:"} {
		if _, err := ParsePlayersOnline(reply); err == nil {
			t.Errorf("expected an error for %q", reply)
		}
	}
}
