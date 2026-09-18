package meta

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.sia.tech/core/types"
)

type Address struct {
	Index     uint64
	Address   types.QdayAddress
	Public    types.QdayKeys
	Reference string
	Kind      string
	CreatedAt time.Time
}

type Withdrawal struct {
	RequestID   string
	Kind        string
	Transaction types.V2Transaction
	Basis       types.ChainIndex
	Confirmed   *types.ChainIndex
	Destination types.QdayAddress
	Amount      types.Currency
	Fee         types.Currency
	CreatedAt   time.Time
	LastError   string
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL&_busy_timeout=10000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version(version) SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM schema_version);
CREATE TABLE IF NOT EXISTS addresses (
  child_index INTEGER PRIMARY KEY,
  address TEXT UNIQUE NOT NULL,
  public_keys BLOB NOT NULL CHECK(length(public_keys) = 64),
  reference TEXT UNIQUE,
  kind TEXT NOT NULL CHECK(kind IN ('deposit','change')),
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS withdrawals (
  request_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK(kind IN ('withdrawal','defend')),
  transaction_blob BLOB NOT NULL,
  basis_height INTEGER NOT NULL,
  basis_id BLOB NOT NULL CHECK(length(basis_id) = 32),
  destination TEXT NOT NULL,
  amount_atomic TEXT NOT NULL,
  fee_atomic TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  confirmed_height INTEGER,
  confirmed_block_id BLOB CHECK(confirmed_block_id IS NULL OR length(confirmed_block_id) = 32),
  CHECK((confirmed_height IS NULL) = (confirmed_block_id IS NULL))
);
CREATE TABLE IF NOT EXISTS swap_keys (
  swap_id TEXT PRIMARY KEY,
  public_keys BLOB NOT NULL CHECK(length(public_keys) = 64),
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS swaps (
  swap_id TEXT PRIMARY KEY REFERENCES swap_keys(swap_id),
  role TEXT NOT NULL CHECK(role IN ('recipient','refund')),
  recipient_keys BLOB NOT NULL CHECK(length(recipient_keys) = 64),
  refund_keys BLOB NOT NULL CHECK(length(refund_keys) = 64),
  secret_hash BLOB NOT NULL CHECK(length(secret_hash) = 32),
  refund_height INTEGER NOT NULL CHECK(refund_height > 0),
  contract_address TEXT UNIQUE NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS swap_actions (
  action_id TEXT PRIMARY KEY,
  swap_id TEXT NOT NULL REFERENCES swaps(swap_id),
  kind TEXT NOT NULL CHECK(kind IN ('fund','claim','refund')),
  output_id TEXT NOT NULL,
  transaction_blob BLOB NOT NULL,
  basis_height INTEGER NOT NULL,
  basis_id BLOB NOT NULL CHECK(length(basis_id) = 32),
  destination TEXT NOT NULL,
  amount_atomic TEXT NOT NULL,
  fee_atomic TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  confirmed_height INTEGER,
  confirmed_block_id BLOB CHECK(confirmed_block_id IS NULL OR length(confirmed_block_id) = 32),
  CHECK((confirmed_height IS NULL) = (confirmed_block_id IS NULL)),
  UNIQUE(swap_id,kind,output_id)
);`); err != nil {
		db.Close()
		return nil, err
	}
	// Development builds before the first public release created the table
	// without confirmation columns. Keep those databases usable.
	columns := make(map[string]bool)
	rows, err := db.Query(`PRAGMA table_info(withdrawals)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			db.Close()
			return nil, err
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		db.Close()
		return nil, err
	}
	if !columns["confirmed_height"] {
		if _, err := db.Exec(`ALTER TABLE withdrawals ADD COLUMN confirmed_height INTEGER`); err != nil {
			db.Close()
			return nil, err
		}
	}
	if !columns["confirmed_block_id"] {
		if _, err := db.Exec(`ALTER TABLE withdrawals ADD COLUMN confirmed_block_id BLOB`); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(`
CREATE INDEX IF NOT EXISTS withdrawals_confirmed_height ON withdrawals(confirmed_height);
CREATE INDEX IF NOT EXISTS swap_actions_confirmed_height ON swap_actions(confirmed_height);
CREATE INDEX IF NOT EXISTS swap_actions_swap_id ON swap_actions(swap_id,created_at);
UPDATE schema_version SET version=3`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func decodeAddress(index uint64, address string, public []byte, reference, kind string, created int64) (Address, error) {
	a, err := types.ParseQdayAddress(address)
	if err != nil {
		return Address{}, err
	}
	if len(public) != 64 {
		return Address{}, errors.New("invalid stored public-key descriptor")
	}
	var keys types.QdayKeys
	copy(keys.Classical[:], public[:32])
	copy(keys.Reserve[:], public[32:])
	if err := keys.Validate(); err != nil {
		return Address{}, err
	} else if keys.Address() != a {
		return Address{}, errors.New("stored address does not match public keys")
	}
	return Address{Index: index, Address: a, Public: keys, Reference: reference, Kind: kind, CreatedAt: time.Unix(created, 0).UTC()}, nil
}

func (s *Store) Addresses() ([]Address, error) {
	var result []Address
	for offset := 0; ; offset += 10_000 {
		page, err := s.AddressesPage(10_000, offset)
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < 10_000 {
			return result, nil
		}
	}
}

func (s *Store) AddressCount() (uint64, error) {
	var count uint64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM addresses`).Scan(&count)
	return count, err
}

func (s *Store) AddressesPage(limit, offset int) ([]Address, error) {
	if limit < 1 || limit > 10_000 || offset < 0 {
		return nil, errors.New("invalid address pagination")
	}
	rows, err := s.db.Query(`SELECT child_index,address,public_keys,COALESCE(reference,''),kind,created_at FROM addresses ORDER BY child_index LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Address
	for rows.Next() {
		var index uint64
		var address, reference, kind string
		var public []byte
		var created int64
		if err := rows.Scan(&index, &address, &public, &reference, &kind, &created); err != nil {
			return nil, err
		}
		a, err := decodeAddress(index, address, public, reference, kind, created)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) AddressByReference(reference string) (Address, bool, error) {
	var index uint64
	var address, ref, kind string
	var public []byte
	var created int64
	err := s.db.QueryRow(`SELECT child_index,address,public_keys,reference,kind,created_at FROM addresses WHERE reference=?`, reference).Scan(&index, &address, &public, &ref, &kind, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Address{}, false, nil
	} else if err != nil {
		return Address{}, false, err
	}
	a, err := decodeAddress(index, address, public, ref, kind, created)
	return a, err == nil, err
}

func (s *Store) AddressByIndex(index uint64) (Address, bool, error) {
	var child uint64
	var address, reference, kind string
	var public []byte
	var created int64
	err := s.db.QueryRow(`SELECT child_index,address,public_keys,COALESCE(reference,''),kind,created_at FROM addresses WHERE child_index=?`, index).Scan(&child, &address, &public, &reference, &kind, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Address{}, false, nil
	} else if err != nil {
		return Address{}, false, err
	}
	a, err := decodeAddress(child, address, public, reference, kind, created)
	return a, err == nil, err
}

func (s *Store) AddressByHash(address types.Address) (Address, bool, error) {
	var index uint64
	var encoded, reference, kind string
	var public []byte
	var created int64
	err := s.db.QueryRow(`SELECT child_index,address,public_keys,COALESCE(reference,''),kind,created_at FROM addresses WHERE address=?`, types.QdayAddress(address).String()).Scan(&index, &encoded, &public, &reference, &kind, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Address{}, false, nil
	} else if err != nil {
		return Address{}, false, err
	}
	a, err := decodeAddress(index, encoded, public, reference, kind, created)
	return a, err == nil, err
}

func (s *Store) NextIndex() (uint64, error) {
	var next uint64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(child_index)+1,0) FROM addresses`).Scan(&next)
	return next, err
}

func (s *Store) AddAddress(a Address) error {
	var reference any
	if a.Reference != "" {
		reference = a.Reference
	}
	_, err := s.db.Exec(`INSERT INTO addresses(child_index,address,public_keys,reference,kind,created_at) VALUES(?,?,?,?,?,?)`, a.Index, a.Address.String(), a.Public.Bytes(), reference, a.Kind, a.CreatedAt.Unix())
	return err
}

func encodeTransaction(txn types.V2Transaction) ([]byte, error) {
	var buf bytes.Buffer
	e := types.NewEncoder(&buf)
	txn.EncodeTo(e)
	if err := e.Flush(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeTransaction(raw []byte) (types.V2Transaction, error) {
	if len(raw) == 0 || len(raw) > 4<<20 {
		return types.V2Transaction{}, errors.New("invalid stored transaction size")
	}
	var txn types.V2Transaction
	d := types.NewBufDecoder(raw)
	txn.DecodeFrom(d)
	if err := d.Err(); err != nil {
		return txn, err
	}
	canonical, err := encodeTransaction(txn)
	if err != nil {
		return txn, err
	} else if !bytes.Equal(raw, canonical) {
		return txn, errors.New("non-canonical stored transaction")
	}
	return txn, nil
}

func (s *Store) AddWithdrawal(w Withdrawal) error {
	raw, err := encodeTransaction(w.Transaction)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO withdrawals(request_id,kind,transaction_blob,basis_height,basis_id,destination,amount_atomic,fee_atomic,created_at,last_error) VALUES(?,?,?,?,?,?,?,?,?,?)`, w.RequestID, w.Kind, raw, w.Basis.Height, w.Basis.ID[:], w.Destination.String(), w.Amount.ExactString(), w.Fee.ExactString(), w.CreatedAt.Unix(), w.LastError)
	return err
}

func scanWithdrawal(row interface{ Scan(...any) error }) (Withdrawal, error) {
	var w Withdrawal
	var raw, basisID, confirmedID []byte
	var destination, amount, fee string
	var created int64
	var confirmedHeight sql.NullInt64
	if err := row.Scan(&w.RequestID, &w.Kind, &raw, &w.Basis.Height, &basisID, &destination, &amount, &fee, &created, &w.LastError, &confirmedHeight, &confirmedID); err != nil {
		return w, err
	}
	if len(basisID) != 32 {
		return w, errors.New("invalid stored withdrawal basis")
	}
	copy(w.Basis.ID[:], basisID)
	if confirmedHeight.Valid != (confirmedID != nil) || confirmedHeight.Int64 < 0 || confirmedID != nil && len(confirmedID) != 32 {
		return w, errors.New("invalid stored withdrawal confirmation")
	} else if confirmedHeight.Valid {
		confirmed := types.ChainIndex{Height: uint64(confirmedHeight.Int64)}
		copy(confirmed.ID[:], confirmedID)
		w.Confirmed = &confirmed
	}
	var err error
	w.Transaction, err = decodeTransaction(raw)
	if err != nil {
		return w, err
	}
	w.Destination, err = types.ParseQdayAddress(destination)
	if err != nil {
		return w, err
	}
	w.Amount, err = types.ParseCurrency(amount)
	if err != nil {
		return w, err
	}
	w.Fee, err = types.ParseCurrency(fee)
	w.CreatedAt = time.Unix(created, 0).UTC()
	return w, err
}

func (s *Store) Withdrawal(requestID string) (Withdrawal, bool, error) {
	w, err := scanWithdrawal(s.db.QueryRow(`SELECT request_id,kind,transaction_blob,basis_height,basis_id,destination,amount_atomic,fee_atomic,created_at,last_error,confirmed_height,confirmed_block_id FROM withdrawals WHERE request_id=?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return Withdrawal{}, false, nil
	}
	return w, err == nil, err
}

func (s *Store) Withdrawals(limit, offset int) ([]Withdrawal, error) {
	if limit < 1 || limit > 200 || offset < 0 {
		return nil, errors.New("invalid withdrawal pagination")
	}
	rows, err := s.db.Query(`SELECT request_id,kind,transaction_blob,basis_height,basis_id,destination,amount_atomic,fee_atomic,created_at,last_error,confirmed_height,confirmed_block_id FROM withdrawals ORDER BY created_at DESC,request_id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Withdrawal
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, w)
	}
	return result, rows.Err()
}

func (s *Store) RebroadcastCandidates(tipHeight, reorgWindow uint64) ([]Withdrawal, error) {
	cutoff := uint64(0)
	if tipHeight > reorgWindow {
		cutoff = tipHeight - reorgWindow
	}
	rows, err := s.db.Query(`SELECT request_id,kind,transaction_blob,basis_height,basis_id,destination,amount_atomic,fee_atomic,created_at,last_error,confirmed_height,confirmed_block_id FROM withdrawals WHERE confirmed_height IS NULL OR confirmed_height>=? ORDER BY created_at`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Withdrawal
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, w)
	}
	return result, rows.Err()
}

func (s *Store) SetWithdrawalConfirmation(requestID string, index *types.ChainIndex) error {
	var height, id any
	if index != nil {
		height = index.Height
		id = index.ID[:]
	}
	result, err := s.db.Exec(`UPDATE withdrawals SET confirmed_height=?,confirmed_block_id=? WHERE request_id=?`, height, id, requestID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("withdrawal %q does not exist", requestID)
	}
	return nil
}

func (s *Store) UpdateWithdrawal(requestID string, basis types.ChainIndex, txn types.V2Transaction, lastError string) error {
	raw, err := encodeTransaction(txn)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`UPDATE withdrawals SET transaction_blob=?,basis_height=?,basis_id=?,last_error=? WHERE request_id=?`, raw, basis.Height, basis.ID[:], lastError, requestID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("withdrawal %q does not exist", requestID)
	}
	return nil
}
