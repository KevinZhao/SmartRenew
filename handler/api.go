package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/KevinZhao/SmartRenew/auth"
	"github.com/KevinZhao/SmartRenew/config"
	"github.com/KevinZhao/SmartRenew/csvutil"
	"github.com/KevinZhao/SmartRenew/model"
)

// ReservationStore is the subset of store operations the handler needs.
type ReservationStore interface {
	List(typeFilter, accountFilter string) ([]model.Reservation, error)
	GetAlerts(maxDays int, remindDays ...int) ([]model.Alert, error)
	Upsert(r model.Reservation) error
	ListGPUCoverage(accountFilter string) ([]model.GPUCoverage, error)
	Ping() error
}

// Syncer triggers a full sync across all configured accounts.
type Syncer interface {
	SyncAll(ctx context.Context) (int, []error)
}

// syncState tracks the progress of the most recent background sync run.
type syncState struct {
	mu         sync.Mutex
	running    bool
	startedAt  time.Time
	finishedAt time.Time
	synced     int
	errors     []string
}

func (s *syncState) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := map[string]any{
		"running": s.running,
		"synced":  s.synced,
		"errors":  append([]string(nil), s.errors...),
	}
	if !s.startedAt.IsZero() {
		resp["started_at"] = s.startedAt.UTC().Format(time.RFC3339)
	}
	if !s.finishedAt.IsZero() {
		resp["finished_at"] = s.finishedAt.UTC().Format(time.RFC3339)
	}
	return resp
}

type Handler struct {
	store     ReservationStore
	syncer    Syncer
	cfg       *config.Config
	frontend  fs.FS
	mux       *http.ServeMux
	syncState syncState

	// Auth is optional: these fields are nil when auth.enabled is false.
	authenticator *auth.Authenticator
	sessions      *auth.SessionStore
	// loginLimiter throttles by client IP, userLimiter by username. Both are
	// needed: the client IP comes from a spoofable header behind a proxy.
	loginLimiter      *auth.LoginLimiter
	userLimiter       *auth.LoginLimiter
	cookieSecure      bool
	trustProxyHeaders bool
}

// Option customises the Handler at construction time.
type Option func(*Handler)

// WithAuth enables session-based login enforcement on every route except
// /api/health and the login page assets.
func WithAuth(a *auth.Authenticator, sessions *auth.SessionStore, limiter, userLimiter *auth.LoginLimiter, cookieSecure bool) Option {
	return func(h *Handler) {
		h.authenticator = a
		h.sessions = sessions
		h.loginLimiter = limiter
		h.userLimiter = userLimiter
		h.cookieSecure = cookieSecure
		// Behind CloudFront/ALB the socket peer is the proxy, so per-IP login
		// throttling needs the forwarded client address to be meaningful.
		h.trustProxyHeaders = true
	}
}

func New(s ReservationStore, sc Syncer, cfg *config.Config, frontendFS fs.FS, opts ...Option) *Handler {
	h := &Handler{store: s, syncer: sc, cfg: cfg, frontend: frontendFS, mux: http.NewServeMux()}
	for _, opt := range opts {
		opt(h)
	}
	h.registerRoutes()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) registerRoutes() {
	// Protected data endpoints.
	h.mux.HandleFunc("GET /api/reservations", h.requireAuth(h.listReservations))
	h.mux.HandleFunc("GET /api/alerts", h.requireAuth(h.getAlerts))
	h.mux.HandleFunc("POST /api/sync", h.requireAuth(h.syncAll))
	h.mux.HandleFunc("GET /api/sync/status", h.requireAuth(h.syncStatus))
	h.mux.HandleFunc("GET /api/export", h.requireAuth(h.exportCSV))
	h.mux.HandleFunc("POST /api/import", h.requireAuth(h.importCSV))
	h.mux.HandleFunc("GET /api/gpu-coverage", h.requireAuth(h.listGPUCoverage))

	// Public: k8s liveness/readiness probes must not need credentials.
	h.mux.HandleFunc("GET /api/health", h.healthCheck)

	// Auth endpoints.
	h.mux.HandleFunc("POST /api/login", h.login)
	h.mux.HandleFunc("POST /api/logout", h.logout)
	h.mux.HandleFunc("GET /api/me", h.me)

	fileServer := http.FileServer(http.FS(h.frontend))
	h.mux.Handle("/", h.requireAuthPage(fileServer.ServeHTTP))
}

