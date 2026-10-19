package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"junglegaming/migrations"
)

type step struct {
	version  int64
	name     string
	up       string
	down     string
	checksum [32]byte
}

type Result struct {
	Version int64
	Changed int
}

type Runner struct {
	conn  *pgx.Conn
	steps []step
}

func New(conn *pgx.Conn) (*Runner, error) {
	files, err := fs.Glob(migrations.Files, "*.up.sql")
	if err != nil {
		return nil, err
	}
	runner := &Runner{conn: conn}
	for i, file := range files {
		prefix := strings.TrimSuffix(file, ".up.sql")
		versionText, name, ok := strings.Cut(prefix, "_")
		version, err := strconv.ParseInt(versionText, 10, 64)
		if !ok || name == "" || err != nil || version != int64(i+1) {
			return nil, fmt.Errorf("invalid migration sequence: %s", file)
		}
		up, err := migrations.Files.ReadFile(file)
		if err != nil {
			return nil, err
		}
		down, err := migrations.Files.ReadFile(prefix + ".down.sql")
		if err != nil {
			return nil, err
		}
		runner.steps = append(runner.steps, step{
			version: version, name: name, up: string(up), down: string(down),
			checksum: sha256.Sum256([]byte(string(up) + "\x00" + string(down))),
		})
	}
	if len(runner.steps) == 0 {
		return nil, errors.New("no migrations found")
	}
	return runner, nil
}

func (r *Runner) Run(ctx context.Context, action string) (Result, error) {
	if action != "up" && action != "down" && action != "status" {
		return Result{}, errors.New("migration action must be up, down or status")
	}
	tx, err := r.conn.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(74119, 1)"); err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version BIGINT PRIMARY KEY,
        name TEXT NOT NULL,
        checksum BYTEA NOT NULL CHECK (octet_length(checksum) = 32),
        applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`); err != nil {
		return Result{}, err
	}
	count, err := r.checkApplied(ctx, tx)
	if err != nil {
		return Result{}, err
	}
	result := Result{Version: int64(count)}
	switch action {
	case "up":
		for _, migration := range r.steps[count:] {
			if _, err := tx.Exec(ctx, migration.up); err != nil {
				return Result{}, fmt.Errorf("apply migration %06d: %w", migration.version, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)", migration.version, migration.name, migration.checksum[:]); err != nil {
				return Result{}, err
			}
			result.Version = migration.version
			result.Changed++
		}
	case "down":
		if count > 0 {
			migration := r.steps[count-1]
			if _, err := tx.Exec(ctx, migration.down); err != nil {
				return Result{}, fmt.Errorf("revert migration %06d: %w", migration.version, err)
			}
			if _, err := tx.Exec(ctx, "DELETE FROM schema_migrations WHERE version = $1", migration.version); err != nil {
				return Result{}, err
			}
			result.Version--
			result.Changed = 1
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (r *Runner) checkApplied(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var version int64
		var name string
		var checksum []byte
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return 0, err
		}
		if count >= len(r.steps) {
			return 0, fmt.Errorf("migration %06d is missing from this build", version)
		}
		migration := r.steps[count]
		if version != migration.version || name != migration.name || !bytes.Equal(checksum, migration.checksum[:]) {
			return 0, fmt.Errorf("migration %06d differs from its applied version", version)
		}
		count++
	}
	return count, rows.Err()
}
