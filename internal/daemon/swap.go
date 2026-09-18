package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/petoshi/qday-walletd/internal/derive"
	"github.com/petoshi/qday-walletd/internal/meta"
	"go.sia.tech/core/consensus"
	"go.sia.tech/core/types"
	mining "go.sia.tech/coreutils/qday"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/walletd/v2/wallet"
	"go.uber.org/zap"
)

type SwapKeysView struct {
	Classical string `json:"classical"`
	Reserve   string `json:"reserve"`
	Address   string `json:"address"`
}

type SwapKeyRequest struct {
	SwapID string `json:"swapID"`
}

type SwapKeyView struct {
	SwapID    string       `json:"swapID"`
	Keys      SwapKeysView `json:"keys"`
	CreatedAt time.Time    `json:"createdAt"`
}

type RegisterSwapRequest struct {
	SwapID       string       `json:"swapID"`
	Role         string       `json:"role"`
	Counterparty SwapKeysView `json:"counterparty"`
	SecretHash   string       `json:"secretHash"`
	RefundHeight uint64       `json:"refundHeight"`
}

type FundSwapRequest struct {
	AmountAtomic       string `json:"amountAtomic"`
	FeeAtomic          string `json:"feeAtomic,omitempty"`
	ExpectedUnitAtomic string `json:"expectedUnitAtomic"`
}

type SpendSwapRequest struct {
	OutputID  string `json:"outputID"`
	FeeAtomic string `json:"feeAtomic,omitempty"`
	Secret    string `json:"secret,omitempty"`
}

