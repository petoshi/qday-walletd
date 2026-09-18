package meta

import (
	"crypto/sha256"
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

func TestSwapJournalRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "meta.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recipientPrivate, err := types.QdayKeysFromSeed([32]byte{1, 9})
	if err != nil {
		t.Fatal(err)
	}
	refundPrivate, err := types.QdayKeysFromSeed([32]byte{2, 9})
	if err != nil {
		t.Fatal(err)
	}
	key := SwapKey{SwapID: "dex-order-42", Public: recipientPrivate.Public, CreatedAt: time.Unix(100, 0).UTC()}
	if err := store.AddSwapKey(key); err != nil {
		t.Fatal(err)
	}
	loadedKey, ok, err := store.SwapKey(key.SwapID)
	if err != nil || !ok {
		t.Fatal(err)
	} else if loadedKey.Public != key.Public || !loadedKey.CreatedAt.Equal(key.CreatedAt) {
		t.Fatalf("swap key changed after round trip: %#v", loadedKey)
	}
	secret := [32]byte{3, 9}
	contract := types.QdayAtomicSwap{
		Recipient: recipientPrivate.Public, Refund: refundPrivate.Public,
		SecretHash: types.Hash256(sha256.Sum256(secret[:])), RefundHeight: 500,
	}
	address, err := contract.Address()
	if err != nil {
		t.Fatal(err)
	}
	swap := Swap{
		SwapID: key.SwapID, Role: "recipient", Recipient: contract.Recipient, Refund: contract.Refund,
		SecretHash: contract.SecretHash, RefundHeight: contract.RefundHeight, Address: address, CreatedAt: time.Unix(101, 0).UTC(),
	}
	if err := store.AddSwap(swap); err != nil {
		t.Fatal(err)
	}
	loadedSwap, ok, err := store.Swap(swap.SwapID)
	if err != nil || !ok {
		t.Fatal(err)
	} else if !sameStoredSwap(loadedSwap, swap) {
		t.Fatalf("swap changed after round trip: %#v", loadedSwap)
	}
	txn := types.V2Transaction{
		SiacoinOutputs: []types.SiacoinOutput{{Value: types.Siacoins(2), Address: types.Address(address)}},
		MinerFee:       types.Siacoins(1),
	}
	action := SwapAction{
		ActionID: swap.SwapID + ":fund", SwapID: swap.SwapID, Kind: "fund", Transaction: txn,
		Basis: types.ChainIndex{Height: 11, ID: types.BlockID{4}}, Destination: address,
		Amount: types.Siacoins(2), Fee: types.Siacoins(1), CreatedAt: time.Unix(102, 0).UTC(),
	}
	if err := store.AddSwapAction(action); err != nil {
		t.Fatal(err)
	}
	loadedAction, ok, err := store.SwapAction(swap.SwapID, "fund", "")
	if err != nil || !ok {
		t.Fatal(err)
	} else if loadedAction.Transaction.ID() != action.Transaction.ID() || loadedAction.Basis != action.Basis || loadedAction.Amount != action.Amount || loadedAction.Fee != action.Fee {
		t.Fatalf("swap action changed after round trip: %#v", loadedAction)
	}
	confirmed := types.ChainIndex{Height: 12, ID: types.BlockID{5}}
	if err := store.SetSwapActionConfirmation(action.ActionID, &confirmed); err != nil {
		t.Fatal(err)
	}
	loadedAction, _, err = store.SwapAction(swap.SwapID, "fund", "")
	if err != nil || loadedAction.Confirmed == nil || *loadedAction.Confirmed != confirmed {
		t.Fatalf("swap action confirmation was not stored: %#v, %v", loadedAction.Confirmed, err)
	}
	loadedAction.Basis.Height++
	if err := store.UpdateSwapAction(action.ActionID, loadedAction.Basis, loadedAction.Transaction, "retry"); err != nil {
		t.Fatal(err)
	}
	loadedAction, _, err = store.SwapAction(swap.SwapID, "fund", "")
	if err != nil || loadedAction.Basis.Height != 12 || loadedAction.LastError != "retry" {
		t.Fatalf("swap action update was not stored: %#v, %v", loadedAction, err)
	}
}

func sameStoredSwap(a, b Swap) bool {
	return a.SwapID == b.SwapID && a.Role == b.Role && a.Recipient == b.Recipient && a.Refund == b.Refund && a.SecretHash == b.SecretHash && a.RefundHeight == b.RefundHeight && a.Address == b.Address && a.CreatedAt.Equal(b.CreatedAt)
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
	} else if version != 3 {
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
	for _, table := range []string{"swap_keys", "swaps", "swap_actions"} {
		var name string
		if err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("migration did not create %s: %v", table, err)
		}
	}
}
