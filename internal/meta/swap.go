package meta

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.sia.tech/core/types"
)

type SwapKey struct {
	SwapID    string
	Public    types.QdayKeys
	CreatedAt time.Time
}

type Swap struct {
	SwapID       string
	Role         string
	Recipient    types.QdayKeys
	Refund       types.QdayKeys
	SecretHash   types.Hash256
	RefundHeight uint64
	Address      types.QdayAddress
	CreatedAt    time.Time
}

type SwapAction struct {
	ActionID    string
	SwapID      string
	Kind        string
	OutputID    string
	Transaction types.V2Transaction
	Basis       types.ChainIndex
	Confirmed   *types.ChainIndex
	Destination types.QdayAddress
	Amount      types.Currency
	Fee         types.Currency
	CreatedAt   time.Time
	LastError   string
}

func decodeQdayKeys(raw []byte) (types.QdayKeys, error) {
	if len(raw) != 64 {
		return types.QdayKeys{}, errors.New("invalid stored QDAY public-key descriptor")
	}
	var keys types.QdayKeys
	copy(keys.Classical[:], raw[:32])
	copy(keys.Reserve[:], raw[32:])
	if err := keys.Validate(); err != nil {
		return types.QdayKeys{}, err
	}
	return keys, nil
}

func (s *Store) AddSwapKey(key SwapKey) error {
	_, err := s.db.Exec(`INSERT INTO swap_keys(swap_id,public_keys,created_at) VALUES(?,?,?)`, key.SwapID, key.Public.Bytes(), key.CreatedAt.Unix())
	return err
}

func scanSwapKey(row interface{ Scan(...any) error }) (SwapKey, error) {
	var key SwapKey
	var raw []byte
	var created int64
	if err := row.Scan(&key.SwapID, &raw, &created); err != nil {
		return key, err
	}
	var err error
	key.Public, err = decodeQdayKeys(raw)
	key.CreatedAt = time.Unix(created, 0).UTC()
	return key, err
}

func (s *Store) SwapKey(swapID string) (SwapKey, bool, error) {
	key, err := scanSwapKey(s.db.QueryRow(`SELECT swap_id,public_keys,created_at FROM swap_keys WHERE swap_id=?`, swapID))
	if errors.Is(err, sql.ErrNoRows) {
		return SwapKey{}, false, nil
	}
	return key, err == nil, err
}

func (s *Store) AddSwap(swap Swap) error {
	_, err := s.db.Exec(`INSERT INTO swaps(swap_id,role,recipient_keys,refund_keys,secret_hash,refund_height,contract_address,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		swap.SwapID, swap.Role, swap.Recipient.Bytes(), swap.Refund.Bytes(), swap.SecretHash[:], swap.RefundHeight, swap.Address.String(), swap.CreatedAt.Unix())
	return err
}

func scanSwap(row interface{ Scan(...any) error }) (Swap, error) {
	var swap Swap
	var recipient, refund, secretHash []byte
	var address string
	var created int64
	if err := row.Scan(&swap.SwapID, &swap.Role, &recipient, &refund, &secretHash, &swap.RefundHeight, &address, &created); err != nil {
		return swap, err
	}
	var err error
	if swap.Recipient, err = decodeQdayKeys(recipient); err != nil {
		return swap, err
	} else if swap.Refund, err = decodeQdayKeys(refund); err != nil {
		return swap, err
	} else if len(secretHash) != 32 {
		return swap, errors.New("invalid stored atomic-swap secret hash")
	}
	copy(swap.SecretHash[:], secretHash)
	swap.Address, err = types.ParseQdayAddress(address)
	if err != nil {
		return swap, err
	}
	contract := types.QdayAtomicSwap{Recipient: swap.Recipient, Refund: swap.Refund, SecretHash: swap.SecretHash, RefundHeight: swap.RefundHeight}
	derived, err := contract.Address()
	if err != nil {
		return swap, err
	} else if derived != swap.Address {
		return swap, errors.New("stored atomic-swap address does not match its contract")
	}
	swap.CreatedAt = time.Unix(created, 0).UTC()
	return swap, nil
}

const swapColumns = `swap_id,role,recipient_keys,refund_keys,secret_hash,refund_height,contract_address,created_at`

func (s *Store) Swap(swapID string) (Swap, bool, error) {
	swap, err := scanSwap(s.db.QueryRow(`SELECT `+swapColumns+` FROM swaps WHERE swap_id=?`, swapID))
	if errors.Is(err, sql.ErrNoRows) {
		return Swap{}, false, nil
	}
	return swap, err == nil, err
}

func (s *Store) Swaps(limit, offset int) ([]Swap, error) {
	if limit < 1 || limit > 10_000 || offset < 0 {
		return nil, errors.New("invalid swap pagination")
	}
	rows, err := s.db.Query(`SELECT `+swapColumns+` FROM swaps ORDER BY created_at DESC,swap_id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Swap
	for rows.Next() {
		swap, err := scanSwap(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, swap)
	}
	return result, rows.Err()
}

func (s *Store) AddSwapAction(action SwapAction) error {
	raw, err := encodeTransaction(action.Transaction)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO swap_actions(action_id,swap_id,kind,output_id,transaction_blob,basis_height,basis_id,destination,amount_atomic,fee_atomic,created_at,last_error) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		action.ActionID, action.SwapID, action.Kind, action.OutputID, raw, action.Basis.Height, action.Basis.ID[:], action.Destination.String(), action.Amount.ExactString(), action.Fee.ExactString(), action.CreatedAt.Unix(), action.LastError)
	return err
}

