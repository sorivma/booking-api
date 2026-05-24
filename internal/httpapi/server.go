package httpapi

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiKey = "dev-api-key"

type ErrorResponse struct {
	Error     string            `json:"error"`
	Message   string            `json:"message"`
	RequestID string            `json:"request_id"`
	Details   map[string]string `json:"details,omitempty"`
}

type Room struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Booking struct {
	ID        string    `json:"id"`
	RoomID    string    `json:"room_id"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Status    string    `json:"status"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateRoomRequest struct {
	Name string `json:"name"`
}

type CreateBookingRequest struct {
	RoomID string `json:"room_id"`
	From   string `json:"from"`
	To     string `json:"to"`
}

type UpdateBookingRequest struct {
	From    *string `json:"from"`
	To      *string `json:"to"`
	Version *int    `json:"version"`
}

type BookingQuery struct {
	RoomID string
	From   *time.Time
	To     *time.Time
	Status string
}

type Readiness struct {
	Status   string `json:"status"`
	Rooms    int    `json:"rooms"`
	Bookings int    `json:"bookings"`
}

type ListResponse[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
}

type Store interface {
	ListRooms() []Room
	CreateRoom(CreateRoomRequest) (Room, error)
	GetRoom(string) (Room, error)
	ListBookings(BookingQuery) []Booking
	CreateBooking(CreateBookingRequest) (Booking, error)
	GetBooking(string) (Booking, error)
	UpdateBooking(string, UpdateBookingRequest, *int) (Booking, error)
	CancelBooking(string, *int) (Booking, error)
	ApproveBooking(string, *int) (Booking, error)
	RejectBooking(string, *int) (Booking, error)
	Ready() Readiness
}

type MemoryStore struct {
	mu       sync.RWMutex
	nextID   int64
	rooms    map[string]Room
	bookings map[string]Booking
	now      func() time.Time
}

type appError struct {
	status  int
	code    string
	message string
	details map[string]string
}

func (e appError) Error() string { return e.message }

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		rooms:    map[string]Room{},
		bookings: map[string]Booking{},
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (s *MemoryStore) ListRooms() []Room {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]Room, 0, len(s.rooms))
	for _, room := range s.rooms {
		items = append(items, room)
	}
	slices.SortFunc(items, func(a, b Room) int { return cmp.Compare(a.Name, b.Name) })
	return items
}

