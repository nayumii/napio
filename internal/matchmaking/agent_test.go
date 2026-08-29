package matchmaking

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestAgent(t *testing.T, lab *labService, client *Client) *Agent {
	t.Helper()
	agent := NewAgent(client, Credentials{Username: "someone", Token: testToken},
		func() (Game, error) { return testGame(), nil }, quietLogger())
	agent.beatEvery = 20 * time.Millisecond
	agent.backoffMin = 20 * time.Millisecond
	t.Cleanup(agent.Withdraw)
	return agent
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAgentPublishesAndHeartbeats(t *testing.T) {
	lab, client := newLab(t)
	agent := newTestAgent(t, lab, client)

	agent.Publish()

	waitFor(t, "the listing to be created", func() bool { return agent.Status().Listed })
	waitFor(t, "heartbeats to flow", func() bool {
		lab.mu.Lock()
		defer lab.mu.Unlock()
		return lab.beats >= 3
	})

	status := agent.Status()
	if status.GameID != 1310675 {
		t.Fatalf("status should report the game id, got %d", status.GameID)
	}
	if status.LastHeartbeat == nil {
		t.Error("status should report when the listing was last confirmed")
	}
}

func TestAgentWithdrawsTheListing(t *testing.T) {
	lab, client := newLab(t)
	agent := newTestAgent(t, lab, client)

	agent.Publish()
	waitFor(t, "the listing to be created", func() bool { return agent.Status().Listed })

	agent.Withdraw()

	lab.mu.Lock()
	defer lab.mu.Unlock()
	if len(lab.removed) != 1 {
		t.Fatalf("withdrawing must remove the listing, got %d removals", len(lab.removed))
	}
	if agent.Status().Listed {
		t.Error("status must no longer claim to be listed")
	}
}

func TestPublishIsIdempotent(t *testing.T) {
	lab, client := newLab(t)
	agent := newTestAgent(t, lab, client)

	for i := 0; i < 5; i++ {
		agent.Publish()
	}
	waitFor(t, "the listing to be created", func() bool { return agent.Status().Listed })
	time.Sleep(100 * time.Millisecond)

	lab.mu.Lock()
	defer lab.mu.Unlock()
	if len(lab.created) != 1 {
		t.Fatalf("repeated Publish calls must not create duplicate listings, got %d", len(lab.created))
	}
}

func TestAgentRepublishesAfterAFailedHeartbeat(t *testing.T) {
	// The service drops listings it has not heard from; a failed heartbeat
	// must lead to a fresh listing rather than silent invisibility.
	lab, client := newLab(t)
	agent := newTestAgent(t, lab, client)

	agent.Publish()
	waitFor(t, "the first listing", func() bool { return agent.Status().Listed })

	lab.mu.Lock()
	lab.failWith = http.StatusInternalServerError
	lab.mu.Unlock()

	waitFor(t, "the failure to be reported", func() bool { return agent.Status().LastError != "" })

	lab.mu.Lock()
	lab.failWith = 0
	lab.mu.Unlock()

	waitFor(t, "a replacement listing", func() bool {
		lab.mu.Lock()
		defer lab.mu.Unlock()
		return len(lab.created) >= 2
	})
}

func TestAgentGivesUpOnARejectedToken(t *testing.T) {
	lab, client := newLab(t)
	lab.failWith = http.StatusUnauthorized
	lab.body = `{"message":"invalid token"}`
	agent := newTestAgent(t, lab, client)

	agent.Publish()

	waitFor(t, "the rejection to be reported", func() bool {
		return strings.Contains(agent.Status().LastError, "credentials")
	})
	time.Sleep(100 * time.Millisecond)

	lab.mu.Lock()
	attempts := len(lab.created)
	lab.mu.Unlock()
	if attempts != 0 {
		t.Fatalf("a rejected token must not be retried in a loop (%d create attempts)", attempts)
	}
	if strings.Contains(agent.Status().LastError, testToken) {
		t.Fatal("the token leaked into the status")
	}
}

func TestAgentSurvivesAFailingMetadataSource(t *testing.T) {
	// A missing server-settings.json must not take napio down; it just means
	// nothing gets advertised.
	_, client := newLab(t)
	agent := NewAgent(client, Credentials{Username: "u", Token: testToken},
		func() (Game, error) { return Game{}, io.ErrUnexpectedEOF }, quietLogger())
	agent.beatEvery = 20 * time.Millisecond
	agent.backoffMin = 20 * time.Millisecond
	defer agent.Withdraw()

	agent.Publish()
	waitFor(t, "the error to be reported", func() bool { return agent.Status().LastError != "" })
	if agent.Status().Listed {
		t.Fatal("nothing should be advertised when the metadata cannot be read")
	}
}

func TestWithdrawWithoutPublishIsSafe(t *testing.T) {
	lab, client := newLab(t)
	agent := newTestAgent(t, lab, client)
	agent.Withdraw()
	agent.Withdraw()
}

func TestStatusNeverContainsCredentials(t *testing.T) {
	lab, client := newLab(t)
	agent := newTestAgent(t, lab, client)
	agent.Publish()
	waitFor(t, "the listing", func() bool { return agent.Status().Listed })

	encoded, err := marshalStatus(agent.Status())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testToken, agent.listing.Secret} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("a secret leaked into the serialised status: %s", encoded)
		}
	}
}

func marshalStatus(s Status) (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}
