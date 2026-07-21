package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"tradeguardian/internal/domain"
	"tradeguardian/internal/service"
)

//go:embed web/*
var webFiles embed.FS

type Server struct {
	service      *service.Service
	template     *template.Template
	mux          *http.ServeMux
	broadcast    *broadcaster
	publicOrigin string
	allowedHost  string
	secureCookie bool
}

type Config struct {
	PublicOrigin string
}

type pageData struct {
	CSRFToken string
}

func New(app *service.Service, configs ...Config) (*Server, error) {
	if app == nil {
		return nil, fmt.Errorf("service is required")
	}
	if len(configs) > 1 {
		return nil, fmt.Errorf("only one HTTP configuration is supported")
	}
	config := Config{PublicOrigin: "http://127.0.0.1:8080"}
	if len(configs) == 1 {
		config = configs[0]
	}
	origin, err := validatePublicOrigin(config.PublicOrigin)
	if err != nil {
		return nil, err
	}
	page, err := template.ParseFS(webFiles, "web/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse dashboard template: %w", err)
	}
	server := &Server{
		service: app, template: page, mux: http.NewServeMux(), broadcast: newBroadcaster(),
		publicOrigin: origin.Scheme + "://" + origin.Host,
		allowedHost:  origin.Host, secureCookie: origin.Scheme == "https",
	}
	server.routes()
	app.SetNotifier(server.broadcast.publish)
	return server, nil
}

func (s *Server) Handler() http.Handler {
	staticFS, _ := fs.Sub(webFiles, "web")
	static := http.FileServer(http.FS(staticFS))
	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app.js" || r.URL.Path == "/style.css" {
			static.ServeHTTP(w, r)
			return
		}
		s.mux.ServeHTTP(w, r)
	}))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.dashboard)
	s.mux.HandleFunc("GET /auth/login", s.login)
	s.mux.HandleFunc("GET /auth/callback", s.callback)
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /api/status", s.status)
	s.mux.HandleFunc("GET /api/positions", s.positions)
	s.mux.HandleFunc("GET /api/orders", s.orders)
	s.mux.HandleFunc("GET /api/instruments", s.instruments)
	s.mux.HandleFunc("GET /api/audit", s.audit)
	s.mux.HandleFunc("GET /api/events", s.events)
	s.mux.Handle("POST /api/orders", s.mutation(http.HandlerFunc(s.place)))
	s.mux.Handle("POST /api/baskets", s.mutation(http.HandlerFunc(s.placeBasket)))
	s.mux.Handle("POST /api/orders/{orderID}/modify", s.mutation(http.HandlerFunc(s.modify)))
	s.mux.Handle("POST /api/orders/{orderID}/cancel", s.mutation(http.HandlerFunc(s.cancel)))
	s.mux.Handle("POST /api/positions/{token}/exit", s.mutation(http.HandlerFunc(s.exitPosition)))
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	csrf, err := s.ensureCSRFCookie(w, r)
	if err != nil {
		http.Error(w, "Unable to initialize secure session.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.template.Execute(w, pageData{CSRFToken: csrf}); err != nil {
		http.Error(w, "Unable to render dashboard.", http.StatusInternalServerError)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	state, err := service.RandomID()
	if err != nil {
		http.Error(w, "Unable to start Kite login.", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "tg_auth_state", Value: state, Path: "/auth/callback", HttpOnly: true, Secure: s.secureCookie, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, s.service.LoginURL(state), http.StatusFound)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie("tg_auth_state")
	if err != nil || stateCookie.Value == "" || r.URL.Query().Get("state") != stateCookie.Value {
		http.Error(w, "Kite login state validation failed. Start login again.", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "tg_auth_state", Value: "", Path: "/auth/callback", HttpOnly: true, Secure: s.secureCookie, MaxAge: -1})
	if r.URL.Query().Get("status") != "success" {
		http.Error(w, "Kite login was not successful.", http.StatusBadRequest)
		return
	}
	requestToken := r.URL.Query().Get("request_token")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.service.Authenticate(ctx, requestToken); err != nil {
		http.Error(w, "Kite connected, but TradeGuardian could not initialize risk monitoring. Review the local logs and retry.", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	snapshot := s.service.Snapshot()
	code := http.StatusOK
	if snapshot.RuntimeStatus == domain.RuntimeDegraded || snapshot.RuntimeStatus == domain.RuntimeLiquidating {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"ok": code == http.StatusOK, "runtime_status": snapshot.RuntimeStatus, "trading_status": snapshot.TradingStatus})
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.service.Snapshot())
}

func (s *Server) positions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"positions": s.service.Positions()})
}

func (s *Server) orders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"orders": s.service.Orders()})
}

