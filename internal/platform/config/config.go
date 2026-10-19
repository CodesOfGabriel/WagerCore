package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr               string
	DatabaseURL            string
	DatabaseMaxConnections int
	HealthTimeout          time.Duration
	Environment            string
	OIDCIssuerURL          string
	OIDCJWKSURL            string
	OIDCAudience           string
	SQSEndpoint            string
	AWSRegion              string
	AWSAccessKeyID         string
	AWSSecretAccessKey     string
	WorkerRoles            string
	WorkerConcurrency      int
	WorkerInterval         time.Duration
	LeaseDuration          time.Duration
	VisibilityTimeout      int32
	ReferenceTTL           time.Duration
	ReferenceMaxAttempts   int
	RetryBase              time.Duration
	FaultPoint             string
	FaultTransaction       string
}

func Defaults() Config {
	return Config{
		HTTPAddr:               ":8080",
		DatabaseMaxConnections: 10,
		HealthTimeout:          2 * time.Second,
		Environment:            "local", OIDCIssuerURL: "http://localhost:8081/realms/wagercore",
		OIDCJWKSURL: "http://localhost:8081/realms/wagercore/protocol/openid-connect/certs", OIDCAudience: "wagercore-api",
		SQSEndpoint: "http://localhost:4566", AWSRegion: "us-east-1", AWSAccessKeyID: "test", AWSSecretAccessKey: "test",
		WorkerRoles: "all", WorkerConcurrency: 4, WorkerInterval: 200 * time.Millisecond,
		LeaseDuration: 30 * time.Second, VisibilityTimeout: 30, ReferenceTTL: 10 * time.Minute,
		ReferenceMaxAttempts: 10, RetryBase: time.Second,
	}
}

func Load() (Config, error) {
	cfg := Defaults()
	if os.Getenv("APP_ENV") == "production" {
		cfg.OIDCIssuerURL, cfg.OIDCJWKSURL, cfg.OIDCAudience = "", "", ""
		cfg.SQSEndpoint, cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey = "", "", ""
	}
	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	for key, destination := range map[string]*string{
		"APP_ENV": &cfg.Environment, "OIDC_ISSUER_URL": &cfg.OIDCIssuerURL,
		"OIDC_JWKS_URL": &cfg.OIDCJWKSURL, "OIDC_AUDIENCE": &cfg.OIDCAudience,
		"SQS_ENDPOINT": &cfg.SQSEndpoint, "AWS_REGION": &cfg.AWSRegion,
		"AWS_ACCESS_KEY_ID": &cfg.AWSAccessKeyID, "AWS_SECRET_ACCESS_KEY": &cfg.AWSSecretAccessKey,
		"WORKER_ROLES": &cfg.WorkerRoles, "FAULT_POINT": &cfg.FaultPoint, "FAULT_TRANSACTION": &cfg.FaultTransaction,
	} {
		if value, exists := os.LookupEnv(key); exists {
			*destination = value
		}
	}
	if value, exists := os.LookupEnv("HTTP_ADDR"); exists {
		cfg.HTTPAddr = value
	}
	_, port, err := net.SplitHostPort(cfg.HTTPAddr)
	if err != nil {
		return Config{}, errors.New("HTTP_ADDR must contain a host and port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 || (number == 0 && cfg.Environment != "test") {
		return Config{}, errors.New("HTTP_ADDR port must be between 1 and 65535")
	}
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if value, exists := os.LookupEnv("HEALTH_TIMEOUT"); exists {
		timeout, err := time.ParseDuration(value)
		if err != nil || timeout <= 0 || timeout > 5*time.Second {
			return Config{}, errors.New("HEALTH_TIMEOUT must be greater than 0 and at most 5s")
		}
		cfg.HealthTimeout = timeout
	}
	for key, destination := range map[string]*time.Duration{
		"WORKER_INTERVAL": &cfg.WorkerInterval, "LEASE_DURATION": &cfg.LeaseDuration,
		"REFERENCE_TTL": &cfg.ReferenceTTL, "RETRY_BASE": &cfg.RetryBase,
	} {
		if value, exists := os.LookupEnv(key); exists {
			duration, err := time.ParseDuration(value)
			if err != nil || duration < time.Millisecond || duration > time.Hour {
				return Config{}, errors.New(key + " must be between 1ms and 1h")
			}
			*destination = duration
		}
	}
	for key, destination := range map[string]*int{"DATABASE_MAX_CONNECTIONS": &cfg.DatabaseMaxConnections, "WORKER_CONCURRENCY": &cfg.WorkerConcurrency, "REFERENCE_MAX_ATTEMPTS": &cfg.ReferenceMaxAttempts} {
		if value, exists := os.LookupEnv(key); exists {
			number, err := strconv.Atoi(value)
			if err != nil || number < 1 || number > 64 {
				return Config{}, errors.New(key + " must be between 1 and 64")
			}
			*destination = number
		}
	}
	if value, exists := os.LookupEnv("SQS_VISIBILITY_TIMEOUT"); exists {
		number, err := strconv.Atoi(value)
		if err != nil || number < 1 || number > 43200 {
			return Config{}, errors.New("SQS_VISIBILITY_TIMEOUT must be between 1 and 43200")
		}
		cfg.VisibilityTimeout = int32(number)
	}
	for key, value := range map[string]string{"OIDC_ISSUER_URL": cfg.OIDCIssuerURL, "OIDC_JWKS_URL": cfg.OIDCJWKSURL, "SQS_ENDPOINT": cfg.SQSEndpoint} {
		if key == "SQS_ENDPOINT" && value == "" {
			continue
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Config{}, errors.New(key + " must be an absolute HTTP URL without credentials, query or fragment")
		}
		if cfg.Environment == "production" {
			host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
			ip := net.ParseIP(host)
			if parsed.Scheme != "https" || host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "keycloak" || host == "localstack" || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
				return Config{}, errors.New(key + " must use HTTPS and a non-local host in production")
			}
		}
	}
	if strings.TrimSpace(cfg.OIDCAudience) == "" || strings.TrimSpace(cfg.AWSRegion) == "" {
		return Config{}, errors.New("OIDC_AUDIENCE and AWS_REGION are required")
	}
	if (cfg.AWSAccessKeyID == "") != (cfg.AWSSecretAccessKey == "") {
		return Config{}, errors.New("AWS credentials must be supplied together")
	}
	if cfg.Environment == "production" && (strings.EqualFold(strings.TrimSpace(cfg.AWSAccessKeyID), "test") || strings.EqualFold(strings.TrimSpace(cfg.AWSSecretAccessKey), "test")) {
		return Config{}, errors.New("test AWS credentials are forbidden in production")
	}
	switch cfg.WorkerRoles {
	case "all", "none", "publisher", "consumer", "references":
	default:
		return Config{}, errors.New("invalid WORKER_ROLES")
	}
	if cfg.Environment != "local" && cfg.Environment != "test" && cfg.Environment != "production" {
		return Config{}, errors.New("APP_ENV must be local, test or production")
	}
	if cfg.FaultPoint != "" && (cfg.Environment != "test" || cfg.FaultTransaction == "") {
		return Config{}, errors.New("fault injection requires APP_ENV=test and FAULT_TRANSACTION")
	}
	switch cfg.FaultPoint {
	case "", "after_financial_commit", "after_sqs_commit", "after_event_publish":
	default:
		return Config{}, errors.New("invalid FAULT_POINT")
	}
	return cfg, nil
}
