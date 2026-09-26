package memory_test

import (
	"testing"

	"receipt-autoitemize/internal/repository"
	"receipt-autoitemize/internal/repository/memory"
	"receipt-autoitemize/internal/repository/repotest"
)

func TestContract(t *testing.T) {
	repotest.Run(t, func(*testing.T) repository.Repository { return memory.New() })
}