func (s *Server) instruments(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"instruments": s.service.SearchInstruments(r.URL.Query().Get("q"), r.URL.Query().Get("exchange"), r.URL.Query().Get("kind"), 30)})
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	events, err := s.service.Audit(r.Context(), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "AUDIT_UNAVAILABLE", "Audit events could not be read.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) place(w http.ResponseWriter, r *http.Request) {
	var request domain.OrderRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	decision, orderID, err := s.service.Place(ctx, request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"decision": decision, "order_id": orderID})
		return
	}
	status := http.StatusOK
	if !decision.Allowed {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]any{"decision": decision, "order_id": orderID})
}

func (s *Server) modify(w http.ResponseWriter, r *http.Request) {
	var request domain.ModifyRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	decision, orderID, err := s.service.Modify(ctx, r.PathValue("orderID"), request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"decision": decision, "order_id": orderID})
		return
	}
	status := http.StatusOK
	if !decision.Allowed {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]any{"decision": decision, "order_id": orderID})
}

func (s *Server) placeBasket(w http.ResponseWriter, r *http.Request) {
	var request domain.BasketRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := s.service.PlaceBasket(ctx, request)
	if err != nil {
		status := http.StatusUnprocessableEntity
		message := err.Error()
		if result.BasketID != "" {
			status = http.StatusBadGateway
			message = "Basket deployment was not completed. Review its result and reconcile the broker order book."
		}
		writeJSON(w, status, map[string]any{"result": result, "error": map[string]string{"code": "BASKET_REJECTED", "message": message}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.service.Cancel(ctx, r.PathValue("orderID")); err != nil {
		writeError(w, http.StatusBadGateway, "CANCEL_FAILED", "Order cancellation was not accepted.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) exitPosition(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Product string `json:"product"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	token64, err := strconv.ParseUint(r.PathValue("token"), 10, 32)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TOKEN", "Invalid instrument token.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	orderID, err := s.service.ExitPosition(ctx, uint32(token64), request.Product)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"order_id": orderID,
			"error":    map[string]string{"code": "EXIT_FAILED", "message": "Position exit could not be fully confirmed. Reconcile the broker order book."},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "order_id": orderID})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported.", http.StatusInternalServerError)
		return
	}
	// The server-level write timeout is appropriate for ordinary responses but
	// would terminate a healthy long-lived SSE stream. Each stream remains bound
	// to its request context and heartbeat loop.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	updates, unsubscribe := s.broadcast.subscribe()
	defer unsubscribe()
	_, _ = io.WriteString(w, "event: state\ndata: refresh\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-updates:
			_, _ = io.WriteString(w, "event: state\ndata: refresh\n\n")
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) mutation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Host, s.allowedHost) {
			writeError(w, http.StatusForbidden, "INVALID_HOST", "Request host was rejected.")
			return
		}
		origin := r.Header.Get("Origin")
		if origin != s.publicOrigin {
			writeError(w, http.StatusForbidden, "INVALID_ORIGIN", "Request origin was rejected.")
			return
		}
		cookie, err := r.Cookie("tg_csrf")
		if err != nil || cookie.Value == "" || r.Header.Get("X-CSRF-Token") != cookie.Value {
			writeError(w, http.StatusForbidden, "CSRF_REJECTED", "CSRF validation failed.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("request body must contain one JSON object")
	}
	return nil
}

func (s *Server) ensureCSRFCookie(w http.ResponseWriter, r *http.Request) (string, error) {
	if cookie, err := r.Cookie("tg_csrf"); err == nil && cookie.Value != "" {
		return cookie.Value, nil
	}
	value, err := service.RandomID()
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{Name: "tg_csrf", Value: value, Path: "/", HttpOnly: true, Secure: s.secureCookie, SameSite: http.SameSiteStrictMode})
	return value, nil
}

func validatePublicOrigin(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "/"))
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("TRADEGUARDIAN_PUBLIC_ORIGIN must be an http or https origin")
	}
	if parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("TRADEGUARDIAN_PUBLIC_ORIGIN must not contain credentials, a path, query, or fragment")
	}
	return parsed, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

type broadcaster struct {
	mu      sync.Mutex
	nextID  int
	clients map[int]chan struct{}
}

func newBroadcaster() *broadcaster { return &broadcaster{clients: make(map[int]chan struct{})} }

func (b *broadcaster) subscribe() (<-chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	channel := make(chan struct{}, 1)
	b.clients[id] = channel
	return channel, func() {
		b.mu.Lock()
		delete(b.clients, id)
		b.mu.Unlock()
	}
}

func (b *broadcaster) publish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, channel := range b.clients {
		select {
		case channel <- struct{}{}:
		default:
		}
	}
}
