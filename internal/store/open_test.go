package store_test

import (
	"path/filepath"
	"testing"

	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

func TestOpenPutsTheDatabaseInWALSoDaemonAndCLICanShareIt(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "nested", "nightwatcher.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("lendo journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, quero wal", mode)
	}
}

func TestOpenAllowsASecondConnectionToTheSameFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nightwatcher.db")

	daemon, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open (daemon): %v", err)
	}
	defer daemon.Close()

	if _, err := daemon.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("criando tabela: %v", err)
	}
	if _, err := daemon.Exec(`INSERT INTO t VALUES ('escrito pelo daemon')`); err != nil {
		t.Fatalf("inserindo: %v", err)
	}

	// This is the `dokku run` container reading the daemon's database.
	cli, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open (cli): %v", err)
	}
	defer cli.Close()

	var got string
	if err := cli.QueryRow(`SELECT v FROM t`).Scan(&got); err != nil {
		t.Fatalf("lendo do segundo processo: %v", err)
	}
	if got != "escrito pelo daemon" {
		t.Errorf("segunda conexão leu %q", got)
	}
}
