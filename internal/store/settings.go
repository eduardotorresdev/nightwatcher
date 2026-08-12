package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
)

// Setting reads one stored value. The boolean separates "not set" from "set to the
// empty string", which is what lets the overlay leave a default alone.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lendo configuração %s: %w", key, err)
	}
	return value, true, nil
}

// Settings reads everything stored.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("listando configurações: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("lendo configuração: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSetting writes a value with no interpretation. It exists for settings that are
// not operating numbers; `config set` goes through SetTunable, which validates.
//
// A credential never belongs here. The database is meant to be readable and copyable
// without leaking one, and secrets come from the environment.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("gravando configuração %s: %w", key, err)
	}
	return nil
}

// SetTunable is what `config set` calls: it proves the key exists and the value
// parses before storing it. Validating on the way in is the difference between a
// typo failing at the command line, where the operator is looking, and failing at the
// next daemon start, where nobody is.
func (s *Store) SetTunable(ctx context.Context, key, value string) error {
	probe := config.Defaults()
	if err := probe.Apply(key, value); err != nil {
		return err
	}
	return s.SetSetting(ctx, key, value)
}

// Tunables overlays the stored settings onto config.Defaults(). Keys that are not
// operating numbers are ignored — the table also carries settings that have nothing
// to do with the worker's numbers — but a known key holding an unparseable value is
// an error and not a silent fallback: a hand-edited `deep_timeout` of "20 minutos"
// must not quietly become 20 minutes' worth of default.
func (s *Store) Tunables(ctx context.Context) (config.Tunables, error) {
	t := config.Defaults()

	stored, err := s.Settings(ctx)
	if err != nil {
		return t, err
	}

	// Iterating over the known keys rather than over the stored map keeps the overlay
	// order fixed no matter how the map happens to iterate.
	for _, key := range config.Settable {
		value, ok := stored[key]
		if !ok {
			continue
		}
		if err := t.Apply(key, value); err != nil {
			return config.Defaults(), fmt.Errorf("configuração armazenada inválida: %w", err)
		}
	}
	return t, nil
}