func (h *Handler) listReservations(w http.ResponseWriter, r *http.Request) {
	typeFilter := r.URL.Query().Get("type")
	accountFilter := r.URL.Query().Get("account")

	rows, err := h.store.List(typeFilter, accountFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, rows)
}

func (h *Handler) getAlerts(w http.ResponseWriter, r *http.Request) {
	alerts, err := h.store.GetAlerts(h.cfg.MaxRemindDays(), h.cfg.RemindDays...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, alerts)
}

// syncAll triggers a full account sync in the background and returns
// immediately with 202 Accepted so that upstream proxies (CloudFront, ALB)
// do not time out waiting for the long-running operation. Progress and
// results can be polled via GET /api/sync/status.
func (h *Handler) syncAll(w http.ResponseWriter, r *http.Request) {
	h.syncState.mu.Lock()
	if h.syncState.running {
		startedAt := h.syncState.startedAt
		synced := h.syncState.synced
		h.syncState.mu.Unlock()
		snap := map[string]any{
			"running": true,
			"synced":  synced,
		}
		if !startedAt.IsZero() {
			snap["started_at"] = startedAt.UTC().Format(time.RFC3339)
		}
		w.Header().Set("Content-Type", "application/json")
		// 409 Conflict signals "already running" distinctly from "just started".
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(snap)
		return
	}
	h.syncState.running = true
	h.syncState.startedAt = time.Now()
	h.syncState.finishedAt = time.Time{}
	h.syncState.synced = 0
	h.syncState.errors = nil
	startedAt := h.syncState.startedAt
	h.syncState.mu.Unlock()

	go func() {
		// Use a fresh context fully decoupled from the HTTP request so that
		// upstream proxy disconnects do not cancel the sync mid-flight.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		total, errs := h.syncer.SyncAll(ctx)
		errStrs := make([]string, len(errs))
		for i, e := range errs {
			errStrs[i] = e.Error()
		}

		h.syncState.mu.Lock()
		h.syncState.running = false
		h.syncState.finishedAt = time.Now()
		h.syncState.synced = total
		h.syncState.errors = errStrs
		h.syncState.mu.Unlock()

		slog.Info("async sync done", "items", total, "errors", len(errs))
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{
		"running":    true,
		"started_at": startedAt.UTC().Format(time.RFC3339),
	})
}

// syncStatus returns the current state of the background sync job.
func (h *Handler) syncStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.syncState.snapshot())
}

func (h *Handler) exportCSV(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.List("", "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	filename := fmt.Sprintf("smartrenew_%s.csv", time.Now().Format("20060102"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	// Write BOM for Excel compatibility
	if _, err := w.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		slog.Error("write BOM failed", "err", err)
		return
	}
	if err := csvutil.Export(w, rows); err != nil {
		slog.Error("csv export failed", "err", err)
	}
}

// maxImportBytes caps the whole upload. ParseMultipartForm's argument is only
// the in-memory buffer — anything above it spills to a temp file, so without
// this the request body is effectively unbounded and a large upload could fill
// the container's /tmp volume.
const maxImportBytes = 32 << 20 // 32 MiB

func (h *Handler) importCSV(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)
	if err := r.ParseMultipartForm(maxImportBytes); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("parse form: %v", err))
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "no file uploaded")
		return
	}
	defer file.Close()

	items, err := csvutil.Import(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	count := 0
	failed := 0
	for _, item := range items {
		if err := h.store.Upsert(item); err != nil {
			slog.Error("import upsert failed", "err", err)
			failed++
			continue
		}
		count++
	}

	writeJSON(w, map[string]any{"imported": count, "failed": failed, "total": len(items)})
}

func (h *Handler) healthCheck(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Ping(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handler) listGPUCoverage(w http.ResponseWriter, r *http.Request) {
	accountFilter := r.URL.Query().Get("account")
	rows, err := h.store.ListGPUCoverage(accountFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, rows)
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("json encode failed", "err", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
