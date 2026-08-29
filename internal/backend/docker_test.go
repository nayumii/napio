package backend

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestDocker(t *testing.T, handler http.HandlerFunc) *Docker {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	d, err := NewDocker("factorio", "tcp://"+strings.TrimPrefix(srv.URL, "http://"), testLogger())
	if err != nil {
		t.Fatalf("building docker backend: %v", err)
	}
	return d
}

func TestDockerIsRunning(t *testing.T) {
	d := newTestDocker(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/containers/factorio/json") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, `{"State":{"Running":true,"Status":"running"}}`)
	})
	running, err := d.IsRunning(context.Background())
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if !running {
		t.Fatal("expected the container to be reported as running")
	}
}

func TestDockerMissingContainerIsAClearError(t *testing.T) {
	d := newTestDocker(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such container: factorio"}`)
	})
	_, err := d.IsRunning(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected a clear missing-container error, got %v", err)
	}
}

func TestDockerStartHandlesAlreadyRunning(t *testing.T) {
	d := newTestDocker(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("starting an already-running container must not be an error: %v", err)
	}
}

func TestDockerStartReportsPortConflicts(t *testing.T) {
	// This is the failure napio must never cause: it means the handover went
	// wrong and napio still holds the published port.
	d := newTestDocker(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"message":"driver failed programming external connectivity"}`)
	})
	err := d.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "external connectivity") {
		t.Fatalf("expected the daemon's message to be surfaced, got %v", err)
	}
}

func TestDockerStopPassesATimeout(t *testing.T) {
	var got string
	d := newTestDocker(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("t")
		w.WriteHeader(http.StatusNoContent)
	})
	if err := d.Stop(context.Background(), 45*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got != "45" {
		t.Fatalf("expected the stop timeout to be passed to docker, got %q", got)
	}
}

func TestDockerWarnsAboutFightingRestartPolicies(t *testing.T) {
	d := newTestDocker(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"State":{"Running":false},"HostConfig":{"RestartPolicy":{"Name":"always"}}}`)
	})
	warnings := d.CheckConfiguration(context.Background())
	if len(warnings) == 0 || !strings.Contains(warnings[0], "restart policy") {
		t.Fatalf("expected a warning about the restart policy, got %v", warnings)
	}
}

func TestUnsupportedDockerHostIsRejected(t *testing.T) {
	if _, err := NewDocker("factorio", "ssh://somewhere", testLogger()); err == nil {
		t.Fatal("expected an error for an unsupported docker host")
	}
}

func TestDockerRequiresAContainerName(t *testing.T) {
	if _, err := NewDocker("", "", testLogger()); err == nil {
		t.Fatal("expected an error when no container is configured")
	}
}