func (s *MemoryStore) CreateRoom(req CreateRoomRequest) (Room, error) {
	if strings.TrimSpace(req.Name) == "" {
		return Room{}, badRequest("invalid_room", "name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room := Room{ID: s.newIDLocked("room"), Name: req.Name, CreatedAt: s.now()}
	s.rooms[room.ID] = room
	return room, nil
}

func (s *MemoryStore) GetRoom(id string) (Room, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	room, ok := s.rooms[id]
	if !ok {
		return Room{}, notFound("room_not_found", "room not found")
	}
	return room, nil
}

func (s *MemoryStore) ListBookings(q BookingQuery) []Booking {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := []Booking{}
	for _, booking := range s.bookings {
		if q.RoomID != "" && booking.RoomID != q.RoomID {
			continue
		}
		if q.Status != "" && booking.Status != q.Status {
			continue
		}
		if q.From != nil && booking.To.Before(*q.From) {
			continue
		}
		if q.To != nil && booking.From.After(*q.To) {
			continue
		}
		items = append(items, booking)
	}
	slices.SortFunc(items, func(a, b Booking) int { return a.From.Compare(b.From) })
	return items
}

func (s *MemoryStore) CreateBooking(req CreateBookingRequest) (Booking, error) {
	from, to, err := parseInterval(req.From, req.To, s.now())
	if err != nil {
		return Booking{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rooms[req.RoomID]; !ok {
		return Booking{}, notFound("room_not_found", "room not found")
	}
	now := s.now()
	booking := Booking{ID: s.newIDLocked("booking"), RoomID: req.RoomID, From: from, To: to, Status: "pending", Version: 1, CreatedAt: now, UpdatedAt: now}
	s.bookings[booking.ID] = booking
	return booking, nil
}

func (s *MemoryStore) GetBooking(id string) (Booking, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	booking, ok := s.bookings[id]
	if !ok {
		return Booking{}, notFound("booking_not_found", "booking not found")
	}
	return booking, nil
}

func (s *MemoryStore) UpdateBooking(id string, req UpdateBookingRequest, ifMatch *int) (Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	booking, ok := s.bookings[id]
	if !ok {
		return Booking{}, notFound("booking_not_found", "booking not found")
	}
	if err := checkVersion(booking.Version, req.Version, ifMatch); err != nil {
		return Booking{}, err
	}
	from, to := booking.From, booking.To
	var err error
	if req.From != nil {
		from, err = parseTime(*req.From)
		if err != nil {
			return Booking{}, err
		}
	}
	if req.To != nil {
		to, err = parseTime(*req.To)
		if err != nil {
			return Booking{}, err
		}
	}
	if err := validateInterval(from, to, s.now()); err != nil {
		return Booking{}, err
	}
	if booking.Status == "approved" && s.hasApprovedOverlapLocked(booking.RoomID, from, to, booking.ID) {
		return Booking{}, conflict("booking_conflict", "approved booking overlaps with another approved booking")
	}
	booking.From = from
	booking.To = to
	booking.Version++
	booking.UpdatedAt = s.now()
	s.bookings[id] = booking
	return booking, nil
}

func (s *MemoryStore) CancelBooking(id string, ifMatch *int) (Booking, error) {
	return s.transition(id, "cancelled", ifMatch)
}

func (s *MemoryStore) ApproveBooking(id string, ifMatch *int) (Booking, error) {
	return s.transition(id, "approved", ifMatch)
}

func (s *MemoryStore) RejectBooking(id string, ifMatch *int) (Booking, error) {
	return s.transition(id, "rejected", ifMatch)
}

func (s *MemoryStore) Ready() Readiness {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Readiness{Status: "ready", Rooms: len(s.rooms), Bookings: len(s.bookings)}
}

func (s *MemoryStore) transition(id, target string, ifMatch *int) (Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	booking, ok := s.bookings[id]
	if !ok {
		return Booking{}, notFound("booking_not_found", "booking not found")
	}
	if err := checkVersion(booking.Version, nil, ifMatch); err != nil {
		return Booking{}, err
	}
	if !allowedTransition(booking.Status, target) {
		return Booking{}, conflict("invalid_state_transition", "booking state transition is not allowed")
	}
	if target == "approved" && s.hasApprovedOverlapLocked(booking.RoomID, booking.From, booking.To, booking.ID) {
		return Booking{}, conflict("booking_conflict", "approved booking overlaps with another approved booking")
	}
	booking.Status = target
	booking.Version++
	booking.UpdatedAt = s.now()
	s.bookings[id] = booking
	return booking, nil
}

func (s *MemoryStore) hasApprovedOverlapLocked(roomID string, from, to time.Time, excludeID string) bool {
	for _, booking := range s.bookings {
		if booking.ID == excludeID || booking.RoomID != roomID || booking.Status != "approved" {
			continue
		}
		if from.Before(booking.To) && to.After(booking.From) {
			return true
		}
	}
	return false
}

func (s *MemoryStore) newIDLocked(prefix string) string {
	s.nextID++
	return prefix + "-" + strconv.FormatInt(s.nextID, 10)
}

type Server struct {
	logger *slog.Logger
	store  Store
}

func NewServer(logger *slog.Logger) http.Handler {
	return NewServerWithStore(logger, NewMemoryStore())
}

func NewServerWithStore(logger *slog.Logger, store Store) http.Handler {
	s := &Server{logger: logger, store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /ready", s.ready)
	mux.HandleFunc("GET /api/v1/rooms", s.listRooms)
	mux.HandleFunc("POST /api/v1/rooms", s.createRoom)
	mux.HandleFunc("GET /api/v1/rooms/{id}", s.getRoom)
	mux.HandleFunc("GET /api/v1/bookings", s.listBookings)
	mux.HandleFunc("POST /api/v1/bookings", s.createBooking)
	mux.HandleFunc("GET /api/v1/bookings/{id}", s.getBooking)
	mux.HandleFunc("PATCH /api/v1/bookings/{id}", s.updateBooking)
	mux.HandleFunc("POST /api/v1/bookings/{id}/cancel", s.cancelBooking)
	mux.HandleFunc("POST /api/v1/bookings/{id}/approve", s.approveBooking)
	mux.HandleFunc("POST /api/v1/bookings/{id}/reject", s.rejectBooking)

	return recoverer(requestID(requestLogger(logger)(timeout(5 * time.Second)(limitBody(1 << 20)(apiKeyAuth(mux))))))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "booking-api"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Ready())
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	items := s.store.ListRooms()
	writeJSON(w, http.StatusOK, ListResponse[Room]{Items: items, Total: len(items)})
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var req CreateRoomRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	room, err := s.store.CreateRoom(req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, room)
}

func (s *Server) getRoom(w http.ResponseWriter, r *http.Request) {
	room, err := s.store.GetRoom(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (s *Server) listBookings(w http.ResponseWriter, r *http.Request) {
	q, err := parseBookingQuery(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	items := s.store.ListBookings(q)
	writeJSON(w, http.StatusOK, ListResponse[Booking]{Items: items, Total: len(items)})
}

func (s *Server) createBooking(w http.ResponseWriter, r *http.Request) {
	var req CreateBookingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	booking, err := s.store.CreateBooking(req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, booking)
}

func (s *Server) getBooking(w http.ResponseWriter, r *http.Request) {
	booking, err := s.store.GetBooking(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

func (s *Server) updateBooking(w http.ResponseWriter, r *http.Request) {
	var req UpdateBookingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	booking, err := s.store.UpdateBooking(r.PathValue("id"), req, parseIfMatch(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

func (s *Server) cancelBooking(w http.ResponseWriter, r *http.Request) {
	booking, err := s.store.CancelBooking(r.PathValue("id"), parseIfMatch(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

func (s *Server) approveBooking(w http.ResponseWriter, r *http.Request) {
	booking, err := s.store.ApproveBooking(r.PathValue("id"), parseIfMatch(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

func (s *Server) rejectBooking(w http.ResponseWriter, r *http.Request) {
	booking, err := s.store.RejectBooking(r.PathValue("id"), parseIfMatch(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

func parseBookingQuery(r *http.Request) (BookingQuery, error) {
	query := r.URL.Query()
	q := BookingQuery{RoomID: query.Get("room_id"), Status: query.Get("status")}
	if q.Status != "" && !validStatus(q.Status) {
		return q, badRequest("invalid_status", "status is invalid")
	}
	if raw := query.Get("from"); raw != "" {
		t, err := parseTime(raw)
		if err != nil {
			return q, err
		}
		q.From = &t
	}
	if raw := query.Get("to"); raw != "" {
		t, err := parseTime(raw)
		if err != nil {
			return q, err
		}
		q.To = &t
	}
	return q, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, r, badRequest("invalid_json", "invalid json body"))
		return false
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, r, badRequest("invalid_json", "body must contain a single json object"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var app appError
	if !errors.As(err, &app) {
		app = appError{status: http.StatusInternalServerError, code: "internal_error", message: "internal server error"}
	}
	writeJSON(w, app.status, ErrorResponse{Error: app.code, Message: app.message, RequestID: r.Header.Get("X-Request-ID"), Details: app.details})
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
			writeError(w, r, appError{status: http.StatusUnauthorized, code: "unauthorized", message: "invalid api key"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				writeError(w, r, appError{status: http.StatusInternalServerError, code: "internal_error", message: "internal server error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func parseInterval(rawFrom, rawTo string, now time.Time) (time.Time, time.Time, error) {
	from, err := parseTime(rawFrom)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parseTime(rawTo)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if err := validateInterval(from, to, now); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return from, to, nil
}

func parseTime(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, badRequest("invalid_time", "time must use RFC3339 format")
	}
	return t.UTC(), nil
}

func validateInterval(from, to, now time.Time) error {
	if !from.Before(to) {
		return badRequest("invalid_interval", "from must be before to")
	}
	if from.Before(now) {
		return badRequest("booking_in_past", "booking cannot start in the past")
	}
	return nil
}

func allowedTransition(from, to string) bool {
	switch from + "=>" + to {
	case "pending=>approved", "pending=>rejected", "pending=>cancelled", "approved=>cancelled":
		return true
	default:
		return false
	}
}

func validStatus(status string) bool {
	return status == "pending" || status == "approved" || status == "rejected" || status == "cancelled"
}

func checkVersion(current int, bodyVersion *int, ifMatch *int) error {
	expected := ifMatch
	if expected == nil {
		expected = bodyVersion
	}
	if expected == nil {
		return nil
	}
	if *expected != current {
		return conflict("version_conflict", "booking version does not match")
	}
	return nil
}

func parseIfMatch(r *http.Request) *int {
	raw := strings.Trim(r.Header.Get("If-Match"), `"`)
	if raw == "" {
		return nil
	}
	version, err := strconv.Atoi(raw)
	if err != nil {
		return nil
	}
	return &version
}

func badRequest(code, message string) appError {
	return appError{status: http.StatusBadRequest, code: code, message: message}
}

func notFound(code, message string) appError {
	return appError{status: http.StatusNotFound, code: code, message: message}
}

func conflict(code, message string) appError {
	return appError{status: http.StatusConflict, code: code, message: message}
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
