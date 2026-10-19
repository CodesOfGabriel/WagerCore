package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.uber.org/fx"

	"junglegaming/internal/platform/config"
)

func RegisterServer(lifecycle fx.Lifecycle, cfg config.Config, health *Health, logger *slog.Logger, shutdown fx.Shutdowner) {
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           health.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	done := make(chan struct{})
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", server.Addr)
			if err != nil {
				return err
			}
			go func() {
				defer close(done)
				if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("http_server_failed", "error", err.Error())
					_ = shutdown.Shutdown(fx.ExitCode(1))
				}
			}()
			logger.Info("http_started", "address", listener.Addr().String())
			return nil
		},
		OnStop: func(ctx context.Context) error {
			err := server.Shutdown(ctx)
			if err != nil {
				_ = server.Close()
			}
			<-done
			logger.Info("http_stopped")
			return err
		},
	})
}
