package state

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A box that has been running since before FTW recorded driver provenance
// already has this table, without the column. Opening state.db has to add it,
// or every read of the install history fails from then on — which is how the
// driver repository screen goes blank after an update.
//
// The default is the honest one for a row written before anything was
// recorded: nothing says FTW signed it, so nothing claims FTW signed it.
func TestUpgradingABoxAddsTheProvenanceColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE driver_repo_installs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_url TEXT NOT NULL,
		repo_id TEXT NOT NULL,
		driver_id TEXT NOT NULL,
		logical_path TEXT NOT NULL,
		version TEXT NOT NULL,
		sha256 TEXT NOT NULL,
		installed_path TEXT NOT NULL,
		previous_installed_path TEXT NOT NULL DEFAULT '',
		installed_at_ms INTEGER NOT NULL,
		active INTEGER NOT NULL DEFAULT 0
	) STRICT`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO driver_repo_installs
		(repo_url, repo_id, driver_id, logical_path, version, sha256, installed_path, installed_at_ms, active)
		VALUES ('https://example.invalid', 'ftw-official', 'sungrow', 'drivers/sungrow.lua',
			'1.0.0', 'ab', '/var/lib/ftw/sungrow.lua', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open a state.db written by an older FTW: %v", err)
	}
	defer store.Close()

	installs, err := store.ActiveDriverRepoInstalls()
	if err != nil {
		t.Fatalf("read the install history back: %v", err)
	}
	if len(installs) != 1 {
		t.Fatalf("installs = %+v, want the one row that was already there", installs)
	}
	if installs[0].FTWSigned {
		t.Error("a row written before FTW recorded provenance claims FTW signed it")
	}

	// And a second open must not trip over the column it added the first time.
	store.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if _, err := again.ActiveDriverRepoInstalls(); err != nil {
		t.Fatalf("read after a second open: %v", err)
	}
}
