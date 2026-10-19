package config_test

import (
	"os"
	"strings"
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

func productionEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"APP_ENV": "production", "DATABASE_URL": "postgres://db/wagercore", "HTTP_ADDR": ":8080",
		"OIDC_ISSUER_URL": "https://identity.example.com/realms/wagercore",
		"OIDC_JWKS_URL":   "https://identity.example.com/realms/wagercore/protocol/openid-connect/certs",
		"OIDC_AUDIENCE":   "wagercore-api", "SQS_ENDPOINT": "", "AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "",
		"FAULT_POINT": "", "FAULT_TRANSACTION": "",
	} {
		t.Setenv(key, value)
	}
}

func TestProductionRequiresExplicitIdentityAndRejectsLocalSettings(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"OIDC_ISSUER_URL", ""}, {"OIDC_JWKS_URL", ""}, {"OIDC_AUDIENCE", "  "},
		{"OIDC_ISSUER_URL", "http://identity.example.com/realms/wagercore"},
		{"OIDC_ISSUER_URL", "https://LOCALHOST./realms/wagercore"},
		{"OIDC_JWKS_URL", "https://keys.localhost/certs"},
		{"OIDC_JWKS_URL", "https://127.0.0.1/certs"}, {"OIDC_JWKS_URL", "https://[::1]/certs"},
		{"OIDC_JWKS_URL", "https://0.0.0.0/certs"}, {"OIDC_JWKS_URL", "https://keycloak/certs"},
		{"SQS_ENDPOINT", "http://localstack:4566"}, {"SQS_ENDPOINT", "https://localhost:4566"},
		{"SQS_ENDPOINT", "https://localstack:4566"},
		{"AWS_ACCESS_KEY_ID", "test"}, {"AWS_SECRET_ACCESS_KEY", "test"},
	} {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			productionEnvironment(t)
			t.Setenv(tt.key, tt.value)
			if strings.HasPrefix(tt.key, "AWS_") {
				t.Setenv("AWS_ACCESS_KEY_ID", "real-access-key")
				t.Setenv("AWS_SECRET_ACCESS_KEY", "real-secret")
				t.Setenv(tt.key, tt.value)
			}
			if _, err := config.Load(); err == nil {
				t.Fatal("unsafe production configuration accepted")
			}
		})
	}
	for _, key := range []string{"OIDC_ISSUER_URL", "OIDC_JWKS_URL", "OIDC_AUDIENCE"} {
		t.Run("missing "+key, func(t *testing.T) {
			productionEnvironment(t)
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Load(); err == nil {
				t.Fatal("local identity default accepted in production")
			}
		})
	}
}

func TestProductionUsesAWSDefaultsWithoutLocalCredentials(t *testing.T) {
	productionEnvironment(t)
	for _, key := range []string{"SQS_ENDPOINT", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SQSEndpoint != "" || cfg.AWSAccessKeyID != "" || cfg.AWSSecretAccessKey != "" {
		t.Fatal("local broker defaults leaked into production")
	}
	t.Setenv("SQS_ENDPOINT", "https://sqs.us-east-1.amazonaws.com")
	if _, err := config.Load(); err != nil {
		t.Fatalf("explicit HTTPS broker endpoint rejected: %v", err)
	}
}

func TestProductionWorkerTimeoutWindows(t *testing.T) {
	for _, tt := range []struct {
		name, roles, lease, visibility string
		valid                          bool
	}{
		{"defaults", "all", "30s", "30", true},
		{"expired during publish", "publisher", "1ms", "30", false},
		{"lease at attempt boundary", "publisher", "12s", "30", false},
		{"reference lease at attempt boundary", "references", "12s", "30", false},
		{"lease above attempt boundary", "all", "12001ms", "30", true},
		{"visibility shorter than processing", "consumer", "30s", "1", false},
		{"visibility at cleanup boundary", "all", "30s", "12", false},
		{"visibility above cleanup boundary", "consumer", "30s", "13", true},
		{"publisher does not consume", "publisher", "30s", "1", true},
		{"consumer does not claim leases", "consumer", "1ms", "30", true},
		{"API without workers", "none", "1ms", "1", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			productionEnvironment(t)
			t.Setenv("WORKER_ROLES", tt.roles)
			t.Setenv("LEASE_DURATION", tt.lease)
			t.Setenv("SQS_VISIBILITY_TIMEOUT", tt.visibility)
			_, err := config.Load()
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t error=%v", tt.valid, err)
			}
		})
	}
	for _, environment := range []string{"local", "test"} {
		t.Run(environment+" permits short recovery windows", func(t *testing.T) {
			productionEnvironment(t)
			t.Setenv("APP_ENV", environment)
			t.Setenv("LEASE_DURATION", "1ms")
			t.Setenv("SQS_VISIBILITY_TIMEOUT", "1")
			if _, err := config.Load(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDLQPollingConfiguration(t *testing.T) {
	for _, value := range []string{"", "0s", "999ms", "2h", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("DLQ_POLL_INTERVAL", value)
			if _, err := config.Load(); err == nil {
				t.Fatal("invalid polling interval accepted")
			}
		})
	}
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("DLQ_POLL_INTERVAL", "1m")
	if cfg, err := config.Load(); err != nil || cfg.DLQPollInterval != time.Minute {
		t.Fatalf("interval override: config=%v error=%v", cfg.DLQPollInterval, err)
	}
	if config.Defaults().DLQPollInterval != 30*time.Second {
		t.Fatal("unexpected default DLQ polling interval")
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
