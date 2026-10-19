package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"go.uber.org/fx"

	httpapi "junglegaming/internal/adapters/http"
	"junglegaming/internal/platform/config"
)

func TestServerLifecycle(t *testing.T) {
	addresses := make(chan string, 1)
	logger := slog.New(slog.NewJSONHandler(addressWriter{addresses}, nil))
	app := serverApp("127.0.0.1:0", logger)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := app.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	var address string
	select {
	case address = <-addresses:
	default:
		t.Fatal("server did not report its address")
	}
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	response, err := client.Get("http://" + address + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s error=%v", response.StatusCode, body, err)
	}
	if err := app.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if response, err := client.Get("http://" + address + "/health/live"); err == nil {
		response.Body.Close()
		t.Fatal("server still accepts requests after shutdown")
	}
}

func TestServerFailsWhenPortIsOccupied(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	app := serverApp(listener.Addr().String(), logger)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := app.Start(ctx); err == nil {
		_ = app.Stop(ctx)
		t.Fatal("server started on an occupied port")
	}
}

func serverApp(address string, logger *slog.Logger) *fx.App {
	return fx.New(
		fx.NopLogger,
		fx.Supply(config.Config{HTTPAddr: address}),
		fx.Supply(logger),
		fx.Provide(func() http.Handler {
			return httpapi.NewHealth(func(context.Context) error { return nil }, time.Second).Routes()
		}),
		fx.Invoke(httpapi.RegisterServer),
	)
}

type addressWriter struct {
	addresses chan<- string
}

func (w addressWriter) Write(data []byte) (int, error) {
	var entry struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return 0, err
	}
	if entry.Address != "" {
		w.addresses <- entry.Address
	}
	return len(data), nil
}
