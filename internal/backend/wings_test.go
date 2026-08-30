package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWings is a stand-in for the daemon: it answers with a state that the
// test can change, and records the power actions it was sent.
type fakeWings struct {
	mu      sync.Mutex
	state   string
	actions []string

	// onAction lets a test move the state in response to a power action, the
	// way the real daemon does on its own goroutine.
	onAction func(action string) string

	suspended   bool
	allocIP     string
	allocPort   int
	noAllocs    bool
	logContents string

	// failStateTimes makes the next N state reads answer 500, to exercise the
	// retry that keeps a connection blip out of the supervisor's backoff.
	failStateTimes int
	stateReads     int
}

func (f *fakeWings) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer node-token" {
			t.Errorf("missing or wrong bearer token: %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("the daemon must be asked for JSON, got %q", got)
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/power"):
			var body struct {
				Action string `json:"action"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decoding the power request: %v", err)
			}
			f.actions = append(f.actions, body.Action)
			if f.onAction != nil {
				f.state = f.onAction(body.Action)
			}
			w.WriteHeader(http.StatusAccepted)

		case strings.HasSuffix(r.URL.Path, "/files/contents"):
			if got := r.URL.Query().Get("file"); got == "" {
				t.Error("the log request must name a file")
			}
			io.WriteString(w, f.logContents)

		default: // GET /api/servers/{uuid}
			f.stateReads++
			if f.stateReads <= f.failStateTimes {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, `{"error":"something broke"}`)
				return
			}
			resp := map[string]any{
				"state":        f.state,
				"is_suspended": f.suspended,
			}
			cfg := map[string]any{"crash_detection_enabled": true}
			if !f.noAllocs {
				cfg["allocations"] = map[string]any{
					"default": map[string]any{"ip": f.allocIP, "port": f.allocPort},
				}
			}
			resp["configuration"] = cfg
			json.NewEncoder(w).Encode(resp)
		}
	}
}

func (f *fakeWings) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

const testUUID = "d3aac109-e5a0-4331-b03e-c8cec1cbfb0d"

func newTestWings(t *testing.T, fake *fakeWings) *Wings {
	t.Helper()
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	w, err := NewWings(srv.URL, testUUID, "node-token", "", false, "0.0.0.0", 34197, testLogger())
	if err != nil {
		t.Fatalf("building wings backend: %v", err)
	}
	// Real timings would make these tests take a minute each.
	w.startAck = 300 * time.Millisecond
	w.poll = 5 * time.Millisecond
	w.killReserve = 300 * time.Millisecond
	w.minGrace = 20 * time.Millisecond
	return w
}

// newTestWingsWithHandler is for the error cases, where the daemon's reply
// matters more than its state.
func newTestWingsWithHandler(t *testing.T, handler http.HandlerFunc) *Wings {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	w, err := NewWings(srv.URL, testUUID, "node-token", "", false, "0.0.0.0", 34197, testLogger())
	if err != nil {
		t.Fatalf("building wings backend: %v", err)
	}
	w.startAck = 200 * time.Millisecond
	w.poll = 5 * time.Millisecond
	w.killReserve = 200 * time.Millisecond
	w.minGrace = 20 * time.Millisecond
	return w
}

func TestWingsIsRunningCountsEveryStateButOffline(t *testing.T) {
	// This is what the supervisor needs: "starting" must not read as a failed
	// launch, and "stopping" must not read as a finished shutdown.
	for state, want := range map[string]bool{
		stateOffline:  false,
		stateStarting: true,
		stateRunning:  true,
		stateStopping: true,
	} {
		fake := &fakeWings{state: state}
		w := newTestWings(t, fake)
		running, err := w.IsRunning(context.Background())
		if err != nil {
			t.Fatalf("state %q: IsRunning: %v", state, err)
		}
		if running != want {
			t.Errorf("state %q: IsRunning = %v, want %v", state, running, want)
		}
	}
}

func TestWingsStartWaitsForTheDaemonToActuallyStart(t *testing.T) {
	// POST /power answers 202 before anything has happened, so a bare 202 is
	// not evidence of a start.
	fake := &fakeWings{
		state:    stateOffline,
		onAction: func(string) string { return stateStarting },
	}
	w := newTestWings(t, fake)

	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := fake.sent(); len(got) != 1 || got[0] != "start" {
		t.Fatalf("expected a single start action, got %v", got)
	}
}

func TestWingsStartReportsALaunchThatNeverHappens(t *testing.T) {
	// Wings reports asynchronous power failures over its websocket only, so
	// this wait is the sole place a bad image or a full disk becomes an error.
	fake := &fakeWings{state: stateOffline} // never leaves offline
	w := newTestWings(t, fake)

	err := w.Start(context.Background())
	if err == nil {
		t.Fatal("expected an error when the server never leaves offline")
	}
	if !strings.Contains(err.Error(), "still offline") {
		t.Fatalf("the error should say the server never started, got %v", err)
	}
}

func TestWingsStartIsANoOpWhenAlreadyRunning(t *testing.T) {
	fake := &fakeWings{state: stateRunning}
	w := newTestWings(t, fake)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := fake.sent(); len(got) != 0 {
		t.Fatalf("a running server must not be sent a power action, got %v", got)
	}
}

func TestWingsStopIsGracefulWhenTheServerGoes(t *testing.T) {
	fake := &fakeWings{
		state:    stateRunning,
		onAction: func(string) string { return stateOffline },
	}
	w := newTestWings(t, fake)

	if err := w.Stop(context.Background(), time.Minute); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := fake.sent(); len(got) != 1 || got[0] != "stop" {
		t.Fatalf("expected a single graceful stop, got %v", got)
	}
}

func TestWingsStopEscalatesToKill(t *testing.T) {
	// Wings' own stop waits ten minutes and never escalates, so napio has to.
	fake := &fakeWings{state: stateRunning}
	fake.onAction = func(action string) string {
		if action == "kill" {
			return stateOffline
		}
		return stateRunning // ignores /quit
	}
	w := newTestWings(t, fake)

	if err := w.Stop(context.Background(), time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := fake.sent()
	if len(got) != 2 || got[0] != "stop" || got[1] != "kill" {
		t.Fatalf("expected a stop followed by a kill, got %v", got)
	}
}

func TestWingsStopIsANoOpWhenAlreadyOffline(t *testing.T) {
	fake := &fakeWings{state: stateOffline}
	w := newTestWings(t, fake)
	if err := w.Stop(context.Background(), time.Minute); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := fake.sent(); len(got) != 0 {
		t.Fatalf("an offline server must not be sent a power action, got %v", got)
	}
}

func TestWingsStateRetriesATransientFailure(t *testing.T) {
	// reconcile() turns a single IsRunning error into a 30-second backoff
	// during which no player can wake the server. One blip must not do that.
	fake := &fakeWings{state: stateRunning, failStateTimes: 2}
	w := newTestWings(t, fake)

	running, err := w.IsRunning(context.Background())
	if err != nil {
		t.Fatalf("a transient 500 should have been retried, got %v", err)
	}
	if !running {
		t.Fatal("expected the server to be reported as running")
	}
}

func TestWingsMissingServerNamesTheUUIDMistake(t *testing.T) {
	// Pasting the short id from the panel URL is the likeliest first-run error.
	w := newTestWingsWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"The requested resource does not exist on this instance."}`, http.StatusNotFound)
	})
	_, err := w.IsRunning(context.Background())
	if err == nil || !strings.Contains(err.Error(), "full UUID") {
		t.Fatalf("expected the error to point at the full UUID, got %v", err)
	}
}

