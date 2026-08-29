package matchmaking

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testToken = "s3cret-token-never-logged"

// labService mimics the real matching service closely enough to test against,
// including the shapes observed on the wire.
type labService struct {
	mu       sync.Mutex
	created  []createRequest
	beats    int
	removed  []uint32
	failWith int
	body     string
}

func newLab(t *testing.T) (*labService, *Client) {
	t.Helper()
	lab := &labService{}
	srv := httptest.NewServer(http.HandlerFunc(lab.handle))
	t.Cleanup(srv.Close)
	return lab, &Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func (l *labService) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.failWith != 0 {
		w.WriteHeader(l.failWith)
		io.WriteString(w, l.body)
		return
	}

	switch {
	case r.URL.Path == "/create-game":
		var req createRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		l.created = append(l.created, req)
		// The live service answers with its own server_secret, which a real
		// server ignores; make sure napio ignores it too.
		io.WriteString(w, `{"game_id":1310675,"server_id":"abc","server_secret":"not-the-one-we-use"}`)
	case strings.HasPrefix(r.URL.Path, "/post-game-heartbeat/"):
		l.beats++
		io.WriteString(w, `{"status":"ok"}`)
	case strings.HasPrefix(r.URL.Path, "/remove-game/"):
		l.removed = append(l.removed, 1310675)
		io.WriteString(w, `{"status":"ok"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func testGame() Game {
	return Game{
		Name:        "test server",
		Description: "a description",
		Version:     Version{GameVersion: "2.0.77", BuildVersion: 84539, BuildMode: "headless", Platform: "linux64"},
		MaxPlayers:  8,
		HostAddress: "203.0.113.7:34197",
		HostPort:    34197,
	}
}

func TestCreateSendsWhatARealServerSends(t *testing.T) {
	lab, client := newLab(t)

	listing, err := client.Create(context.Background(), testGame(),
		Credentials{Username: "someone", Token: testToken})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if listing.GameID != 1310675 {
		t.Fatalf("game_id: got %d", listing.GameID)
	}
	// The secret napio keeps is its own, not the service's.
	if listing.Secret == "not-the-one-we-use" || len(listing.Secret) != 32 {
		t.Fatalf("expected a 32-character self-generated secret, got %q", listing.Secret)
	}

	lab.mu.Lock()
	defer lab.mu.Unlock()
	req := lab.created[0]
	if req.Game.GameID != noGameID {
		t.Errorf("create-game must send the 0xFFFFFFFF sentinel, got %d", req.Game.GameID)
	}
	if req.Game.GameSecret != listing.Secret {
		t.Error("the generated secret must be sent in create-game")
	}
	if !req.Game.Headless {
		t.Error("headless_server must be true")
	}
	if req.Username != "someone" || req.Token != testToken {
		t.Error("credentials must be sent in the create-game body")
	}
}

func TestGameTimeElapsedIsNeverInvented(t *testing.T) {
	// napio cannot observe the tick count while the server is off, so the
	// field must simply not be advertised.
	encoded, err := json.Marshal(testGame())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "game_time_elapsed") {
		t.Fatalf("a listing must not advertise a made-up game time: %s", encoded)
	}
}

func TestHeartbeatAndRemove(t *testing.T) {
	lab, client := newLab(t)
	listing := Listing{GameID: 1310675, Secret: "abc"}

	if err := client.Heartbeat(context.Background(), listing, nil); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := client.Remove(context.Background(), listing); err != nil {
		t.Fatalf("remove: %v", err)
	}
	lab.mu.Lock()
	defer lab.mu.Unlock()
	if lab.beats != 1 || len(lab.removed) != 1 {
		t.Fatalf("expected one heartbeat and one removal, got %d/%d", lab.beats, len(lab.removed))
	}
}

func TestRejectedTokenIsFatalAndNotEchoed(t *testing.T) {
	lab, client := newLab(t)
	lab.failWith = http.StatusUnauthorized
	lab.body = `{"message":"invalid token"}`

	_, err := client.Create(context.Background(), testGame(),
		Credentials{Username: "someone", Token: testToken})
	if err == nil {
		t.Fatal("expected an error")
	}
	var httpErr *HTTPError
	if !errorsAs(err, &httpErr) {
		t.Fatalf("expected an *HTTPError, got %T", err)
	}
	if !httpErr.Fatal() {
		t.Error("a rejected token must be fatal: retrying it forever helps nobody")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("the token leaked into an error message: %v", err)
	}
}

func TestServiceErrorsAreRetryable(t *testing.T) {
	lab, client := newLab(t)
	lab.failWith = http.StatusInternalServerError
	lab.body = "upstream unavailable"

	_, err := client.Create(context.Background(), testGame(), Credentials{Username: "u", Token: testToken})
	var httpErr *HTTPError
	if !errorsAs(err, &httpErr) || httpErr.Fatal() {
		t.Fatalf("a 500 must be retryable, got %v", err)
	}
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }
