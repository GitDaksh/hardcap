// Package admin is Hardcap's management API and the dashboard that uses it:
// create projects and keys, set caps, watch spending live, and (in demo
// mode) launch a stampede of requests at a project to watch the cap hold.
//
// Every /admin/api route needs the admin token: `Authorization: Bearer <token>`.
package admin

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/GitDaksh/hardcap/internal/ledger"
	"github.com/GitDaksh/hardcap/internal/pricing"
)

//go:embed dashboard
var dashboardFiles embed.FS

// Config is everything needed to build the admin server.
type Config struct {
	Ledger *ledger.Ledger
	Token  string   // the admin token; required
	Models []string // model names, for the dashboard's model picker
	Logger *slog.Logger
	// Demo enables the Stampede button. nil leaves it off.
	Demo *Demo
}

// Server serves the admin API and the dashboard.
type Server struct {
	ledger *ledger.Ledger
	token  []byte
	models []string
	logger *slog.Logger
	demo   *Demo
}

// New returns an admin server. It refuses an empty token: an admin API
// without a password is a hole.
func New(cfg Config) (*Server, error) {
	if len(cfg.Token) < 8 {
		return nil, errors.New("admin: the admin token must be at least 8 characters")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{ledger: cfg.Ledger, token: []byte(cfg.Token), models: cfg.Models, logger: logger, demo: cfg.Demo}, nil
}

// Handler returns the dashboard (at /dashboard/) and the API (at /admin/api/).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(dashboardFiles, "dashboard")
	mux.Handle("GET /dashboard/", http.StripPrefix("/dashboard/", http.FileServerFS(static)))
	mux.Handle("GET /{$}", http.RedirectHandler("/dashboard/", http.StatusFound))

	api := func(pattern string, h func(http.ResponseWriter, *http.Request) error) {
		mux.HandleFunc(pattern, s.authorize(h))
	}
	api("GET /admin/api/overview", s.overview)
	api("GET /admin/api/projects", s.listProjects)
	api("POST /admin/api/projects", s.createProject)
	api("GET /admin/api/projects/{id}", s.getProject)
	api("PATCH /admin/api/projects/{id}", s.updateProject)
	api("POST /admin/api/projects/{id}/reset", s.resetProject)
	api("GET /admin/api/projects/{id}/keys", s.listKeys)
	api("POST /admin/api/projects/{id}/keys", s.createKey)
	api("POST /admin/api/keys/{id}/revoke", s.revokeKey)
	api("GET /admin/api/projects/{id}/usage", s.usage)
	api("GET /admin/api/projects/{id}/audit", s.audit)
	api("POST /admin/api/projects/{id}/stampede", s.startStampede)
	return mux
}

// httpError is an error with an HTTP status.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func fail(status int, msg string) error { return &httpError{status, msg} }

