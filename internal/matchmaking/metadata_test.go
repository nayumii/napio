package matchmaking

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadServerSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server-settings.json")
	writeFile(t, path, `{
      "name": "My Factory",
      "description": "come and build",
      "tags": ["vanilla", "eu"],
      "max_players": 12,
      "game_password": "hunter2",
      "username": "someone",
      "token": "tok",
      "visibility": {"public": true, "lan": true}
    }`)

	settings, err := LoadServerSettings(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if settings.Name != "My Factory" || settings.MaxPlayers != 12 {
		t.Fatalf("unexpected settings: %+v", settings)
	}
	if !settings.Visibility.Public {
		t.Error("visibility.public should be parsed")
	}

	game := BuildGame(settings, Version{GameVersion: "2.0.77"}, nil, "203.0.113.7:34197", 34197, "[asleep]")
	if !game.HasPassword {
		t.Error("a server with a game_password must be advertised as password-protected")
	}
	if game.Description != "come and build [asleep]" {
		t.Errorf("the sleeping note should be appended, got %q", game.Description)
	}
	if game.Name != "My Factory" {
		t.Errorf("the advertised name must come from the server settings, got %q", game.Name)
	}
}

func TestServerSettingsWithoutANameIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server-settings.json")
	writeFile(t, path, `{"name": ""}`)
	if _, err := LoadServerSettings(path); err == nil {
		t.Fatal("a nameless server has nothing honest to advertise")
	}
}

func TestLoadMods(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mod-list.json"), `{"mods":[
      {"name":"base","enabled":true},
      {"name":"space-age","enabled":true},
      {"name":"Krastorio2","enabled":true},
      {"name":"disabled-thing","enabled":false}
    ]}`)
	writeFile(t, filepath.Join(dir, "Krastorio2_1.3.25.zip"), "")

	mods, err := LoadMods(dir, "2.0.77")
	if err != nil {
		t.Fatalf("load mods: %v", err)
	}
	got := map[string]string{}
	for _, m := range mods {
		got[m.Name] = m.Version
	}
	if len(got) != 3 {
		t.Fatalf("disabled mods must not be advertised: %v", got)
	}
	if got["Krastorio2"] != "1.3.25" {
		t.Errorf("mod version should come from the archive name, got %q", got["Krastorio2"])
	}
	// base and DLC ship with the game and are advertised at the game version,
	// which is what a real server sends.
	if got["base"] != "2.0.77" || got["space-age"] != "2.0.77" {
		t.Errorf("base and DLC should carry the game version, got %v", got)
	}
}

func TestLoadModsFailsLoudly(t *testing.T) {
	// Silently advertising "no mods" for a modded server would mislead players
	// browsing with a mod filter.
	if _, err := LoadMods(t.TempDir(), "2.0.77"); err == nil {
		t.Fatal("a missing mod-list.json must be an error, not an empty mod list")
	}
}

func TestParseVersionBanner(t *testing.T) {
	// Captured verbatim from a factoriotools/factorio container log.
	log := `   0.000 2026-08-29 13:59:00; Factorio 2.0.77 (build 84539, linux64, headless, space-age)
   0.000 Operating system: Linux
   0.712 Info UDPSocket.cpp:38: Opening socket at (IP ADDR:({0.0.0.0:34197}))`

	version, ok := ParseVersionBanner(log)
	if !ok {
		t.Fatal("the banner should be recognised")
	}
	if version.GameVersion != "2.0.77" || version.BuildVersion != 84539 ||
		version.Platform != "linux64" || version.BuildMode != "headless" {
		t.Fatalf("unexpected version: %+v", version)
	}
}

func TestParseVersionBannerPrefersTheMostRecentRun(t *testing.T) {
	log := "Factorio 2.0.60 (build 80000, linux64, headless)\nFactorio 2.0.77 (build 84539, linux64, headless)"
	version, ok := ParseVersionBanner(log)
	if !ok || version.BuildVersion != 84539 {
		t.Fatalf("the latest run should win, got %+v", version)
	}
}

func TestParseVersionBannerRefusesToGuess(t *testing.T) {
	if _, ok := ParseVersionBanner("no banner here"); ok {
		t.Fatal("without a banner napio must report that it does not know the version")
	}
}

func TestSplitHostPort(t *testing.T) {
	if port, err := SplitHostPort("203.0.113.7:34197"); err != nil || port != 34197 {
		t.Fatalf("got %d, %v", port, err)
	}
	if _, err := SplitHostPort("203.0.113.7"); err == nil {
		t.Error("an address without a port must be rejected")
	}
	if _, err := SplitHostPort("example.org:nope"); err == nil {
		t.Error("a non-numeric port must be rejected")
	}
}

func TestSecretsAreDistinctAndCorrectlyShaped(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s, err := NewSecret()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 32 {
			t.Fatalf("expected 32 characters, got %d", len(s))
		}
		if strings.ContainsAny(s, "-_/+= ") {
			t.Fatalf("unexpected characters in %q", s)
		}
		if seen[s] {
			t.Fatal("secrets must not repeat")
		}
		seen[s] = true
	}
}
