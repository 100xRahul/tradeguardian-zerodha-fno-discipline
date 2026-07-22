package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMutationSecurity(t *testing.T) {
	server := &Server{publicOrigin: "http://127.0.0.1:8080", allowedHost: "127.0.0.1:8080"}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := server.mutation(next)

	t.Run("rejects foreign origin", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/orders", strings.NewReader(`{}`))
		request.Host = "127.0.0.1:8080"
		request.Header.Set("Origin", "https://evil.example")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
		}
	})

	t.Run("accepts local origin and csrf", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/orders", strings.NewReader(`{}`))
		request.Host = "127.0.0.1:8080"
		request.Header.Set("Origin", "http://127.0.0.1:8080")
		request.Header.Set("X-CSRF-Token", "token")
		request.AddCookie(&http.Cookie{Name: "tg_csrf", Value: "token"})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
		}
	})
}

func TestMutationSecurityBehindHTTPSProxy(t *testing.T) {
	server := &Server{publicOrigin: "https://trade.example.com", allowedHost: "trade.example.com", secureCookie: true}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodPost, "https://trade.example.com/api/orders", strings.NewReader(`{}`))
	request.Host = "trade.example.com"
	request.Header.Set("Origin", "https://trade.example.com")
	request.Header.Set("X-CSRF-Token", "token")
	request.AddCookie(&http.Cookie{Name: "tg_csrf", Value: "token"})
	response := httptest.NewRecorder()
	server.mutation(next).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestPublicOriginValidation(t *testing.T) {
	for _, value := range []string{"", "trade.example.com", "ftp://trade.example.com", "https://user@trade.example.com", "https://trade.example.com/app"} {
		if _, err := validatePublicOrigin(value); err == nil {
			t.Fatalf("validatePublicOrigin(%q) error = nil", value)
		}
	}
	origin, err := validatePublicOrigin("https://trade.example.com/")
	if err != nil || origin.Host != "trade.example.com" {
		t.Fatalf("valid origin = %#v, error = %v", origin, err)
	}
}

func TestSecureCSRFCookie(t *testing.T) {
	server := &Server{secureCookie: true}
	request := httptest.NewRequest(http.MethodGet, "https://trade.example.com/", nil)
	response := httptest.NewRecorder()
	if _, err := server.ensureCSRFCookie(response, request); err != nil {
		t.Fatal(err)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("csrf cookies = %#v", cookies)
	}
}

func TestDecodeJSONRejectsUnknownFieldsAndMultipleObjects(t *testing.T) {
	tests := []string{`{"known":1,"unknown":2}`, `{"known":1}{"known":2}`}
	for _, body := range tests {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		response := httptest.NewRecorder()
		var target struct {
			Known int `json:"known"`
		}
		if err := decodeJSON(response, request, &target); err == nil {
			t.Fatalf("decodeJSON(%q) error = nil", body)
		}
	}
}

func TestBroadcasterCloseIsIdempotent(t *testing.T) {
	broadcast := newBroadcaster()
	broadcast.close()
	broadcast.close()
	select {
	case <-broadcast.done:
	default:
		t.Fatal("broadcaster close did not release live streams")
	}
}