// authorize checks the admin token (in constant time, so response timing
// can't leak it) and turns handler errors into JSON replies.
func (s *Server) authorize(h func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), s.token) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or wrong admin token"})
			return
		}
		err := h(w, r)
		if err == nil {
			return
		}
		var he *httpError
		switch {
		case errors.As(err, &he):
			writeJSON(w, he.status, map[string]string{"error": he.msg})
		case errors.Is(err, ledger.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		case errors.Is(err, ledger.ErrNameTaken):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			s.logger.Error("admin request failed", "path", r.URL.Path, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
	}
}

// projectView is a project plus its live stampede, if one is running.
type projectView struct {
	ledger.Project
	Stampede any `json:"stampede"`
}

func (s *Server) view(p ledger.Project) projectView {
	v := projectView{Project: p}
	if s.demo != nil {
		if snap, ok := s.demo.Status(p.ID); ok {
			v.Stampede = snap
		}
	}
	return v
}

// overview is everything the dashboard polls for, in one call.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) error {
	projects, err := s.ledger.Projects(r.Context())
	if err != nil {
		return err
	}
	views := make([]projectView, 0, len(projects))
	for _, p := range projects {
		views = append(views, s.view(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": views, "models": s.models, "demo": s.demo != nil})
	return nil
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) error {
	projects, err := s.ledger.Projects(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, projects)
	return nil
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name   string      `json:"name"`
		CapUSD json.Number `json:"cap_usd"`
		Mode   ledger.Mode `json:"mode"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		return fail(http.StatusBadRequest, "name must be 1 to 100 characters")
	}
	cap, err := pricing.ParseUSD(in.CapUSD.String())
	if err != nil {
		return fail(http.StatusBadRequest, "cap_usd: "+err.Error())
	}
	if in.Mode == "" {
		in.Mode = ledger.ModeHardcap
	}
	if !in.Mode.Valid() {
		return fail(http.StatusBadRequest, `mode must be "hardcap" or "naive"`)
	}
	p, err := s.ledger.CreateProject(r.Context(), in.Name, cap, in.Mode)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, p)
	return nil
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	p, err := s.ledger.Project(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.view(p))
	return nil
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		CapUSD *json.Number `json:"cap_usd"`
		Mode   *ledger.Mode `json:"mode"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	var cap *pricing.Micros
	if in.CapUSD != nil {
		c, err := pricing.ParseUSD(in.CapUSD.String())
		if err != nil {
			return fail(http.StatusBadRequest, "cap_usd: "+err.Error())
		}
		cap = &c
	}
	if in.Mode != nil && !in.Mode.Valid() {
		return fail(http.StatusBadRequest, `mode must be "hardcap" or "naive"`)
	}
	p, err := s.ledger.UpdateProject(r.Context(), id, cap, in.Mode)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, p)
	return nil
}

func (s *Server) resetProject(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if s.demo != nil && s.demo.Running(id) {
		return fail(http.StatusConflict, "a stampede is running on this project")
	}
	if err := s.ledger.ResetSpend(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	keys, err := s.ledger.Keys(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, keys)
	return nil
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		Name   string       `json:"name"`
		CapUSD *json.Number `json:"cap_usd"`
		RPM    *int         `json:"rpm_limit"`
		TPM    *int         `json:"tpm_limit"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	opts := ledger.KeyOptions{Name: strings.TrimSpace(in.Name), RPM: in.RPM, TPM: in.TPM}
	if opts.Name == "" {
		opts.Name = "key"
	}
	if len(opts.Name) > 100 {
		return fail(http.StatusBadRequest, "name must be at most 100 characters")
	}
	if (in.RPM != nil && *in.RPM < 0) || (in.TPM != nil && *in.TPM < 0) {
		return fail(http.StatusBadRequest, "limits can't be negative (0 means no limit)")
	}
	if in.CapUSD != nil {
		c, err := pricing.ParseUSD(in.CapUSD.String())
		if err != nil {
			return fail(http.StatusBadRequest, "cap_usd: "+err.Error())
		}
		opts.Cap = &c
	}
	key, secret, err := s.ledger.CreateKey(r.Context(), id, opts)
	if err != nil {
		return err
	}
	// The secret is shown this once. Only its hash is stored.
	writeJSON(w, http.StatusCreated, map[string]any{"key": key, "secret": secret})
	return nil
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if err := s.ledger.RevokeKey(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	events, err := s.ledger.RecentUsage(r.Context(), id, limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, events)
	return nil
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	a, err := s.ledger.Audit(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"audit":        a,
		"balanced":     a.Balanced(),
		"within_cap":   a.Project.Spent+a.Project.Reserved <= a.Project.Cap,
		"spent_usd":    a.Project.Spent.USD(),
		"cap_usd":      a.Project.Cap.USD(),
		"reserved_usd": a.Project.Reserved.USD(),
	})
	return nil
}

func (s *Server) startStampede(w http.ResponseWriter, r *http.Request) error {
	if s.demo == nil {
		return fail(http.StatusNotFound, "demo mode is off: start Hardcap with HARDCAP_DEMO=1 to use the stampede")
	}
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		Workers int    `json:"workers"`
		Seconds int    `json:"seconds"`
		Model   string `json:"model"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Workers < 1 || in.Workers > 1000 || in.Seconds < 1 || in.Seconds > 120 {
		return fail(http.StatusBadRequest, "workers must be 1 to 1000 and seconds 1 to 120")
	}
	if in.Model == "" {
		in.Model = "mock-1"
	}
	if err := s.demo.Start(r.Context(), id, in.Workers, in.Seconds, in.Model); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, fail(http.StatusBadRequest, "bad id")
	}
	return id, nil
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fail(http.StatusBadRequest, "invalid JSON body: "+err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
