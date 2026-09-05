// Package store is the SQLite persistence layer.
//
// At the fleet sizes Bosun targets (1–20 machines) SQLite is not a compromise:
// it needs no configuration, and the user's entire install backs up by copying
// one file. That matters more than throughput here.
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps the binary static and cross-compilable
)

type Store struct {
	db *sql.DB
}

// Open connects to the database at path and applies any pending migrations.
func Open(path string) (*Store, error) {
	// _pragma args are applied per-connection by the driver.
	//   busy_timeout — wait rather than immediately erroring under contention
	//   journal_mode=WAL — readers don't block the writer
	//   foreign_keys — the ON DELETE rules in the schema are load-bearing
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite tolerates one writer. Capping the pool avoids a thundering herd of
	// blocked writes when several actions finish at once.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for callers that need raw access (tests, maintenance).
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create migrations table: %w", err)
	}

	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	for i, stmt := range migrations {
		version := i + 1
		if version <= current {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("store: begin migration %d: %w", version, err)
		}
		if err := execMigration(tx, stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: apply migration %d: %w", version, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, ms(time.Now()),
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %d: %w", version, err)
		}
	}
	return nil
}

// execMigration runs a migration one statement at a time and tolerates a
// column that already exists. That case arises only from history: the seen_*
// columns on host_keys were briefly added by editing migration 001 in place,
// so a database created in that window already has them when migration 002
// tries to add them. "duplicate column name" there is success, not failure.
func execMigration(tx *sql.Tx, stmt string) error {
	for _, raw := range strings.Split(stmt, ";") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if _, err := tx.Exec(s); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// --- time helpers -----------------------------------------------------------
//
// Timestamps are stored as Unix milliseconds rather than as a text or DATETIME
// column. It is unambiguous, sorts correctly, and sidesteps driver-specific
// time parsing entirely.

func ms(t time.Time) int64 { return t.UTC().UnixMilli() }

func msPtr(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	v := ms(*t)
	return &v
}

func fromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }

func fromMsPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMs(v.Int64)
	return &t
}

func nullInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func ptrInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func ptrInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}
