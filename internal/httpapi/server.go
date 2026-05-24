package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

const apiKey = "dev-api-key"

type ErrorResponse struct {
	Error     string            `json:"error"`
	Message   string            `json:"message"`
	RequestID string            `json:"request_id"`
	Details   map[string]string `json:"details,omitempty"`
}

type Server struct {
	logger       *slog.Logger
	roomCount    int
	bookingCount int
}

func NewServer(logger *slog.Logger) http.Handler {
	s := &Server{logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /ready", s.ready)
	mux.HandleFunc("GET /api/v1/rooms", s.notImplemented("list rooms"))
	mux.HandleFunc("POST /api/v1/rooms", s.notImplemented("create room"))
	mux.HandleFunc("GET /api/v1/rooms/{id}", s.notImplemented("get room"))
	mux.HandleFunc("GET /api/v1/bookings", s.notImplemented("list bookings"))
	mux.HandleFunc("POST /api/v1/bookings", s.notImplemented("create booking"))
	mux.HandleFunc("GET /api/v1/bookings/{id}", s.notImplemented("get booking"))
	mux.HandleFunc("PATCH /api/v1/bookings/{id}", s.notImplemented("patch booking"))
	mux.HandleFunc("POST /api/v1/bookings/{id}/cancel", s.notImplemented("cancel booking"))
	mux.HandleFunc("POST /api/v1/bookings/{id}/approve", s.notImplemented("approve booking"))
	mux.HandleFunc("POST /api/v1/bookings/{id}/reject", s.notImplemented("reject booking"))

	return recoverer(requestID(requestLogger(logger)(timeout(5 * time.Second)(limitBody(1 << 20)(apiKeyAuth(mux))))))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "booking-api"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ready",
		"rooms":    s.roomCount,
		"bookings": s.bookingCount,
	})
}

func (s *Server) notImplemented(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotImplemented, "not_implemented", action+" is not implemented yet", nil)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]string) {
	writeJSON(w, status, ErrorResponse{Error: code, Message: message, RequestID: r.Header.Get("X-Request-ID"), Details: details})
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = randomID()
		}
		r.Header.Set("X-Request-ID", id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			logger.Info("request", "request_id", r.Header.Get("X-Request-ID"), "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
		})
	}
}

func timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, `{"error":"timeout","message":"request timed out"}`)
	}
}

func limitBody(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}

func apiKeyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != apiKey {
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "invalid api key", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				writeError(w, r, http.StatusInternalServerError, "internal_error", "internal server error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
