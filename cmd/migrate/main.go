package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"junglegaming/internal/platform/migration"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 || (os.Args[1] != "up" && os.Args[1] != "down" && os.Args[1] != "status") {
		return errors.New("usage: migrate up|down|status")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is required")
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return errors.New("invalid DATABASE_URL")
	}
	cfg.ConnectTimeout = 5 * time.Second
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return errors.New("PostgreSQL connection failed")
	}
	defer conn.Close(context.Background())
	runner, err := migration.New(conn)
	if err != nil {
		return err
	}
	result, err := runner.Run(ctx, os.Args[1])
	if err != nil {
		return err
	}
	fmt.Printf("version=%d changed=%d\n", result.Version, result.Changed)
	return nil
}
