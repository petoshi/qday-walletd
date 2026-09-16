package meta

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"go.sia.tech/core/types"
)

func TestAddressAndWithdrawalRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "meta.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	keys, err := types.QdayKeysFromSeed([32]byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	a := Address{Index: 7, Address: keys.Public.Address(), Public: keys.Public, Reference: "customer-42", Kind: "deposit", CreatedAt: time.Unix(1234, 0).UTC()}
	if err := store.AddAddress(a); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.AddressByReference(a.Reference)
	if err != nil || !ok {
		t.Fatal(err)
	} else if got.Index != a.Index || got.Address != a.Address || got.Public != a.Public || got.Kind != a.Kind || !got.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("address changed after round trip: %#v", got)
	}
	if next, err := store.NextIndex(); err != nil || next != 8 {
		t.Fatalf("next index = %d, %v", next, err)
	}

	destination, err := types.QdayKeysFromSeed([32]byte{9})
	if err != nil {
		t.Fatal(err)
	}
	txn := types.V2Transaction{MinerFee: types.Siacoins(1), SiacoinOutputs: []types.SiacoinOutput{{Value: types.Siacoins(2), Address: destination.Public.Policy().Address()}}}
	w := Withdrawal{
		RequestID: "order-9", Kind: "withdrawal", Transaction: txn, Basis: types.ChainIndex{Height: 11, ID: types.BlockID{1, 2, 3}},
		Destination: destination.Public.Address(), Amount: types.Siacoins(2), Fee: types.Siacoins(1), CreatedAt: time.Unix(5678, 0).UTC(),
	}
	if err := store.AddWithdrawal(w); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := store.Withdrawal(w.RequestID)
	if err != nil || !ok {
		t.Fatal(err)
	} else if loaded.Kind != w.Kind || loaded.Transaction.ID() != txn.ID() || loaded.Basis != w.Basis || loaded.Destination != w.Destination || loaded.Amount != w.Amount || loaded.Fee != w.Fee || !loaded.CreatedAt.Equal(w.CreatedAt) {
		t.Fatalf("withdrawal changed after round trip: %#v", loaded)
	}
	loaded.Basis.Height++
	loaded.LastError = "temporary"
	if err := store.UpdateWithdrawal(loaded.RequestID, loaded.Basis, loaded.Transaction, loaded.LastError); err != nil {
		t.Fatal(err)
	}
	updated, _, err := store.Withdrawal(w.RequestID)
	if err != nil {
		t.Fatal(err)
	} else if updated.Basis != loaded.Basis || updated.LastError != "temporary" {
		t.Fatalf("withdrawal update was not stored: %#v", updated)
	}
	confirmed := types.ChainIndex{Height: 14, ID: types.BlockID{4, 5, 6}}
	if err := store.SetWithdrawalConfirmation(w.RequestID, &confirmed); err != nil {
		t.Fatal(err)
	}
	updated, _, err = store.Withdrawal(w.RequestID)
	if err != nil {
		t.Fatal(err)
	} else if updated.Confirmed == nil || *updated.Confirmed != confirmed {
		t.Fatalf("withdrawal confirmation was not stored: %#v", updated.Confirmed)
	}
	if candidates, err := store.RebroadcastCandidates(1_000, 144); err != nil {
		t.Fatal(err)
	} else if len(candidates) != 0 {
		t.Fatalf("old confirmed withdrawal remained a rebroadcast candidate: %#v", candidates)
	}
	if candidates, err := store.RebroadcastCandidates(100, 144); err != nil {
		t.Fatal(err)
	} else if len(candidates) != 1 {
		t.Fatalf("recent confirmed withdrawal was not monitored for reorgs: %#v", candidates)
	}
	if err := store.SetWithdrawalConfirmation(w.RequestID, nil); err != nil {
		t.Fatal(err)
	}
	updated, _, err = store.Withdrawal(w.RequestID)
	if err != nil || updated.Confirmed != nil {
		t.Fatalf("withdrawal confirmation was not cleared: %#v, %v", updated.Confirmed, err)
	}
}

func TestOpenMigratesDevelopmentSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.sqlite3")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version(version) VALUES(1);
CREATE TABLE withdrawals (
  request_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  transaction_blob BLOB NOT NULL,
  basis_height INTEGER NOT NULL,
  basis_id BLOB NOT NULL,
  destination TEXT NOT NULL,
  amount_atomic TEXT NOT NULL,
  fee_atomic TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT ''
);`)
	if err != nil {
		t.Fatal(err)
	} else if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	} else if version != 2 {
		t.Fatalf("schema version = %d", version)
	}
	rows, err := store.db.Query(`PRAGMA table_info(withdrawals)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if !columns["confirmed_height"] || !columns["confirmed_block_id"] {
		t.Fatalf("confirmation columns missing after migration: %#v", columns)
	}
}
