package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBookingFlowStateMachineAndOverlap(t *testing.T) {
	h := NewServerWithStore(slog.New(slog.NewTextHandler(io.Discard, nil)), NewMemoryStore())
	room := doJSON[Room](t, h, http.MethodPost, "/api/v1/rooms", `{"name":"A101"}`, http.StatusCreated, nil)
	from := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	to := time.Now().UTC().Add(25 * time.Hour).Format(time.RFC3339)
	body := `{"room_id":"` + room.ID + `","from":"` + from + `","to":"` + to + `"}`

	first := doJSON[Booking](t, h, http.MethodPost, "/api/v1/bookings", body, http.StatusCreated, nil)
	approved := doJSON[Booking](t, h, http.MethodPost, "/api/v1/bookings/"+first.ID+"/approve", `{}`, http.StatusOK, map[string]string{"If-Match": "1"})
	if approved.Status != "approved" || approved.Version != 2 {
		t.Fatalf("approved booking = %+v", approved)
	}

	second := doJSON[Booking](t, h, http.MethodPost, "/api/v1/bookings", body, http.StatusCreated, nil)
	doJSON[ErrorResponse](t, h, http.MethodPost, "/api/v1/bookings/"+second.ID+"/approve", `{}`, http.StatusConflict, map[string]string{"If-Match": "1"})
	cancelled := doJSON[Booking](t, h, http.MethodPost, "/api/v1/bookings/"+second.ID+"/cancel", `{}`, http.StatusOK, nil)
	if cancelled.Status != "cancelled" {
		t.Fatalf("cancelled status = %s", cancelled.Status)
	}
	doJSON[ErrorResponse](t, h, http.MethodPost, "/api/v1/bookings/"+second.ID+"/reject", `{}`, http.StatusConflict, nil)
}

func TestBookingValidationAuthReadyAndConcurrency(t *testing.T) {
	h := NewServerWithStore(slog.New(slog.NewTextHandler(io.Discard, nil)), NewMemoryStore())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/rooms", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", rec.Code)
	}

	room := doJSON[Room](t, h, http.MethodPost, "/api/v1/rooms", `{"name":"A101"}`, http.StatusCreated, nil)
	pastFrom := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	pastTo := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	doJSON[ErrorResponse](t, h, http.MethodPost, "/api/v1/bookings", `{"room_id":"`+room.ID+`","from":"`+pastFrom+`","to":"`+pastTo+`"}`, http.StatusBadRequest, nil)

	from := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	to := time.Now().UTC().Add(49 * time.Hour).Format(time.RFC3339)
	booking := doJSON[Booking](t, h, http.MethodPost, "/api/v1/bookings", `{"room_id":"`+room.ID+`","from":"`+from+`","to":"`+to+`"}`, http.StatusCreated, nil)
	doJSON[ErrorResponse](t, h, http.MethodPatch, "/api/v1/bookings/"+booking.ID, `{"version":99,"to":"`+time.Now().UTC().Add(50*time.Hour).Format(time.RFC3339)+`"}`, http.StatusConflict, nil)

	readyReq := httptest.NewRequest(http.MethodGet, "/ready", nil)
	readyRec := httptest.NewRecorder()
	h.ServeHTTP(readyRec, readyReq)
	if readyRec.Code != http.StatusOK {
		t.Fatalf("ready status = %d", readyRec.Code)
	}
	var ready Readiness
	if err := json.NewDecoder(readyRec.Body).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if ready.Rooms != 1 || ready.Bookings != 1 {
		t.Fatalf("ready = %+v", ready)
	}
}

func doJSON[T any](t *testing.T, h http.Handler, method, path, body string, want int, headers map[string]string) T {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("%s %s status = %d, want %d, body=%s", method, path, rec.Code, want, rec.Body.String())
	}
	var out T
	if rec.Body.Len() > 0 {
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
		}
	}
	return out
}