func scanSwapAction(row interface{ Scan(...any) error }) (SwapAction, error) {
	var action SwapAction
	var raw, basisID, confirmedID []byte
	var destination, amount, fee string
	var created int64
	var confirmedHeight sql.NullInt64
	if err := row.Scan(&action.ActionID, &action.SwapID, &action.Kind, &action.OutputID, &raw, &action.Basis.Height, &basisID, &destination, &amount, &fee, &created, &action.LastError, &confirmedHeight, &confirmedID); err != nil {
		return action, err
	}
	if len(basisID) != 32 {
		return action, errors.New("invalid stored atomic-swap action basis")
	}
	copy(action.Basis.ID[:], basisID)
	if confirmedHeight.Valid != (confirmedID != nil) || confirmedHeight.Int64 < 0 || confirmedID != nil && len(confirmedID) != 32 {
		return action, errors.New("invalid stored atomic-swap action confirmation")
	} else if confirmedHeight.Valid {
		confirmed := types.ChainIndex{Height: uint64(confirmedHeight.Int64)}
		copy(confirmed.ID[:], confirmedID)
		action.Confirmed = &confirmed
	}
	var err error
	if action.Transaction, err = decodeTransaction(raw); err != nil {
		return action, err
	} else if action.Destination, err = types.ParseQdayAddress(destination); err != nil {
		return action, err
	} else if action.Amount, err = types.ParseCurrency(amount); err != nil {
		return action, err
	} else if action.Fee, err = types.ParseCurrency(fee); err != nil {
		return action, err
	}
	action.CreatedAt = time.Unix(created, 0).UTC()
	return action, nil
}

const swapActionColumns = `action_id,swap_id,kind,output_id,transaction_blob,basis_height,basis_id,destination,amount_atomic,fee_atomic,created_at,last_error,confirmed_height,confirmed_block_id`

func (s *Store) SwapAction(swapID, kind, outputID string) (SwapAction, bool, error) {
	action, err := scanSwapAction(s.db.QueryRow(`SELECT `+swapActionColumns+` FROM swap_actions WHERE swap_id=? AND kind=? AND output_id=?`, swapID, kind, outputID))
	if errors.Is(err, sql.ErrNoRows) {
		return SwapAction{}, false, nil
	}
	return action, err == nil, err
}

func (s *Store) SwapActions(swapID string) ([]SwapAction, error) {
	rows, err := s.db.Query(`SELECT `+swapActionColumns+` FROM swap_actions WHERE swap_id=? ORDER BY created_at,action_id`, swapID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SwapAction
	for rows.Next() {
		action, err := scanSwapAction(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, action)
	}
	return result, rows.Err()
}

func (s *Store) SwapActionRebroadcastCandidates(tipHeight, reorgWindow uint64) ([]SwapAction, error) {
	cutoff := uint64(0)
	if tipHeight > reorgWindow {
		cutoff = tipHeight - reorgWindow
	}
	rows, err := s.db.Query(`SELECT `+swapActionColumns+` FROM swap_actions WHERE confirmed_height IS NULL OR confirmed_height>=? ORDER BY created_at`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SwapAction
	for rows.Next() {
		action, err := scanSwapAction(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, action)
	}
	return result, rows.Err()
}

func (s *Store) SetSwapActionConfirmation(actionID string, index *types.ChainIndex) error {
	var height, id any
	if index != nil {
		height = index.Height
		id = index.ID[:]
	}
	result, err := s.db.Exec(`UPDATE swap_actions SET confirmed_height=?,confirmed_block_id=? WHERE action_id=?`, height, id, actionID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("atomic-swap action %q does not exist", actionID)
	}
	return nil
}

func (s *Store) UpdateSwapAction(actionID string, basis types.ChainIndex, txn types.V2Transaction, lastError string) error {
	raw, err := encodeTransaction(txn)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`UPDATE swap_actions SET transaction_blob=?,basis_height=?,basis_id=?,last_error=? WHERE action_id=?`, raw, basis.Height, basis.ID[:], lastError, actionID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("atomic-swap action %q does not exist", actionID)
	}
	return nil
}