func TestWingsRejectedTokenIsAClearError(t *testing.T) {
	w := newTestWingsWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"You are not authorized to access this endpoint."}`, http.StatusForbidden)
	})
	_, err := w.IsRunning(context.Background())
	if err == nil || !strings.Contains(err.Error(), "config.yml") {
		t.Fatalf("expected the error to say where the token comes from, got %v", err)
	}
}

func TestWingsAuthFailureIsNotRetried(t *testing.T) {
	var calls int
	w := newTestWingsWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"error":"nope"}`, http.StatusForbidden)
	})
	if _, err := w.IsRunning(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Fatalf("a rejected token is a deployment mistake, not a blip; it was retried %d times", calls)
	}
}

func TestWingsRecentLogsKeepsTheHeadOfTheFile(t *testing.T) {
	// Factorio truncates factorio-current.log at every launch and writes the
	// version banner first, so the head is what matters. The tail is what a
	// naive implementation would keep, and on a played-in server it holds no
	// banner at all.
	var log strings.Builder
	log.WriteString("   0.000 2026-08-30 10:00:00; Factorio 2.0.77 (build 12345, linux64, headless)\n")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&log, "%4d.000 Info ServerMultiplayerManager.cpp:1: chatty line %d\n", i, i)
	}

	fake := &fakeWings{state: stateOffline, logContents: log.String()}
	w := newTestWings(t, fake)

	out, err := w.RecentLogs(context.Background(), 10)
	if err != nil {
		t.Fatalf("RecentLogs: %v", err)
	}
	if !strings.Contains(out, "Factorio 2.0.77 (build 12345, linux64, headless)") {
		t.Fatal("the version banner must survive: it is the only reason this method exists")
	}
	if got := strings.Count(out, "\n"); got > 10 {
		t.Fatalf("asked for 10 lines, got %d", got)
	}
}