type SwapActionView struct {
	ActionID      string    `json:"actionID"`
	Kind          string    `json:"kind"`
	OutputID      string    `json:"outputID,omitempty"`
	TransactionID string    `json:"transactionID"`
	Destination   string    `json:"destination"`
	Amount        Amount    `json:"amount"`
	Fee           Amount    `json:"fee"`
	Basis         string    `json:"basis"`
	Status        string    `json:"status"`
	Confirmations uint64    `json:"confirmations"`
	BlockHeight   *uint64   `json:"blockHeight,omitempty"`
	BlockID       string    `json:"blockID,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	LastError     string    `json:"lastError,omitempty"`
}

type SwapOutputView struct {
	ID                 string  `json:"id"`
	Value              Amount  `json:"value"`
	Spendable          *Amount `json:"spendable,omitempty"`
	MaturityHeight     uint64  `json:"maturityHeight"`
	FundingTransaction string  `json:"fundingTransaction"`
	FundingHeight      *uint64 `json:"fundingHeight,omitempty"`
	Confirmations      uint64  `json:"confirmations"`
	Status             string  `json:"status"`
	SpendTransaction   string  `json:"spendTransaction,omitempty"`
	SpendHeight        *uint64 `json:"spendHeight,omitempty"`
	RevealedSecret     string  `json:"revealedSecret,omitempty"`
}

type SwapView struct {
	SwapID       string           `json:"swapID"`
	Role         string           `json:"role"`
	Status       string           `json:"status"`
	Local        SwapKeysView     `json:"local"`
	Recipient    SwapKeysView     `json:"recipient"`
	Refund       SwapKeysView     `json:"refund"`
	SecretHash   string           `json:"secretHash"`
	RefundHeight uint64           `json:"refundHeight"`
	Address      string           `json:"address"`
	Height       uint64           `json:"height"`
	Outputs      []SwapOutputView `json:"outputs"`
	Actions      []SwapActionView `json:"actions"`
	CreatedAt    time.Time        `json:"createdAt"`
}

func swapKeysView(keys types.QdayKeys) SwapKeysView {
	return SwapKeysView{
		Classical: hex.EncodeToString(keys.Classical[:]),
		Reserve:   hex.EncodeToString(keys.Reserve[:]),
		Address:   keys.Address().String(),
	}
}

func parseSwapKeys(name string, view SwapKeysView) (types.QdayKeys, error) {
	var keys types.QdayKeys
	decode := func(field, value string, dst []byte) error {
		if len(value) != 64 || value != strings.ToLower(value) {
			return fmt.Errorf("%s.%s must be 32-byte lowercase hexadecimal data", name, field)
		}
		raw, err := hex.DecodeString(value)
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("%s.%s must be 32-byte lowercase hexadecimal data", name, field)
		}
		copy(dst, raw)
		return nil
	}
	if err := decode("classical", view.Classical, keys.Classical[:]); err != nil {
		return keys, err
	} else if err := decode("reserve", view.Reserve, keys.Reserve[:]); err != nil {
		return keys, err
	} else if err := keys.Validate(); err != nil {
		return keys, fmt.Errorf("%s: %w", name, err)
	} else if view.Address != "" && view.Address != keys.Address().String() {
		return keys, fmt.Errorf("%s.address does not match its public keys", name)
	}
	return keys, nil
}

func parseHash(name, value string) (types.Hash256, error) {
	var hash types.Hash256
	if len(value) != 64 || value != strings.ToLower(value) {
		return hash, fmt.Errorf("%s must be 32-byte lowercase hexadecimal data", name)
	}
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 32 {
		return hash, fmt.Errorf("%s must be 32-byte lowercase hexadecimal data", name)
	}
	copy(hash[:], raw)
	return hash, nil
}

func swapContract(record meta.Swap) types.QdayAtomicSwap {
	return types.QdayAtomicSwap{
		Recipient: record.Recipient, Refund: record.Refund, SecretHash: record.SecretHash, RefundHeight: record.RefundHeight,
	}
}

func (s *Service) CreateSwapKeys(swapID string) (SwapKeyView, bool, error) {
	if !validRequestID(swapID) {
		return SwapKeyView{}, false, errors.New("swapID must contain 1..128 letters, digits, '.', '_', ':' or '-'")
	}
	s.op.Lock()
	defer s.op.Unlock()
	if existing, ok, err := s.records.SwapKey(swapID); err != nil {
		return SwapKeyView{}, false, err
	} else if ok {
		return SwapKeyView{SwapID: existing.SwapID, Keys: swapKeysView(existing.Public), CreatedAt: existing.CreatedAt}, false, nil
	}
	master, err := s.masterSeed()
	if err != nil {
		return SwapKeyView{}, false, err
	}
	defer clear(master[:])
	keys, err := derive.SwapKeys(master, swapID)
	if err != nil {
		return SwapKeyView{}, false, err
	}
	defer func() {
		clear(keys.Classical)
		keys = types.QdayPrivateKeys{}
	}()
	record := meta.SwapKey{SwapID: swapID, Public: keys.Public, CreatedAt: time.Now().UTC()}
	if err := s.records.AddSwapKey(record); err != nil {
		return SwapKeyView{}, false, err
	}
	return SwapKeyView{SwapID: record.SwapID, Keys: swapKeysView(record.Public), CreatedAt: record.CreatedAt}, true, nil
}

func sameSwap(a, b meta.Swap) bool {
	return a.SwapID == b.SwapID && a.Role == b.Role && a.Recipient == b.Recipient && a.Refund == b.Refund && a.SecretHash == b.SecretHash && a.RefundHeight == b.RefundHeight && a.Address == b.Address
}

func (s *Service) RegisterSwap(request RegisterSwapRequest) (SwapView, bool, error) {
	if !validRequestID(request.SwapID) {
		return SwapView{}, false, errors.New("swapID must contain 1..128 letters, digits, '.', '_', ':' or '-'")
	} else if request.Role != "recipient" && request.Role != "refund" {
		return SwapView{}, false, errors.New("role must be 'recipient' or 'refund'")
	}
	counterparty, err := parseSwapKeys("counterparty", request.Counterparty)
	if err != nil {
		return SwapView{}, false, err
	}
	secretHash, err := parseHash("secretHash", request.SecretHash)
	if err != nil {
		return SwapView{}, false, err
	}
	s.op.Lock()
	defer s.op.Unlock()
	local, ok, err := s.records.SwapKey(request.SwapID)
	if err != nil {
		return SwapView{}, false, err
	} else if !ok {
		return SwapView{}, false, errors.New("create the swap session keys before registering the contract")
	}
	record := meta.Swap{
		SwapID: request.SwapID, Role: request.Role, SecretHash: secretHash,
		RefundHeight: request.RefundHeight, CreatedAt: time.Now().UTC(),
	}
	if request.Role == "recipient" {
		record.Recipient, record.Refund = local.Public, counterparty
	} else {
		record.Recipient, record.Refund = counterparty, local.Public
	}
	contract := swapContract(record)
	address, err := contract.Address()
	if err != nil {
		return SwapView{}, false, err
	}
	record.Address = address
	if existing, found, err := s.records.Swap(request.SwapID); err != nil {
		return SwapView{}, false, err
	} else if found {
		if !sameSwap(existing, record) {
			return SwapView{}, false, errors.New("swapID is already bound to a different contract")
		}
		if err := s.registerSwap(existing); err != nil {
			return SwapView{}, false, err
		}
		view, err := s.swapView(existing)
		return view, false, err
	}
	if err := s.records.AddSwap(record); err != nil {
		return SwapView{}, false, err
	} else if err := s.registerSwap(record); err != nil {
		return SwapView{}, false, err
	}
	view, err := s.swapView(record)
	return view, true, err
}

func (s *Service) registerSwap(record meta.Swap) error {
	policy, err := swapContract(record).Policy()
	if err != nil {
		return err
	}
	return s.node.WM.AddAddresses(s.node.SwapWalletID, wallet.Address{Address: policy.Address(), SpendPolicy: &policy, Description: record.SwapID})
}

func defaultFee(value string) (types.Currency, error) {
	if value == "" {
		return types.HastingsPerSiacoin.Div64(1000), nil
	}
	return parsePositiveCurrency(value, "feeAtomic")
}

func (s *Service) buildCustodyTransfer(ctx context.Context, cs consensus.State, master [32]byte, destination types.QdayAddress, value, fee types.Currency) (types.V2Transaction, error) {
	required, overflow := value.AddWithOverflow(fee)
	if overflow {
		return types.V2Transaction{}, errors.New("amount plus fee overflows")
	}
	outputs, err := s.spendableOutputs(cs)
	if err != nil {
		return types.V2Transaction{}, err
	}
	sort.Slice(outputs, func(i, j int) bool {
		return cs.QdayValue(outputs[i].SiacoinElement, cs.Index.Height+2).Cmp(cs.QdayValue(outputs[j].SiacoinElement, cs.Index.Height+2)) > 0
	})
	txn := types.V2Transaction{MinerFee: fee}
	owners := make([]meta.Address, 0, maxInputs)
	var total types.Currency
	for _, output := range outputs {
		owner, ok, err := s.records.AddressByHash(output.SiacoinOutput.Address)
		if err != nil {
			return types.V2Transaction{}, err
		} else if !ok {
			return types.V2Transaction{}, errors.New("indexed output belongs to an unknown key")
		}
		txn.SiacoinInputs = append(txn.SiacoinInputs, types.V2SiacoinInput{Parent: output.SiacoinElement, SatisfiedPolicy: types.SatisfiedPolicy{Policy: owner.Public.Policy()}})
		owners = append(owners, owner)
		total = total.Add(cs.QdayValue(output.SiacoinElement, cs.Index.Height+2))
		if total.Cmp(required) >= 0 || len(txn.SiacoinInputs) == maxInputs {
			break
		}
	}
	if total.Cmp(required) < 0 {
		return types.V2Transaction{}, errors.New("insufficient confirmed spendable balance within the 128-input limit")
	}
	txn.SiacoinOutputs = append(txn.SiacoinOutputs, types.SiacoinOutput{Value: value, Address: types.Address(destination)})
	if change := total.Sub(required); !change.IsZero() {
		changeAddress, err := s.newAddress("", "change")
		if err != nil {
			return types.V2Transaction{}, err
		}
		txn.SiacoinOutputs = append(txn.SiacoinOutputs, types.SiacoinOutput{Value: change, Address: types.Address(changeAddress.Address)})
	}
	txn.ArbitraryData = (consensus.QdayEnvelope{Kind: consensus.QdayTransfer}).Encode()
	if err := performDefendWork(ctx, cs, &txn); err != nil {
		return types.V2Transaction{}, err
	} else if err := signInputs(ctx, cs, &txn, owners, master); err != nil {
		return types.V2Transaction{}, err
	} else if s.node.CM.Tip() != cs.Index {
		return types.V2Transaction{}, errors.New("chain changed while signing; retry")
	} else if err := consensus.ValidateV2Transaction(consensus.NewMidState(cs), txn); err != nil {
		return types.V2Transaction{}, fmt.Errorf("constructed custody transfer is invalid: %w", err)
	}
	return txn, nil
}

func (s *Service) submitSwapAction(action *meta.SwapAction) {
	if _, err := s.node.CM.AddV2PoolTransactions(action.Basis, []types.V2Transaction{action.Transaction}); err != nil {
		action.LastError = err.Error()
	} else if err := s.broadcast(action.Basis, []types.V2Transaction{action.Transaction}); err != nil && !errors.Is(err, syncer.ErrNoPeers) {
		action.LastError = err.Error()
	}
	_ = s.records.UpdateSwapAction(action.ActionID, action.Basis, action.Transaction, action.LastError)
	s.wakeRebroadcast()
}

func (s *Service) FundSwap(ctx context.Context, swapID string, request FundSwapRequest) (SwapActionView, bool, error) {
	value, err := parsePositiveCurrency(request.AmountAtomic, "amountAtomic")
	if err != nil {
		return SwapActionView{}, false, err
	}
	fee, err := defaultFee(request.FeeAtomic)
	if err != nil {
		return SwapActionView{}, false, err
	}
	s.op.Lock()
	defer s.op.Unlock()
	record, ok, err := s.records.Swap(swapID)
	if err != nil {
		return SwapActionView{}, false, err
	} else if !ok {
		return SwapActionView{}, false, errors.New("atomic swap not found")
	}
	if existing, found, err := s.records.SwapAction(swapID, "fund", ""); err != nil {
		return SwapActionView{}, false, err
	} else if found {
		if existing.Amount != value || existing.Fee != fee || existing.Destination != record.Address {
			return SwapActionView{}, false, errors.New("this swap is already bound to different funding parameters")
		}
		view, err := s.swapActionView(existing)
		return view, false, err
	}
	master, err := s.masterSeed()
	if err != nil {
		return SwapActionView{}, false, err
	}
	defer clear(master[:])
	if !s.node.NetworkSynced() {
		return SwapActionView{}, false, errors.New("QDAY node has no synchronized peer")
	}
	cs := s.node.CM.TipState()
	scan, err := s.node.WM.Tip()
	if err != nil {
		return SwapActionView{}, false, err
	} else if scan != cs.Index {
		return SwapActionView{}, false, errors.New("wallet index is synchronizing")
	} else if !cs.QdayV1Active(cs.Index.Height + 1) {
		return SwapActionView{}, false, fmt.Errorf("atomic swaps activate at block %d", cs.Network.Qday.V1Height)
	} else if cs.Index.Height+1 >= record.RefundHeight {
		return SwapActionView{}, false, errors.New("refusing to fund a contract whose refund branch is already active at the next block")
	}
	unit := cs.QdayUnits(cs.Index.Height)
	if request.ExpectedUnitAtomic == "" || request.ExpectedUnitAtomic != unit.ExactString() {
		return SwapActionView{}, false, errors.New("expectedUnitAtomic does not match the active QDAY denomination")
	}
	txn, err := s.buildCustodyTransfer(ctx, cs, master, record.Address, value, fee)
	if err != nil {
		return SwapActionView{}, false, err
	}
	action := meta.SwapAction{
		ActionID: swapID + ":fund", SwapID: swapID, Kind: "fund", Transaction: txn, Basis: cs.Index,
		Destination: record.Address, Amount: value, Fee: fee, CreatedAt: time.Now().UTC(),
	}
	if err := s.records.AddSwapAction(action); err != nil {
		return SwapActionView{}, false, err
	}
	s.submitSwapAction(&action)
	view, err := s.swapActionView(action)
	return view, true, err
}

func (s *Service) findSwapOutput(record meta.Swap, outputID types.SiacoinOutputID, cs consensus.State) (wallet.UnspentSiacoinElement, error) {
	for _, pending := range s.node.CM.V2PoolTransactions() {
		for _, input := range pending.SiacoinInputs {
			if input.Parent.ID == outputID {
				return wallet.UnspentSiacoinElement{}, errors.New("atomic-swap output is already spent by a mempool transaction")
			}
		}
	}
	for offset := 0; ; offset += pageSize {
		page, basis, err := s.node.WM.AddressSiacoinOutputs(types.Address(record.Address), false, offset, pageSize)
		if err != nil {
			return wallet.UnspentSiacoinElement{}, err
		} else if basis != cs.Index {
			return wallet.UnspentSiacoinElement{}, errors.New("wallet index is synchronizing")
		}
		for _, output := range page {
			if output.ID == outputID {
				return output, nil
			}
		}
		if len(page) < pageSize {
			break
		}
	}
	return wallet.UnspentSiacoinElement{}, errors.New("atomic-swap output is not confirmed and unspent")
}

func storedClaimSecret(txn types.V2Transaction) string {
	if len(txn.SiacoinInputs) != 1 || len(txn.SiacoinInputs[0].SatisfiedPolicy.Preimages) == 0 {
		return ""
	}
	preimages := txn.SiacoinInputs[0].SatisfiedPolicy.Preimages
	return hex.EncodeToString(preimages[len(preimages)-1][:])
}

func (s *Service) spendSwap(ctx context.Context, swapID, kind string, request SpendSwapRequest) (SwapActionView, bool, error) {
	var outputID types.SiacoinOutputID
	if err := outputID.UnmarshalText([]byte(strings.TrimSpace(request.OutputID))); err != nil {
		return SwapActionView{}, false, errors.New("outputID must be a 32-byte hexadecimal siacoin output ID")
	}
	fee, err := defaultFee(request.FeeAtomic)
	if err != nil {
		return SwapActionView{}, false, err
	}
	secret := ""
	var secretBytes [32]byte
	if kind == "claim" {
		parsed, err := parseHash("secret", request.Secret)
		if err != nil {
			return SwapActionView{}, false, err
		}
		copy(secretBytes[:], parsed[:])
		secret = request.Secret
	} else if request.Secret != "" {
		return SwapActionView{}, false, errors.New("refund request must not contain a secret")
	}
	s.op.Lock()
	defer s.op.Unlock()
	record, ok, err := s.records.Swap(swapID)
	if err != nil {
		return SwapActionView{}, false, err
	} else if !ok {
		return SwapActionView{}, false, errors.New("atomic swap not found")
	} else if kind == "claim" && record.Role != "recipient" {
		return SwapActionView{}, false, errors.New("this daemon is not the swap recipient")
	} else if kind == "refund" && record.Role != "refund" {
		return SwapActionView{}, false, errors.New("this daemon is not the swap refund owner")
	}
	if existing, found, err := s.records.SwapAction(swapID, kind, outputID.String()); err != nil {
		return SwapActionView{}, false, err
	} else if found {
		if existing.Fee != fee || kind == "claim" && storedClaimSecret(existing.Transaction) != secret {
			return SwapActionView{}, false, errors.New("this output action is already bound to different parameters")
		}
		view, err := s.swapActionView(existing)
		return view, false, err
	}
	master, err := s.masterSeed()
	if err != nil {
		return SwapActionView{}, false, err
	}
	defer clear(master[:])
	keys, err := derive.SwapKeys(master, swapID)
	if err != nil {
		return SwapActionView{}, false, err
	}
	defer func() {
		clear(keys.Classical)
		keys = types.QdayPrivateKeys{}
	}()
	local, _, err := s.records.SwapKey(swapID)
	if err != nil {
		return SwapActionView{}, false, err
	} else if keys.Public != local.Public {
		return SwapActionView{}, false, errors.New("derived swap key does not match the persistent session")
	}
	if !s.node.NetworkSynced() {
		return SwapActionView{}, false, errors.New("QDAY node has no synchronized peer")
	}
	cs := s.node.CM.TipState()
	scan, err := s.node.WM.Tip()
	if err != nil {
		return SwapActionView{}, false, err
	} else if scan != cs.Index {
		return SwapActionView{}, false, errors.New("wallet index is synchronizing")
	} else if !cs.QdayV1Active(cs.Index.Height + 1) {
		return SwapActionView{}, false, fmt.Errorf("atomic swaps activate at block %d", cs.Network.Qday.V1Height)
	} else if kind == "refund" && cs.Index.Height < record.RefundHeight {
		return SwapActionView{}, false, fmt.Errorf("refund is locked until height %d", record.RefundHeight)
	}
	parent, err := s.findSwapOutput(record, outputID, cs)
	if err != nil {
		return SwapActionView{}, false, err
	} else if parent.MaturityHeight > cs.Index.Height+1 {
		return SwapActionView{}, false, errors.New("atomic-swap output is not mature")
	}
	value := cs.QdayValue(parent.SiacoinElement, cs.Index.Height+1)
	if value.Cmp(fee) <= 0 {
		return SwapActionView{}, false, errors.New("atomic-swap output does not cover the fee")
	}
	destination, err := s.newAddress("", "change")
	if err != nil {
		return SwapActionView{}, false, err
	}
	txn := types.V2Transaction{
		SiacoinInputs:  []types.V2SiacoinInput{{Parent: parent.SiacoinElement}},
		SiacoinOutputs: []types.SiacoinOutput{{Value: value.Sub(fee), Address: types.Address(destination.Address)}},
		MinerFee:       fee,
		ArbitraryData:  (consensus.QdayEnvelope{Kind: consensus.QdayTransfer}).Encode(),
	}
	contract := swapContract(record)
	if kind == "claim" {
		if err := mining.SignAtomicSwapClaim(ctx, cs, &txn, contract, &keys, secretBytes); err != nil {
			return SwapActionView{}, false, err
		}
	} else if err := mining.SignAtomicSwapRefund(ctx, cs, &txn, contract, &keys); err != nil {
		return SwapActionView{}, false, err
	}
	if s.node.CM.Tip() != cs.Index {
		return SwapActionView{}, false, errors.New("chain changed while signing; retry")
	} else if err := consensus.ValidateV2Transaction(consensus.NewMidState(cs), txn); err != nil {
		return SwapActionView{}, false, fmt.Errorf("constructed atomic-swap %s is invalid: %w", kind, err)
	}
	action := meta.SwapAction{
		ActionID: swapID + ":" + kind + ":" + outputID.String(), SwapID: swapID, Kind: kind, OutputID: outputID.String(),
		Transaction: txn, Basis: cs.Index, Destination: destination.Address, Amount: value.Sub(fee), Fee: fee, CreatedAt: time.Now().UTC(),
	}
	if err := s.records.AddSwapAction(action); err != nil {
		return SwapActionView{}, false, err
	}
	s.submitSwapAction(&action)
	view, err := s.swapActionView(action)
	return view, true, err
}

func (s *Service) ClaimSwap(ctx context.Context, swapID string, request SpendSwapRequest) (SwapActionView, bool, error) {
	return s.spendSwap(ctx, swapID, "claim", request)
}

func (s *Service) RefundSwap(ctx context.Context, swapID string, request SpendSwapRequest) (SwapActionView, bool, error) {
	return s.spendSwap(ctx, swapID, "refund", request)
}

func (s *Service) swapActionView(action meta.SwapAction) (SwapActionView, error) {
	cs := s.node.CM.TipState()
	view := SwapActionView{
		ActionID: action.ActionID, Kind: action.Kind, OutputID: action.OutputID, TransactionID: action.Transaction.ID().String(),
		Destination: action.Destination.String(), Amount: amount(action.Amount, cs.QdayUnits(cs.Index.Height)),
		Fee: amount(action.Fee, cs.QdayUnits(cs.Index.Height)), Basis: action.Basis.String(), Status: "queued",
		CreatedAt: action.CreatedAt, LastError: action.LastError,
	}
	events, err := s.node.WM.Events([]types.Hash256{types.Hash256(action.Transaction.ID())})
	if err != nil {
		return view, err
	}
	if len(events) > 0 {
		view.Status = "confirmed"
		view.Confirmations = events[0].Confirmations
		height := events[0].Index.Height
		view.BlockHeight = &height
		view.BlockID = events[0].Index.ID.String()
		view.LastError = ""
		return view, nil
	}
	for _, pending := range s.node.CM.V2PoolTransactions() {
		if pending.ID() == action.Transaction.ID() {
			view.Status = "mempool"
			view.LastError = ""
			return view, nil
		}
	}
	if action.LastError != "" {
		view.Status = "retrying"
	}
	return view, nil
}

type observedSwapOutput struct {
	view      SwapOutputView
	confirmed bool
	spent     bool
}

func classifySwapSpend(contract types.QdayAtomicSwap, input types.V2SiacoinInput) (kind, secret string) {
	fullPolicy, err := contract.Policy()
	if err != nil || input.SatisfiedPolicy.Policy.Address() != fullPolicy.Address() || !types.IsQdaySwapSpendPolicy(input.SatisfiedPolicy.Policy) {
		return "unknown", ""
	}
	outer, ok := input.SatisfiedPolicy.Policy.Type.(types.PolicyTypeThreshold)
	if !ok || outer.N != 1 || len(outer.Of) != 2 {
		return "unknown", ""
	}
	var branch types.SpendPolicy
	for _, candidate := range outer.Of {
		if _, opaque := candidate.Type.(types.PolicyTypeOpaque); !opaque {
			branch = candidate
		}
	}
	inner, ok := branch.Type.(types.PolicyTypeThreshold)
	if !ok || inner.N != 3 || len(inner.Of) != 3 {
		return "unknown", ""
	}
	switch lock := inner.Of[2].Type.(type) {
	case types.PolicyTypeHash:
		if types.Hash256(lock) != contract.SecretHash {
			return "unknown", ""
		}
		if len(input.SatisfiedPolicy.Preimages) == 0 {
			return "claim", ""
		}
		preimage := input.SatisfiedPolicy.Preimages[len(input.SatisfiedPolicy.Preimages)-1]
		if types.Hash256(sha256.Sum256(preimage[:])) == contract.SecretHash {
			return "claim", hex.EncodeToString(preimage[:])
		}
		return "claim", ""
	case types.PolicyTypeAbove:
		if uint64(lock) != contract.RefundHeight {
			return "unknown", ""
		}
		return "refund", ""
	default:
		return "unknown", ""
	}
}

func observeSwapTransaction(outputs map[string]*observedSwapOutput, record meta.Swap, event wallet.Event, confirmed bool) {
	data, ok := event.Data.(wallet.EventV2Transaction)
	if !ok {
		return
	}
	txn := types.V2Transaction(data)
	txid := txn.ID()
	for i, output := range txn.SiacoinOutputs {
		if output.Address != types.Address(record.Address) {
			continue
		}
		id := txn.SiacoinOutputID(txid, i).String()
		observed, exists := outputs[id]
		if !exists || confirmed && !observed.confirmed {
			status := "funding"
			var height *uint64
			if confirmed {
				status = "funded"
				h := event.Index.Height
				height = &h
			}
			outputs[id] = &observedSwapOutput{view: SwapOutputView{
				ID: id, Value: Amount{Atomic: output.Value.ExactString()}, FundingTransaction: txid.String(), FundingHeight: height,
				MaturityHeight: event.MaturityHeight, Confirmations: event.Confirmations, Status: status,
			}, confirmed: confirmed}
		}
	}
	contract := swapContract(record)
	for _, input := range txn.SiacoinInputs {
		if input.Parent.SiacoinOutput.Address != types.Address(record.Address) {
			continue
		}
		id := input.Parent.ID.String()
		observed, exists := outputs[id]
		if !exists {
			observed = &observedSwapOutput{view: SwapOutputView{ID: id, Value: Amount{Atomic: input.Parent.SiacoinOutput.Value.ExactString()}}}
			outputs[id] = observed
		}
		if observed.spent && !confirmed {
			continue
		}
		kind, secret := classifySwapSpend(contract, input)
		observed.spent = true
		if kind == "unknown" {
			observed.view.Status = "spending"
		} else {
			observed.view.Status = kind + "ing"
		}
		observed.view.SpendTransaction = txid.String()
		observed.view.RevealedSecret = secret
		if confirmed {
			if kind == "unknown" {
				observed.view.Status = "spent"
			} else {
				observed.view.Status = kind + "ed"
			}
			h := event.Index.Height
			observed.view.SpendHeight = &h
		}
	}
}

func (s *Service) observedSwapOutputs(record meta.Swap) ([]SwapOutputView, error) {
	cs := s.node.CM.TipState()
	scan, err := s.node.WM.Tip()
	if err != nil {
		return nil, err
	} else if scan != cs.Index {
		return nil, errors.New("wallet index is synchronizing")
	}
	observed := make(map[string]*observedSwapOutput)
	const maxEvents = 10_000
	var confirmedEvents []wallet.Event
	for offset := 0; offset < maxEvents; offset += pageSize {
		events, err := s.node.WM.AddressEvents(types.Address(record.Address), offset, pageSize)
		if err != nil {
			return nil, err
		}
		confirmedEvents = append(confirmedEvents, events...)
		if len(events) < pageSize {
			break
		} else if offset+pageSize == maxEvents {
			return nil, errors.New("atomic-swap event history exceeds the safety limit")
		}
	}
	// AddressEvents is newest first. Replaying oldest first preserves a spend
	// when its older funding event is processed later in this snapshot.
	for i := len(confirmedEvents) - 1; i >= 0; i-- {
		observeSwapTransaction(observed, record, confirmedEvents[i], true)
	}
	if indexed, err := s.node.WM.Tip(); err != nil {
		return nil, err
	} else if indexed != cs.Index || s.node.CM.Tip() != cs.Index {
		return nil, errors.New("chain changed while reading atomic-swap events; retry")
	}
	unconfirmed, err := s.node.WM.AddressUnconfirmedEvents(types.Address(record.Address))
	if err != nil {
		return nil, err
	}
	for _, event := range unconfirmed {
		observeSwapTransaction(observed, record, event, false)
	}
	unit := cs.QdayUnits(cs.Index.Height)
	result := make([]SwapOutputView, 0, len(observed))
	for _, output := range observed {
		value, err := types.ParseCurrency(output.view.Value.Atomic)
		if err != nil {
			return nil, err
		}
		output.view.Value = amount(value, unit)
		if !output.spent && output.confirmed {
			spendable := amount(cs.QdayValue(types.SiacoinElement{
				SiacoinOutput: types.SiacoinOutput{Value: value}, MaturityHeight: output.view.MaturityHeight,
			}, cs.Index.Height+1), unit)
			output.view.Spendable = &spendable
		}
		result = append(result, output.view)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].FundingHeight == nil {
			return true
		} else if result[j].FundingHeight == nil {
			return false
		} else if *result[i].FundingHeight != *result[j].FundingHeight {
			return *result[i].FundingHeight > *result[j].FundingHeight
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func aggregateSwapStatus(outputs []SwapOutputView, actions []SwapActionView) string {
	status := "waiting"
	for _, action := range actions {
		if action.Kind == "fund" && action.Status != "confirmed" {
			status = "funding"
		}
	}
	for _, output := range outputs {
		switch output.Status {
		case "claimed", "refunded":
			return output.Status
		case "claiming", "refunding":
			status = output.Status
		case "funded":
			if status == "waiting" || status == "funding" {
				status = "funded"
			}
		case "funding":
			if status == "waiting" {
				status = "funding"
			}
		}
	}
	return status
}

func (s *Service) swapView(record meta.Swap) (SwapView, error) {
	local, ok, err := s.records.SwapKey(record.SwapID)
	if err != nil {
		return SwapView{}, err
	} else if !ok {
		return SwapView{}, errors.New("atomic-swap session key is missing")
	}
	actions, err := s.records.SwapActions(record.SwapID)
	if err != nil {
		return SwapView{}, err
	}
	actionViews := make([]SwapActionView, len(actions))
	for i := range actions {
		actionViews[i], err = s.swapActionView(actions[i])
		if err != nil {
			return SwapView{}, err
		}
	}
	outputs, err := s.observedSwapOutputs(record)
	if err != nil {
		return SwapView{}, err
	}
	cs := s.node.CM.TipState()
	return SwapView{
		SwapID: record.SwapID, Role: record.Role, Status: aggregateSwapStatus(outputs, actionViews),
		Local: swapKeysView(local.Public), Recipient: swapKeysView(record.Recipient), Refund: swapKeysView(record.Refund),
		SecretHash: hex.EncodeToString(record.SecretHash[:]), RefundHeight: record.RefundHeight,
		Address: record.Address.String(), Height: cs.Index.Height, Outputs: outputs, Actions: actionViews, CreatedAt: record.CreatedAt,
	}, nil
}

func (s *Service) Swap(swapID string) (SwapView, bool, error) {
	record, ok, err := s.records.Swap(swapID)
	if err != nil || !ok {
		return SwapView{}, ok, err
	}
	view, err := s.swapView(record)
	return view, true, err
}

func (s *Service) Swaps(limit, offset int) ([]SwapView, error) {
	records, err := s.records.Swaps(limit, offset)
	if err != nil {
		return nil, err
	}
	result := make([]SwapView, len(records))
	for i := range records {
		result[i], err = s.swapView(records[i])
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Service) rebroadcastSwapActions(tip types.ChainIndex) {
	actions, err := s.records.SwapActionRebroadcastCandidates(tip.Height, reorgMonitoringDepth)
	if err != nil {
		s.log.Error("load atomic-swap actions for rebroadcast", zap.Error(err))
		return
	}
	for _, action := range actions {
		events, eventErr := s.node.WM.Events([]types.Hash256{types.Hash256(action.Transaction.ID())})
		if eventErr != nil {
			s.log.Warn("check atomic-swap action confirmation", zap.String("actionID", action.ActionID), zap.Error(eventErr))
			continue
		} else if len(events) > 0 {
			confirmed := events[0].Index
			if action.Confirmed == nil || *action.Confirmed != confirmed {
				if err := s.records.SetSwapActionConfirmation(action.ActionID, &confirmed); err != nil {
					s.log.Error("save atomic-swap action confirmation", zap.String("actionID", action.ActionID), zap.Error(err))
				}
			}
			continue
		}
		if action.Confirmed != nil {
			if best, ok := s.node.CM.BestIndex(action.Confirmed.Height); ok && best == *action.Confirmed {
				continue
			}
			if err := s.records.SetSwapActionConfirmation(action.ActionID, nil); err != nil {
				s.log.Error("clear orphaned atomic-swap action confirmation", zap.String("actionID", action.ActionID), zap.Error(err))
				continue
			}
		}
		txns, err := s.updateProofs([]types.V2Transaction{action.Transaction.DeepCopy()}, action.Basis, tip)
		if err == nil && len(txns) == 1 {
			_, err = s.node.CM.AddV2PoolTransactions(tip, txns)
		}
		if err == nil {
			err = s.broadcast(tip, txns)
			if errors.Is(err, syncer.ErrNoPeers) {
				err = nil
			}
		}
		lastError := ""
		if err != nil {
			lastError = err.Error()
		}
		if len(txns) == 1 {
			action.Transaction = txns[0]
			action.Basis = tip
		}
		if updateErr := s.records.UpdateSwapAction(action.ActionID, action.Basis, action.Transaction, lastError); updateErr != nil {
			s.log.Error("save atomic-swap rebroadcast state", zap.String("actionID", action.ActionID), zap.Error(updateErr))
		}
	}
}
