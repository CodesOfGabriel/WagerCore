package config_test

import (
	"os"
	"testing"
	"time"

	"junglegaming/internal/platform/config"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://jungle:jungle@localhost:5432/jungle")
	for _, key := range []string{"HTTP_ADDR", "HEALTH_TIMEOUT"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.HealthTimeout != 2*time.Second || cfg.DatabaseURL == "" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("HTTP_ADDR", "127.0.0.1:9090")
	t.Setenv("HEALTH_TIMEOUT", "500ms")
	cfg, err := config.Load()
	if err != nil || cfg.HTTPAddr != "127.0.0.1:9090" || cfg.HealthTimeout != 500*time.Millisecond || cfg.DatabaseURL != "postgres://localhost/test" {
		t.Fatalf("config=%+v error=%v", cfg, err)
	}
}

func TestLoadRejectsInvalidEnvironment(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"DATABASE_URL", ""}, {"DATABASE_URL", "  "},
		{"HTTP_ADDR", ""}, {"HTTP_ADDR", "localhost"}, {"HTTP_ADDR", ":0"},
		{"HTTP_ADDR", ":65536"}, {"HTTP_ADDR", ":http"},
		{"HEALTH_TIMEOUT", ""}, {"HEALTH_TIMEOUT", "1"}, {"HEALTH_TIMEOUT", "0s"},
		{"HEALTH_TIMEOUT", "-1s"}, {"HEALTH_TIMEOUT", "6s"},
	} {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("HTTP_ADDR", ":8080")
			t.Setenv("HEALTH_TIMEOUT", "2s")
			t.Setenv(tt.key, tt.value)
			if _, err := config.Load(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestWorkerAndAuthenticationConfiguration(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"OIDC_ISSUER_URL", ""}, {"OIDC_JWKS_URL", "https://user:password@example.com/keys"}, {"OIDC_AUDIENCE", ""},
		{"WORKER_ROLES", "invalid"}, {"WORKER_CONCURRENCY", "0"}, {"DATABASE_MAX_CONNECTIONS", "65"},
		{"LEASE_DURATION", "0s"}, {"REFERENCE_TTL", "2h"}, {"REFERENCE_MAX_ATTEMPTS", "0"}, {"RETRY_BASE", "invalid"},
		{"SQS_VISIBILITY_TIMEOUT", "0"}, {"FAULT_POINT", "after_financial_commit"},
	} {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv(tt.key, tt.value)
			if _, err := config.Load(); err == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
	t.Run("faults restricted to test environment", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://localhost/test")
		t.Setenv("FAULT_POINT", "after_sqs_commit")
		t.Setenv("FAULT_TRANSACTION", "target")
		t.Setenv("APP_ENV", "production")
		if _, err := config.Load(); err == nil {
			t.Fatal("faults enabled in production")
		}
		t.Setenv("APP_ENV", "test")
		if _, err := config.Load(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("AWS default credential chain", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://localhost/test")
		t.Setenv("SQS_ENDPOINT", "")
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
		if _, err := config.Load(); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AWS_ACCESS_KEY_ID", "test")
		if _, err := config.Load(); err == nil {
			t.Fatal("incomplete credential pair accepted")
		}
	})
}
