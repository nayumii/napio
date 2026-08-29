// Package api exposes napio's state over HTTP.
//
// It is intentionally small: a read-only status surface plus optional manual
// start/stop. That is everything a future web UI needs, and nothing it does
// not — the UI can be a static page served elsewhere that talks to this.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"git.naknu.li/emily/napio/internal/config"
	"git.naknu.li/emily/napio/internal/supervisor"
)

// Controller is the subset of a supervisor the API is allowed to use.
type Controller interface {
	Name() string
	Status() supervisor.Status
	RequestStart(ctx context.Context, reason string) error
	RequestStop(ctx context.Context, reason string) error
}

// Server is the napio HTTP API.
type Server struct {
	cfg     config.API
	version string
	servers map[string]Controller
	order   []string
	log     *slog.Logger
}

// New builds the API over the given controllers.
func New(cfg config.API, version string, controllers []Controller, log *slog.Logger) *Server {
	s := &Server{
		cfg:     cfg,
		version: version,
		servers: make(map[string]Controller, len(controllers)),
		log:     log,
	}
	for _, c := range controllers {
		s.servers[c.Name()] = c
		s.order = append(s.order, c.Name())
	}
	return s
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/servers", s.authed(s.handleList))
	mux.HandleFunc("GET /api/v1/servers/{name}", s.authed(s.handleGet))
	mux.HandleFunc("POST /api/v1/servers/{name}/start", s.authed(s.control("start")))
	mux.HandleFunc("POST /api/v1/servers/{name}/stop", s.authed(s.control("stop")))
	return mux
}

// ListenAndServe runs the API until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	s.log.Info("api listening", "address", s.cfg.Listen, "control", s.cfg.Control,
		"authenticated", s.cfg.Token != "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	out := make([]supervisor.Status, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, s.servers[name].Status())
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	c, ok := s.servers[r.PathValue("name")]
	if !ok {
		writeError(w, http.StatusNotFound, "no such server")
		return
	}
	writeJSON(w, http.StatusOK, c.Status())
}

func (s *Server) control(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Control {
			writeError(w, http.StatusForbidden, "control endpoints are disabled (set api.control: true)")
			return
		}
		c, ok := s.servers[r.PathValue("name")]
		if !ok {
			writeError(w, http.StatusNotFound, "no such server")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		var err error
		switch action {
		case "start":
			err = c.RequestStart(ctx, "api request")
		case "stop":
			err = c.RequestStop(ctx, "api request")
		}
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, c.Status())
	}
}

// authed enforces the bearer token, when one is configured.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token != "" {
			const prefix = "Bearer "
			header := r.Header.Get("Authorization")
			if len(header) <= len(prefix) ||
				subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(s.cfg.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
				return
			}
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
