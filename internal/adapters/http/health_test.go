package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpapi "junglegaming/internal/adapters/http"
)

func TestHealthRoutes(t *testing.T) {
	for _, tt := range []struct {
		name       string
		method     string
		path       string
		checkError error
		wantStatus int
		wantCheck  bool
		wantState  string
	}{
		{"live without database", "GET", "/health/live", errors.New("database unavailable"), 200, false, "alive"},
		{"ready", "GET", "/health/ready", nil, 200, true, "ready"},
		{"database unavailable", "GET", "/health/ready", errors.New("private connection details"), 503, true, "not_ready"},
		{"unsupported method", "POST", "/health/live", nil, 405, false, ""},
		{"unknown route", "GET", "/wallets", nil, 404, false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checked := false
			health := httpapi.NewHealth(func(context.Context) error {
				checked = true
				return tt.checkError
			}, time.Second)
			response := httptest.NewRecorder()
			health.Routes().ServeHTTP(response, httptest.NewRequest(tt.method, tt.path, nil))
			if response.Code != tt.wantStatus || checked != tt.wantCheck {
				t.Fatalf("status=%d checked=%t body=%s", response.Code, checked, response.Body)
			}
			if tt.wantState == "" {
				return
			}
			if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("unexpected headers: %v", response.Header())
			}
			var body map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["status"] != tt.wantState {
				t.Fatalf("body=%s error=%v", response.Body, err)
			}
			if strings.Contains(response.Body.String(), "private") {
				t.Fatal("readiness exposed connection details")
			}
		})
	}
}

func TestReadinessRespectsTimeoutAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		var checkErr error
		health := httpapi.NewHealth(func(ctx context.Context) error {
			<-ctx.Done()
			checkErr = ctx.Err()
			return checkErr
		}, time.Millisecond)
		response := httptest.NewRecorder()
		request := httptest.NewRequest("GET", "/health/ready", nil).WithContext(ctx)
		health.Routes().ServeHTTP(response, request)
		cancel()
		want := context.DeadlineExceeded
		if canceled {
			want = context.Canceled
		}
		if response.Code != 503 || !errors.Is(checkErr, want) {
			t.Fatalf("canceled=%t status=%d error=%v", canceled, response.Code, checkErr)
		}
	}
}
