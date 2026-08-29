package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.naknu.li/emily/napio/internal/config"
	"git.naknu.li/emily/napio/internal/supervisor"
)

type fakeController struct {
	name     string
	status   supervisor.Status
	starts   int
	stops    int
	startErr error
}

func (f *fakeController) Name() string              { return f.name }
func (f *fakeController) Status() supervisor.Status { return f.status }
func (f *fakeController) RequestStart(context.Context, string) error {
	f.starts++
	return f.startErr
}
func (f *fakeController) RequestStop(context.Context, string) error {
	f.stops++
	return nil
}

func newTestAPI(t *testing.T, cfg config.API, controllers ...Controller) http.Handler {
	t.Helper()
	return New(cfg, "test", controllers, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
}

func do(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestListAndGetServers(t *testing.T) {
	c := &fakeController{name: "one", status: supervisor.Status{Name: "one", State: supervisor.StateOffline}}
	h := newTestAPI(t, config.API{Enabled: true}, c)

	rec := do(t, h, http.MethodGet, "/api/v1/servers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d", rec.Code)
	}
	var list struct {
		Servers []supervisor.Status `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding list: %v", err)
	}
	if len(list.Servers) != 1 || list.Servers[0].Name != "one" {
		t.Fatalf("unexpected list %+v", list.Servers)
	}

	rec = do(t, h, http.MethodGet, "/api/v1/servers/one", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/api/v1/servers/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown server: got %d, want 404", rec.Code)
	}
}

func TestControlIsDisabledByDefault(t *testing.T) {
	c := &fakeController{name: "one"}
	h := newTestAPI(t, config.API{Enabled: true}, c)

	rec := do(t, h, http.MethodPost, "/api/v1/servers/one/start", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 when control is off", rec.Code)
	}
	if c.starts != 0 {
		t.Fatal("no start should have been requested")
	}
}

func TestStartAndStopWhenControlIsEnabled(t *testing.T) {
	c := &fakeController{name: "one"}
	h := newTestAPI(t, config.API{Enabled: true, Control: true}, c)

	if rec := do(t, h, http.MethodPost, "/api/v1/servers/one/start", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("start: got %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/servers/one/stop", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("stop: got %d", rec.Code)
	}
	if c.starts != 1 || c.stops != 1 {
		t.Fatalf("expected one start and one stop, got %d/%d", c.starts, c.stops)
	}
}

func TestTokenIsEnforced(t *testing.T) {
	c := &fakeController{name: "one"}
	h := newTestAPI(t, config.API{Enabled: true, Control: true, Token: "s3cret"}, c)

	if rec := do(t, h, http.MethodGet, "/api/v1/servers", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d, want 401", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/servers", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/servers", "s3cret"); rec.Code != http.StatusOK {
		t.Fatalf("correct token: got %d", rec.Code)
	}
	// The health endpoint stays open so container health checks work.
	if rec := do(t, h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz: got %d", rec.Code)
	}
}

func TestStatusNeverLeaksSecrets(t *testing.T) {
	c := &fakeController{name: "one", status: supervisor.Status{Name: "one"}}
	h := newTestAPI(t, config.API{Enabled: true, Token: "s3cret"}, c)

	rec := do(t, h, http.MethodGet, "/api/v1/servers/one", "s3cret")
	if body := rec.Body.String(); contains(body, "s3cret") {
		t.Fatalf("the API response leaked a secret: %s", body)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
