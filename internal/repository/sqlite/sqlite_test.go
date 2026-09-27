package sqlite_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/repository"
	"receipt-autoitemize/internal/repository/repotest"
	"receipt-autoitemize/internal/repository/sqlite"
)

func TestContract(t *testing.T) {
	repotest.Run(t, func(t *testing.T) repository.Repository {
		st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		return st
	})
}

// A database created before migrations existed (schema applied, user_version 0,
// data in it) is adopted in place on Open, and reopening is a no-op.
func TestMigrationsUpgradeExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	init, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(init)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO receipts (id, filename, content_type, size_bytes, sha256, storage_path, status, created_at)
		VALUES ('r_old', 'a.txt', 'text/plain', 1, printf('%064d', 1), '/tmp/a', 'PROCESSING', 1773306000000)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for i := 0; i < 2; i++ { // upgrade, then a plain reopen
		st, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		r, err := st.GetReceipt(ctx, "r_old")
		if err != nil || r.Status != domain.ReceiptProcessing {
			t.Fatalf("old row after upgrade: %+v %v", r, err)
		}
		st.Close()
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, _ := sql.Open("sqlite", "file:"+path)
	db.Exec(`PRAGMA user_version = 999`)
	db.Close()
	if _, err := sqlite.Open(context.Background(), path); err == nil {
		t.Fatal("opened a database written by a newer binary")
	}
}

// STRICT tables and CHECKs make the database reject wrongly typed or malformed
// values, whatever the application code does.
func TestSchemaRejectsBadData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "strict.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	insert := func(createdAt, sha any) error {
		_, err := db.Exec(`INSERT INTO receipts (id, filename, content_type, size_bytes, sha256, storage_path, status, created_at)
			VALUES (lower(hex(randomblob(4))), 'a.txt', 'text/plain', 1, ?, '/tmp/a', 'UPLOADED', ?)`, sha, createdAt)
		return err
	}
	good := "0000000000000000000000000000000000000000000000000000000000000001"
	if err := insert(int64(1773306000000), good); err != nil {
		t.Fatalf("valid row refused: %v", err)
	}
	if err := insert("2026-03-12T09:00:00Z", good); err == nil {
		t.Error("STRICT accepted text in the epoch-ms created_at column")
	}
	if err := insert(int64(1), "not-a-sha"); err == nil {
		t.Error("CHECK accepted a malformed sha256")
	}
}
