package localstore_test

import (
	"path/filepath"
	"testing"

	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	"babytracker/server/internal/security/storetest"
)

func TestFilesystemStoreContract(t *testing.T) {
	storetest.Run(t, filesystemFactory())
}

func TestFilesystemGenerationMismatchFailsClosed(t *testing.T) {
	storetest.RunGenerationMismatch(t, filesystemFactory())
}

func filesystemFactory() storetest.Factory {
	return func(t *testing.T) storetest.Store {
		t.Helper()
		root := t.TempDir()
		ledgerDir, headDir := filepath.Join(root, "ledger"), filepath.Join(root, "head")
		opened, err := localstore.Open(ledgerDir, headDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := opened.InitializeEmpty(t.Context()); err != nil {
			t.Fatal(err)
		}
		return storetest.Store{
			Ledger: opened.Ledger,
			Heads:  opened.Heads,
			Reopen: func(t *testing.T) (security.SecurityLedger, security.LedgerHeadStore) {
				reopened, err := localstore.Open(ledgerDir, headDir)
				if err != nil {
					t.Fatal(err)
				}
				return reopened.Ledger, reopened.Heads
			},
		}
	}
}
