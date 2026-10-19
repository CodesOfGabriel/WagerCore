package config

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr      string
	DatabaseURL   string
	HealthTimeout time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:      ":8080",
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		HealthTimeout: 2 * time.Second,
	}
	if value, exists := os.LookupEnv("HTTP_ADDR"); exists {
		cfg.HTTPAddr = value
	}
	_, port, err := net.SplitHostPort(cfg.HTTPAddr)
	if err != nil {
		return Config{}, errors.New("HTTP_ADDR must contain a host and port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
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
	return cfg, nil
}
