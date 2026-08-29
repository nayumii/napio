package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"git.naknu.li/emily/napio/internal/matchmaking"
)

// TestPublishAgainstTheRealService exercises the full listing lifecycle
// against multiplayer.factorio.com with a real factorio.com token.
//
// It is skipped unless a token is supplied, because it publishes a listing
// that real players can see:
//
//	NAPIO_INTEGRATION_FACTORIO_TOKEN=... \
//	NAPIO_INTEGRATION_FACTORIO_USERNAME=... \
//	NAPIO_INTEGRATION_PUBLIC_ADDRESS=203.0.113.7:34197 \
//	go test ./internal/integration/ -run RealService -v
//
// The listing is removed again in every exit path. Do not run this against an
// address where nothing can actually be woken.
func TestPublishAgainstTheRealService(t *testing.T) {
	token := os.Getenv("NAPIO_INTEGRATION_FACTORIO_TOKEN")
	username := os.Getenv("NAPIO_INTEGRATION_FACTORIO_USERNAME")
	address := os.Getenv("NAPIO_INTEGRATION_PUBLIC_ADDRESS")
	if token == "" || username == "" || address == "" {
		t.Skip("set NAPIO_INTEGRATION_FACTORIO_TOKEN, _USERNAME and _PUBLIC_ADDRESS to run this")
	}

	port, err := matchmaking.SplitHostPort(address)
	if err != nil {
		t.Fatalf("public address: %v", err)
	}

	client := matchmaking.NewClient("")
	game := matchmaking.Game{
		Name:        "napio integration test (please ignore)",
		Description: "automated test listing, removed within a minute",
		Tags:        []string{"napio", "test"},
		Version: matchmaking.Version{
			GameVersion: "2.0.77", BuildVersion: 84539,
			BuildMode: "headless", Platform: "linux64",
		},
		MaxPlayers:  1,
		HostAddress: address,
		HostPort:    port,
		Mods:        []matchmaking.Mod{{Name: "base", Version: "2.0.77"}},
	}

	ctx := context.Background()
	listing, err := client.Create(ctx, game, matchmaking.Credentials{Username: username, Token: token})
	if err != nil {
		t.Fatalf("create-game: %v", err)
	}
	t.Logf("published game_id=%d", listing.GameID)
	defer func() {
		if err := client.Remove(context.Background(), listing); err != nil {
			t.Errorf("remove-game failed, the listing may linger: %v", err)
		}
	}()

	details := fetchDetails(t, listing.GameID)
	if details["name"] != game.Name {
		t.Errorf("the service should echo the advertised name, got %v", details["name"])
	}
	if details["host_address"] != address {
		t.Errorf("advertised address: got %v, want %s", details["host_address"], address)
	}
	if _, ok := details["game_time_elapsed"]; ok {
		t.Log("note: the service reports a game_time_elapsed napio did not send")
	}

	first, _ := details["last_heartbeat"].(float64)
	if err := client.Heartbeat(ctx, listing, nil); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	time.Sleep(2 * time.Second)
	if second, _ := fetchDetails(t, listing.GameID)["last_heartbeat"].(float64); second <= first {
		t.Errorf("the heartbeat should refresh the listing: %v -> %v", first, second)
	}
}

func fetchDetails(t *testing.T, gameID uint32) map[string]any {
	t.Helper()
	url := matchmaking.DefaultBaseURL + "/get-game-details/" + strconv.FormatUint(uint64(gameID), 10)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get-game-details: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("get-game-details: %s", body)
	}
	return out
}