func TestWingsMissingLogFileNamesTheConfiguredPath(t *testing.T) {
	w := newTestWingsWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"The requested resources was not found on the system."}`, http.StatusNotFound)
	})
	_, err := w.RecentLogs(context.Background(), 10)
	if err == nil || !strings.Contains(err.Error(), DefaultWingsLogFile) {
		t.Fatalf("the error must name the path that was tried, got %v", err)
	}
	if !strings.Contains(err.Error(), "log_file") {
		t.Fatalf("the error must name the setting that fixes it, got %v", err)
	}
}

func TestWingsCheckConfigurationCatchesAPortMismatch(t *testing.T) {
	// The failure this exists to prevent: napio guards a port nobody connects
	// to, wakes nothing, and reports itself perfectly healthy.
	fake := &fakeWings{state: stateOffline, allocIP: "0.0.0.0", allocPort: 34255}
	w := newTestWings(t, fake)

	warnings := w.CheckConfiguration(context.Background())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "34255") {
		t.Fatalf("expected a warning naming the real allocation, got %v", warnings)
	}
}

func TestWingsCheckConfigurationAcceptsAMatchingAllocation(t *testing.T) {
	fake := &fakeWings{state: stateOffline, allocIP: "0.0.0.0", allocPort: 34197}
	w := newTestWings(t, fake)
	if warnings := w.CheckConfiguration(context.Background()); len(warnings) != 0 {
		t.Fatalf("a matching allocation must not warn, got %v", warnings)
	}
}

func TestWingsCheckConfigurationCatchesAnAddressMismatch(t *testing.T) {
	fake := &fakeWings{state: stateOffline, allocIP: "203.0.113.7", allocPort: 34197}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	w, err := NewWings(srv.URL, testUUID, "node-token", "", false, "10.0.0.5", 34197, testLogger())
	if err != nil {
		t.Fatalf("building wings backend: %v", err)
	}

	warnings := w.CheckConfiguration(context.Background())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "203.0.113.7") {
		t.Fatalf("expected a warning naming the allocation address, got %v", warnings)
	}
}

func TestWingsCheckConfigurationExplainsAWildcardAllocation(t *testing.T) {
	// napio binding one address while the allocation accepts every address is
	// a real gap, but "a player connecting to 0.0.0.0" is not what happens.
	fake := &fakeWings{state: stateOffline, allocIP: "0.0.0.0", allocPort: 34197}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	w, err := NewWings(srv.URL, testUUID, "node-token", "", false, "127.0.0.1", 34197, testLogger())
	if err != nil {
		t.Fatalf("building wings backend: %v", err)
	}

	warnings := w.CheckConfiguration(context.Background())
	if len(warnings) != 1 {
		t.Fatalf("expected one warning, got %v", warnings)
	}
	if !strings.Contains(warnings[0], "every address") {
		t.Fatalf("the warning should explain what a wildcard allocation means, got %q", warnings[0])
	}
	if strings.Contains(warnings[0], "connecting to 0.0.0.0") {
		t.Fatalf("nobody connects to 0.0.0.0; the warning is misleading: %q", warnings[0])
	}
}

func TestWingsCheckConfigurationReportsAServerWithNoAllocation(t *testing.T) {
	fake := &fakeWings{state: stateOffline, noAllocs: true}
	w := newTestWings(t, fake)

	warnings := w.CheckConfiguration(context.Background())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no default allocation") {
		t.Fatalf("expected a warning that the port could not be confirmed, got %v", warnings)
	}
}

func TestWingsCheckConfigurationReportsSuspension(t *testing.T) {
	fake := &fakeWings{state: stateOffline, suspended: true, allocIP: "0.0.0.0", allocPort: 34197}
	w := newTestWings(t, fake)

	warnings := w.CheckConfiguration(context.Background())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "suspended") {
		t.Fatalf("expected a suspension warning, got %v", warnings)
	}
}

func TestWingsOwnsTheQuit(t *testing.T) {
	// Wings restarts anything that exits without its say-so, so napio must not
	// quit Factorio out of band over RCON.
	fake := &fakeWings{state: stateOffline}
	w := newTestWings(t, fake)
	var stopper GracefulStopper = w
	if !stopper.OwnsQuit() {
		t.Fatal("the wings backend must own the quit")
	}
}

func TestWingsDescribeKeepsTheTokenOut(t *testing.T) {
	// Describe lands in the logs and in the HTTP status API.
	fake := &fakeWings{state: stateOffline}
	w := newTestWings(t, fake)
	if strings.Contains(w.Describe(), "node-token") {
		t.Fatalf("Describe leaks the node token: %q", w.Describe())
	}
	if !strings.Contains(w.Describe(), testUUID) {
		t.Fatalf("Describe should identify the server, got %q", w.Describe())
	}
}

func TestNewWingsRejectsAnUnusableURL(t *testing.T) {
	if _, err := NewWings("node.example.com:8080", testUUID, "t", "", false, "0.0.0.0", 1, testLogger()); err == nil {
		t.Fatal("a URL without a scheme should be refused")
	}
	if _, err := NewWings("https://node:8080", "", "t", "", false, "0.0.0.0", 1, testLogger()); err == nil {
		t.Fatal("a missing uuid should be refused")
	}
	if _, err := NewWings("https://node:8080", testUUID, "", "", false, "0.0.0.0", 1, testLogger()); err == nil {
		t.Fatal("a missing token should be refused")
	}
}
