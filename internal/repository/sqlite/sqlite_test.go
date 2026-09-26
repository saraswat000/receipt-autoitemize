package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

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
