package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/petoshi/qday-walletd/internal/derive"
	"github.com/petoshi/qday-walletd/internal/meta"
	"go.sia.tech/core/consensus"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/walletd/v2/qday"
	"go.sia.tech/walletd/v2/wallet"
	"go.uber.org/zap"
)

const (
	maxInputs            = 128
	pageSize             = 500
	reorgMonitoringDepth = 144
)

type Amount struct {
	Atomic string `json:"atomic"`
	QDAY   string `json:"qday"`
}

type AddressView struct {
	Index     uint64    `json:"index"`
	Address   string    `json:"address"`
	Reference string    `json:"reference,omitempty"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"createdAt"`
}

type Balance struct {
	Height      uint64 `json:"height"`
	Synced      bool   `json:"synced"`
	UnitAtomic  string `json:"unitAtomic"`
	Spendable   Amount `json:"spendable"`
	Immature    Amount `json:"immature"`
	PendingIn   Amount `json:"pendingIn"`
	OutputCount int    `json:"outputCount"`
	ShieldUntil uint64 `json:"shieldUntil,omitempty"`
}

type Deposit struct {
	ID             string    `json:"id"`
	Type           string    `json:"type"`
	EventID        string    `json:"eventID"`
	TransactionID  string    `json:"transactionID,omitempty"`
	OutputIndex    *int      `json:"outputIndex,omitempty"`
	Address        string    `json:"address"`
	Reference      string    `json:"reference"`
	Amount         Amount    `json:"amount"`
	BlockHeight    uint64    `json:"blockHeight"`
	BlockID        string    `json:"blockID"`
	Confirmations  uint64    `json:"confirmations"`
	MaturityHeight uint64    `json:"maturityHeight"`
	Timestamp      time.Time `json:"timestamp"`
	Creditable     bool      `json:"creditable"`
}

type WithdrawalRequest struct {
	RequestID          string `json:"requestID"`
	Destination        string `json:"destination"`
	AmountAtomic       string `json:"amountAtomic"`
	FeeAtomic          string `json:"feeAtomic,omitempty"`
	ExpectedUnitAtomic string `json:"expectedUnitAtomic"`
}

type WithdrawalView struct {
	RequestID     string    `json:"requestID"`
	Kind          string    `json:"kind"`
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

type Service struct {
	ctx     context.Context
	cancel  context.CancelFunc
	node    *Node
	records *meta.Store
	keyPath string
	log     *zap.Logger

	op     sync.Mutex
	mu     sync.RWMutex
	master *[32]byte

	rebroadcastWake chan struct{}
	rebroadcastDone chan struct{}
	cancelReorg     func()
}

func NewService(ctx context.Context, node *Node, records *meta.Store, keyPath string, log *zap.Logger) *Service {
	ctx, cancel := context.WithCancel(ctx)
	s := &Service{
		ctx: ctx, cancel: cancel, node: node, records: records, keyPath: keyPath, log: log,
		rebroadcastWake: make(chan struct{}, 1), rebroadcastDone: make(chan struct{}),
	}
	s.cancelReorg = node.CM.OnReorg(func(types.ChainIndex) { s.wakeRebroadcast() })
	go func() {
		defer close(s.rebroadcastDone)
		s.rebroadcastLoop()
	}()
	s.wakeRebroadcast()
	return s
}

func (s *Service) Close() {
	s.cancel()
	if s.cancelReorg != nil {
		s.cancelReorg()
	}
	<-s.rebroadcastDone
	s.Lock()
}

func (s *Service) Unlocked() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.master != nil
}

func (s *Service) Unlock(password string) error {
	seed, err := qday.ReadKey(s.keyPath, password)
	if err != nil {
		return err
	}
	s.op.Lock()
	s.mu.Lock()
	if s.master != nil {
		clear(s.master[:])
	}
	s.master = &seed
	s.mu.Unlock()
	s.op.Unlock()
	s.wakeRebroadcast()
	return nil
}

func (s *Service) Lock() {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.master != nil {
		clear(s.master[:])
		s.master = nil
	}
}

func (s *Service) masterSeed() ([32]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.master == nil {
		return [32]byte{}, errors.New("wallet is locked")
	}
	return *s.master, nil
}

func validReference(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func viewAddress(a meta.Address) AddressView {
	return AddressView{Index: a.Index, Address: a.Address.String(), Reference: a.Reference, Kind: a.Kind, CreatedAt: a.CreatedAt}
}

func (s *Service) Addresses(limit, offset int) ([]AddressView, uint64, error) {
	addresses, err := s.records.AddressesPage(limit, offset)
	if err != nil {
		return nil, 0, err
	}
	count, err := s.records.AddressCount()
	if err != nil {
		return nil, 0, err
	}
	result := make([]AddressView, len(addresses))
	for i := range addresses {
		result[i] = viewAddress(addresses[i])
	}
	return result, count, nil
}

func (s *Service) NewDepositAddress(reference string) (AddressView, error) {
	if !validReference(reference) {
		return AddressView{}, errors.New("reference must contain 1..128 printable ASCII characters")
	}
	s.op.Lock()
	defer s.op.Unlock()
	if existing, ok, err := s.records.AddressByReference(reference); err != nil {
		return AddressView{}, err
	} else if ok {
		if err := s.registerAddress(existing); err != nil {
			return AddressView{}, err
		}
		return viewAddress(existing), nil
	}
	a, err := s.newAddress(reference, "deposit")
	return viewAddress(a), err
}

func (s *Service) newAddress(reference, kind string) (meta.Address, error) {
	master, err := s.masterSeed()
	if err != nil {
		return meta.Address{}, err
	}
	defer clear(master[:])
	index, err := s.records.NextIndex()
	if err != nil {
		return meta.Address{}, err
	}
	keys, err := derive.Keys(master, index)
	if err != nil {
		return meta.Address{}, err
	}
	defer func() {
		clear(keys.Classical)
		keys = types.QdayPrivateKeys{}
	}()
	a := meta.Address{Index: index, Address: keys.Public.Address(), Public: keys.Public, Reference: reference, Kind: kind, CreatedAt: time.Now().UTC()}
	if err := s.records.AddAddress(a); err != nil {
		return meta.Address{}, err
	}
	if err := s.registerAddress(a); err != nil {
		return meta.Address{}, err
	}
	return a, nil
}

func (s *Service) registerAddress(a meta.Address) error {
	policy := a.Public.Policy()
	return s.node.WM.AddAddresses(s.node.WalletID, wallet.Address{Address: policy.Address(), SpendPolicy: &policy, Description: a.Reference})
}

func amount(value, unit types.Currency) Amount {
	return Amount{Atomic: value.ExactString(), QDAY: qday.FormatAmount(value, unit)}
}

func (s *Service) spendableOutputs(cs consensus.State) ([]wallet.UnspentSiacoinElement, error) {
	spent := make(map[types.SiacoinOutputID]bool)
	for _, txn := range s.node.CM.V2PoolTransactions() {
		for _, input := range txn.SiacoinInputs {
			spent[input.Parent.ID] = true
		}
	}
	var result []wallet.UnspentSiacoinElement
	for offset := 0; ; offset += pageSize {
		page, basis, err := s.node.WM.UnspentSiacoinOutputs(s.node.WalletID, offset, pageSize)
		if err != nil {
			return nil, err
		} else if basis != cs.Index {
			return nil, errors.New("wallet index is synchronizing")
		}
		for _, output := range page {
			if !spent[output.ID] && !cs.QdayValue(output.SiacoinElement, cs.Index.Height+2).IsZero() {
				result = append(result, output)
			}
		}
		if len(page) < pageSize {
			break
		}
	}
	if s.node.CM.Tip() != cs.Index {
		return nil, errors.New("chain changed while reading wallet outputs")
	}
	return result, nil
}

func (s *Service) Balance() (Balance, error) {
	cs := s.node.CM.TipState()
	index, err := s.node.WM.Tip()
	if err != nil {
		return Balance{}, err
	}
	unit := cs.QdayUnits(cs.Index.Height)
	result := Balance{Height: cs.Index.Height, Synced: index == cs.Index && s.node.NetworkSynced(), UnitAtomic: unit.ExactString()}
	if index != cs.Index {
		return result, nil
	}
	outputs, err := s.spendableOutputs(cs)
	if err != nil {
		return result, err
	}
	var spendable types.Currency
	for _, output := range outputs {
		spendable = spendable.Add(cs.QdayValue(output.SiacoinElement, cs.Index.Height))
		if cs.QdayHeight != 0 {
			expiry := max(output.MaturityHeight, cs.QdayHeight) + cs.Network.Qday.ShieldBlocks
			if result.ShieldUntil == 0 || expiry < result.ShieldUntil {
				result.ShieldUntil = expiry
			}
		}
	}
	confirmed, err := s.node.WM.WalletBalance(s.node.WalletID)
	if err != nil {
		return result, err
	}
	pool := s.node.CM.V2PoolTransactions()
	poolSpent := make(map[types.SiacoinOutputID]bool)
	for _, txn := range pool {
		for _, input := range txn.SiacoinInputs {
			poolSpent[input.Parent.ID] = true
		}
	}
	var pending types.Currency
	for _, txn := range pool {
		id := txn.ID()
		for i, output := range txn.SiacoinOutputs {
			_, managed, err := s.records.AddressByHash(output.Address)
			if err != nil {
				return result, err
			}
			if managed && !poolSpent[txn.SiacoinOutputID(id, i)] {
				pending = pending.Add(output.Value)
			}
		}
	}
	result.Spendable = amount(spendable, unit)
	result.Immature = amount(confirmed.ImmatureSiacoins, unit)
	result.PendingIn = amount(pending, unit)
	result.OutputCount = len(outputs)
	return result, nil
}

func (s *Service) Deposits(limit, eventOffset int) ([]Deposit, int, error) {
	if limit < 1 || limit > 200 || eventOffset < 0 {
		return nil, eventOffset, errors.New("limit must be 1..200 and offset must be nonnegative")
	}
	cs := s.node.CM.TipState()
	scan, err := s.node.WM.Tip()
	if err != nil {
		return nil, eventOffset, err
	} else if scan != cs.Index {
		return nil, eventOffset, errors.New("wallet index is synchronizing")
	}
	deposits := make([]Deposit, 0, limit)
	offset := eventOffset
	for len(deposits) < limit {
		events, err := s.node.WM.WalletEvents(s.node.WalletID, offset, min(pageSize, limit*2))
		if err != nil {
			return nil, offset, err
		}
		if len(events) == 0 {
			break
		}
		for _, event := range events {
			offset++
			unit := cs.QdayUnits(event.Index.Height)
			switch data := event.Data.(type) {
			case wallet.EventV2Transaction:
				transaction := types.V2Transaction(data)
				id := transaction.ID()
				internal := false
				for _, input := range data.SiacoinInputs {
					if _, found, err := s.records.AddressByHash(input.Parent.SiacoinOutput.Address); err != nil {
						return nil, offset, err
					} else if found {
						internal = true
						break
					}
				}
				for i, output := range data.SiacoinOutputs {
					record, found, err := s.records.AddressByHash(output.Address)
					if err != nil {
						return nil, offset, err
					} else if !found || record.Kind != "deposit" {
						continue
					}
					outputIndex := i
					deposits = append(deposits, Deposit{
						ID: fmt.Sprintf("%s:%d", id, i), Type: event.Type, EventID: event.ID.String(),
						TransactionID: id.String(), OutputIndex: &outputIndex, Address: record.Address.String(),
						Reference: record.Reference, Amount: amount(output.Value, unit),
						BlockHeight: event.Index.Height, BlockID: event.Index.ID.String(), Confirmations: event.Confirmations,
						MaturityHeight: event.MaturityHeight, Timestamp: event.Timestamp, Creditable: !internal,
					})
				}
			case wallet.EventPayout:
				output := data.SiacoinElement.SiacoinOutput
				record, found, err := s.records.AddressByHash(output.Address)
				if err != nil {
					return nil, offset, err
				} else if !found || record.Kind != "deposit" {
					continue
				}
				deposits = append(deposits, Deposit{
					ID: event.ID.String(), Type: event.Type, EventID: event.ID.String(),
					Address: record.Address.String(), Reference: record.Reference, Amount: amount(output.Value, unit),
					BlockHeight: event.Index.Height, BlockID: event.Index.ID.String(), Confirmations: event.Confirmations,
					MaturityHeight: event.MaturityHeight, Timestamp: event.Timestamp, Creditable: true,
				})
			}
			if len(deposits) >= limit {
				break
			}
		}
		if len(events) < min(pageSize, limit*2) {
			break
		}
	}
	endScan, err := s.node.WM.Tip()
	if err != nil {
		return nil, eventOffset, err
	} else if endScan != scan || s.node.CM.Tip() != cs.Index {
		return nil, eventOffset, errors.New("chain changed while reading deposits; retry")
	}
	return deposits, offset, nil
}

func validRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("._:-", r) {
			return false
		}
	}
	return true
}

func parsePositiveCurrency(value, name string) (types.Currency, error) {
	if value == "" || len(value) > 80 {
		return types.ZeroCurrency, fmt.Errorf("%s is required", name)
	}
	c, err := types.ParseCurrency(value)
	if err != nil || c.IsZero() {
		return types.ZeroCurrency, fmt.Errorf("%s must be a positive atomic integer", name)
	}
	return c, nil
}

func (s *Service) CreateWithdrawal(ctx context.Context, request WithdrawalRequest) (WithdrawalView, bool, error) {
	if !validRequestID(request.RequestID) {
		return WithdrawalView{}, false, errors.New("requestID must contain 1..128 letters, digits, '.', '_', ':' or '-'")
	}
	destination, err := types.ParseQdayAddress(request.Destination)
	if err != nil {
		return WithdrawalView{}, false, fmt.Errorf("invalid destination: %w", err)
	}
	value, err := parsePositiveCurrency(request.AmountAtomic, "amountAtomic")
	if err != nil {
		return WithdrawalView{}, false, err
	}
	fee := types.HastingsPerSiacoin.Div64(1000)
	if request.FeeAtomic != "" {
		fee, err = parsePositiveCurrency(request.FeeAtomic, "feeAtomic")
		if err != nil {
			return WithdrawalView{}, false, err
		}
	}
	s.op.Lock()
	defer s.op.Unlock()
	if existing, ok, err := s.records.Withdrawal(request.RequestID); err != nil {
		return WithdrawalView{}, false, err
	} else if ok {
		if existing.Destination != destination || existing.Amount != value || existing.Fee != fee {
			return WithdrawalView{}, false, errors.New("requestID is already bound to a different withdrawal")
		}
		view, err := s.withdrawalView(existing)
		return view, false, err
	}
	master, err := s.masterSeed()
	if err != nil {
		return WithdrawalView{}, false, err
	}
	defer clear(master[:])
	if !s.node.NetworkSynced() {
		return WithdrawalView{}, false, errors.New("QDAY node has no synchronized peer")
	}
	cs := s.node.CM.TipState()
	scan, err := s.node.WM.Tip()
	if err != nil {
		return WithdrawalView{}, false, err
	} else if scan != cs.Index {
		return WithdrawalView{}, false, errors.New("wallet index is synchronizing")
	}
	unit := cs.QdayUnits(cs.Index.Height)
	if request.ExpectedUnitAtomic == "" || request.ExpectedUnitAtomic != unit.ExactString() {
		return WithdrawalView{}, false, errors.New("expectedUnitAtomic does not match the active QDAY denomination")
	}
	required, overflow := value.AddWithOverflow(fee)
	if overflow {
		return WithdrawalView{}, false, errors.New("amount plus fee overflows")
	}
	outputs, err := s.spendableOutputs(cs)
	if err != nil {
		return WithdrawalView{}, false, err
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
			return WithdrawalView{}, false, err
		} else if !ok {
			return WithdrawalView{}, false, errors.New("indexed output belongs to an unknown key")
		}
		txn.SiacoinInputs = append(txn.SiacoinInputs, types.V2SiacoinInput{Parent: output.SiacoinElement, SatisfiedPolicy: types.SatisfiedPolicy{Policy: owner.Public.Policy()}})
		owners = append(owners, owner)
		total = total.Add(cs.QdayValue(output.SiacoinElement, cs.Index.Height+2))
		if total.Cmp(required) >= 0 || len(txn.SiacoinInputs) == maxInputs {
			break
		}
	}
	if total.Cmp(required) < 0 {
		return WithdrawalView{}, false, errors.New("insufficient confirmed spendable balance within the 128-input limit")
	}
	txn.SiacoinOutputs = append(txn.SiacoinOutputs, types.SiacoinOutput{Value: value, Address: types.Address(destination)})
	if change := total.Sub(required); !change.IsZero() {
		changeAddress, err := s.newAddress("", "change")
		if err != nil {
			return WithdrawalView{}, false, err
		}
		txn.SiacoinOutputs = append(txn.SiacoinOutputs, types.SiacoinOutput{Value: change, Address: types.Address(changeAddress.Address)})
	}
	txn.ArbitraryData = (consensus.QdayEnvelope{Kind: consensus.QdayTransfer}).Encode()
	if err := performDefendWork(ctx, cs, &txn); err != nil {
		return WithdrawalView{}, false, err
	}
	if err := signInputs(ctx, cs, &txn, owners, master); err != nil {
		return WithdrawalView{}, false, err
	}
	if s.node.CM.Tip() != cs.Index {
		return WithdrawalView{}, false, errors.New("chain changed while signing; retry with the same requestID")
	}
	if err := consensus.ValidateV2Transaction(consensus.NewMidState(cs), txn); err != nil {
		return WithdrawalView{}, false, fmt.Errorf("constructed withdrawal is invalid: %w", err)
	}
	w := meta.Withdrawal{RequestID: request.RequestID, Kind: "withdrawal", Transaction: txn, Basis: cs.Index, Destination: destination, Amount: value, Fee: fee, CreatedAt: time.Now().UTC()}
	if err := s.records.AddWithdrawal(w); err != nil {
		return WithdrawalView{}, false, err
	}
	if _, err := s.node.CM.AddV2PoolTransactions(w.Basis, []types.V2Transaction{w.Transaction}); err != nil {
		w.LastError = err.Error()
		_ = s.records.UpdateWithdrawal(w.RequestID, w.Basis, w.Transaction, w.LastError)
	} else if err := s.broadcast(w.Basis, []types.V2Transaction{w.Transaction}); err != nil && !errors.Is(err, syncer.ErrNoPeers) {
		w.LastError = err.Error()
		_ = s.records.UpdateWithdrawal(w.RequestID, w.Basis, w.Transaction, w.LastError)
	}
	s.wakeRebroadcast()
	view, err := s.withdrawalView(w)
	return view, true, err
}

func performDefendWork(ctx context.Context, cs consensus.State, txn *types.V2Transaction) error {
	if !cs.QdayActive(cs.Index.Height + 1) {
		return nil
	}
	envelope, err := consensus.ParseQdayEnvelope(txn.ArbitraryData)
	if err != nil {
		return err
	}
	intent := cs.QdayWorkIntent(*txn)
	for !consensus.QdayWorkValid(intent, envelope.Nonce, cs.Network.Qday.DefendBits) {
		envelope.Nonce++
		if envelope.Nonce%1024 == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
	}
	txn.ArbitraryData = envelope.Encode()
	return nil
}

func signInputs(ctx context.Context, cs consensus.State, txn *types.V2Transaction, owners []meta.Address, master [32]byte) error {
	if len(owners) != len(txn.SiacoinInputs) {
		return errors.New("input owner count mismatch")
	}
	hash := cs.InputSigHash(*txn)
	signed := make(map[uint64]types.SatisfiedPolicy)
	for i, owner := range owners {
		if err := ctx.Err(); err != nil {
			return err
		}
		policy, ok := signed[owner.Index]
		if !ok {
			keys, err := derive.Keys(master, owner.Index)
			if err != nil {
				return err
			} else if keys.Public != owner.Public {
				return errors.New("derived key does not match stored address")
			}
			policy, err = keys.Sign(hash)
			clear(keys.Classical)
			keys = types.QdayPrivateKeys{}
			if err != nil {
				return err
			}
			signed[owner.Index] = policy
		}
		txn.SiacoinInputs[i].SatisfiedPolicy = policy
	}
	return nil
}

func (s *Service) withdrawalView(w meta.Withdrawal) (WithdrawalView, error) {
	cs := s.node.CM.TipState()
	unit := cs.QdayUnits(cs.Index.Height)
	view := WithdrawalView{
		RequestID: w.RequestID, Kind: w.Kind, TransactionID: w.Transaction.ID().String(), Destination: w.Destination.String(),
		Amount: amount(w.Amount, unit), Fee: amount(w.Fee, unit), Basis: w.Basis.String(), Status: "queued",
		CreatedAt: w.CreatedAt, LastError: w.LastError,
	}
	events, err := s.node.WM.Events([]types.Hash256{types.Hash256(w.Transaction.ID())})
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
	if w.Confirmed != nil {
		if best, ok := s.node.CM.BestIndex(w.Confirmed.Height); ok && best == *w.Confirmed {
			view.Status = "confirmed"
			view.Confirmations = 1 + cs.Index.Height - w.Confirmed.Height
			height := w.Confirmed.Height
			view.BlockHeight = &height
			view.BlockID = w.Confirmed.ID.String()
			view.LastError = ""
			return view, nil
		}
	}
	for _, txn := range s.node.CM.V2PoolTransactions() {
		if txn.ID() == w.Transaction.ID() {
			view.Status = "mempool"
			view.LastError = ""
			return view, nil
		}
	}
	if w.LastError != "" {
		view.Status = "retrying"
	}
	return view, nil
}

func (s *Service) Withdrawal(requestID string) (WithdrawalView, bool, error) {
	w, ok, err := s.records.Withdrawal(requestID)
	if err != nil || !ok {
		return WithdrawalView{}, ok, err
	}
	view, err := s.withdrawalView(w)
	return view, true, err
}

func (s *Service) Withdrawals(limit, offset int) ([]WithdrawalView, error) {
	withdrawals, err := s.records.Withdrawals(limit, offset)
	if err != nil {
		return nil, err
	}
	result := make([]WithdrawalView, 0, len(withdrawals))
	for _, withdrawal := range withdrawals {
		view, err := s.withdrawalView(withdrawal)
		if err != nil {
			return nil, err
		}
		result = append(result, view)
	}
	return result, nil
}

func (s *Service) Status() (map[string]any, error) {
	cs := s.node.CM.TipState()
	scan, err := s.node.WM.Tip()
	if err != nil {
		return nil, err
	}
	connections, inbound := 0, 0
	if s.node.Syncer != nil {
		for _, peer := range s.node.Syncer.Peers() {
			if peer.Err() != nil {
				continue
			}
			connections++
			if peer.Inbound {
				inbound++
			}
		}
	}
	addressCount, err := s.records.AddressCount()
	if err != nil {
		return nil, err
	}
	unit := cs.QdayUnits(cs.Index.Height)
	return map[string]any{
		"network": cs.Network.Name, "genesis": s.node.Manifest.Genesis.ID(), "height": cs.Index.Height,
		"scanHeight": scan.Height, "synced": scan == cs.Index && s.node.NetworkSynced(), "networkSynced": s.node.NetworkSynced(),
		"connections": connections, "inboundConnections": inbound, "mempoolTransactions": len(s.node.CM.V2PoolTransactions()),
		"unlocked": s.Unlocked(), "addresses": addressCount, "unitAtomic": unit.ExactString(),
		"qdayHeight": cs.QdayHeight, "pqDay": cs.QdayActive(cs.Index.Height),
	}, nil
}

func (s *Service) wakeRebroadcast() {
	select {
	case s.rebroadcastWake <- struct{}{}:
	default:
	}
}

func (s *Service) rebroadcastLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		case <-s.rebroadcastWake:
		}
		s.rebroadcast()
		s.autoDefend()
	}
}

func (s *Service) updateProofs(txns []types.V2Transaction, from, to types.ChainIndex) ([]types.V2Transaction, error) {
	const step = uint64(72)
	for from != to {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		best, onBest := s.node.CM.BestIndex(from.Height)
		var next types.ChainIndex
		if onBest && best == from && from.Height < to.Height {
			var ok bool
			next, ok = s.node.CM.BestIndex(min(from.Height+step, to.Height))
			if !ok {
				return nil, errors.New("withdrawal is waiting for chain synchronization")
			}
		} else if onBest && best == from && from.Height == to.Height {
			next = to
		} else {
			next = from
			for range step {
				if next.Height == 0 {
					return nil, errors.New("withdrawal basis is outside this chain")
				}
				block, ok := s.node.CM.Block(next.ID)
				if !ok {
					return nil, errors.New("withdrawal basis is not available")
				}
				next = types.ChainIndex{Height: next.Height - 1, ID: block.ParentID}
				if best, ok := s.node.CM.BestIndex(next.Height); ok && best == next && next.Height <= to.Height {
					break
				}
			}
		}
		var err error
		txns, err = s.node.CM.UpdateV2TransactionSet(txns, from, next)
		if err != nil || len(txns) == 0 {
			return txns, err
		}
		from = next
	}
	return txns, nil
}

func (s *Service) rebroadcast() {
	tip := s.node.CM.Tip()
	withdrawals, err := s.records.RebroadcastCandidates(tip.Height, reorgMonitoringDepth)
	if err != nil {
		s.log.Error("load withdrawals for rebroadcast", zap.Error(err))
		return
	}
	for _, withdrawal := range withdrawals {
		events, eventErr := s.node.WM.Events([]types.Hash256{types.Hash256(withdrawal.Transaction.ID())})
		if eventErr != nil {
			s.log.Warn("check withdrawal confirmation", zap.String("requestID", withdrawal.RequestID), zap.Error(eventErr))
			continue
		} else if len(events) > 0 {
			confirmed := events[0].Index
			if withdrawal.Confirmed == nil || *withdrawal.Confirmed != confirmed {
				if err := s.records.SetWithdrawalConfirmation(withdrawal.RequestID, &confirmed); err != nil {
					s.log.Error("save withdrawal confirmation", zap.String("requestID", withdrawal.RequestID), zap.Error(err))
				}
			}
			continue
		}
		if withdrawal.Confirmed != nil {
			if best, ok := s.node.CM.BestIndex(withdrawal.Confirmed.Height); ok && best == *withdrawal.Confirmed {
				// The chain index still confirms this transaction. The wallet index
				// may be catching up, so do not resurrect a spent transaction.
				continue
			}
			if err := s.records.SetWithdrawalConfirmation(withdrawal.RequestID, nil); err != nil {
				s.log.Error("clear orphaned withdrawal confirmation", zap.String("requestID", withdrawal.RequestID), zap.Error(err))
				continue
			}
		}
		txns, err := s.updateProofs([]types.V2Transaction{withdrawal.Transaction.DeepCopy()}, withdrawal.Basis, tip)
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
			withdrawal.Transaction = txns[0]
			withdrawal.Basis = tip
		}
		if updateErr := s.records.UpdateWithdrawal(withdrawal.RequestID, withdrawal.Basis, withdrawal.Transaction, lastError); updateErr != nil {
			s.log.Error("save withdrawal rebroadcast state", zap.String("requestID", withdrawal.RequestID), zap.Error(updateErr))
		}
	}
}

func (s *Service) autoDefend() {
	if !s.Unlocked() || !s.node.NetworkSynced() || !s.op.TryLock() {
		return
	}
	defer s.op.Unlock()
	cs := s.node.CM.TipState()
	if !cs.QdayActive(cs.Index.Height + 1) {
		return
	}
	scan, err := s.node.WM.Tip()
	if err != nil || scan != cs.Index {
		return
	}
	master, err := s.masterSeed()
	if err != nil {
		return
	}
	defer clear(master[:])
	outputs, err := s.spendableOutputs(cs)
	if err != nil {
		s.log.Warn("read outputs for automatic DEFEND", zap.Error(err))
		return
	}
	threshold := max(uint64(2), cs.Network.Qday.ShieldBlocks/4)
	sort.Slice(outputs, func(i, j int) bool {
		left := max(outputs[i].MaturityHeight, cs.QdayHeight) + cs.Network.Qday.ShieldBlocks
		right := max(outputs[j].MaturityHeight, cs.QdayHeight) + cs.Network.Qday.ShieldBlocks
		return left < right
	})
	fee := types.HastingsPerSiacoin.Div64(1000)
	var selected []wallet.UnspentSiacoinElement
	var owners []meta.Address
	var total types.Currency
	for _, output := range outputs {
		expiry := max(output.MaturityHeight, cs.QdayHeight) + cs.Network.Qday.ShieldBlocks
		if cs.Index.Height+threshold < expiry {
			continue
		}
		owner, ok, err := s.records.AddressByHash(output.SiacoinOutput.Address)
		if err != nil {
			s.log.Warn("look up DEFEND output owner", zap.Error(err))
			return
		} else if !ok {
			s.log.Error("DEFEND output has no managed key", zap.String("output", output.ID.String()))
			return
		}
		selected = append(selected, output)
		owners = append(owners, owner)
		total = total.Add(cs.QdayValue(output.SiacoinElement, cs.Index.Height+2))
		if len(selected) == maxInputs {
			break
		}
	}
	if len(selected) == 0 {
		return
	} else if total.Cmp(fee) <= 0 {
		s.log.Error("outputs requiring DEFEND cannot pay the transaction fee", zap.Int("outputs", len(selected)))
		return
	}
	destination, err := s.newAddress("", "change")
	if err != nil {
		s.log.Warn("create automatic DEFEND destination", zap.Error(err))
		return
	}
	txn := types.V2Transaction{
		MinerFee:       fee,
		SiacoinOutputs: []types.SiacoinOutput{{Value: total.Sub(fee), Address: types.Address(destination.Address)}},
		ArbitraryData:  (consensus.QdayEnvelope{Kind: consensus.QdayTransfer}).Encode(),
	}
	for i, output := range selected {
		txn.SiacoinInputs = append(txn.SiacoinInputs, types.V2SiacoinInput{Parent: output.SiacoinElement, SatisfiedPolicy: types.SatisfiedPolicy{Policy: owners[i].Public.Policy()}})
	}
	if err := performDefendWork(s.ctx, cs, &txn); err != nil {
		return
	} else if err := signInputs(s.ctx, cs, &txn, owners, master); err != nil {
		s.log.Warn("sign automatic DEFEND transaction", zap.Error(err))
		return
	} else if s.node.CM.Tip() != cs.Index {
		return
	} else if err := consensus.ValidateV2Transaction(consensus.NewMidState(cs), txn); err != nil {
		s.log.Error("automatic DEFEND transaction is invalid", zap.Error(err))
		return
	}
	requestID := "defend:" + txn.ID().String()
	w := meta.Withdrawal{
		RequestID: requestID, Kind: "defend", Transaction: txn, Basis: cs.Index,
		Destination: destination.Address, Amount: total.Sub(fee), Fee: fee, CreatedAt: time.Now().UTC(),
	}
	if err := s.records.AddWithdrawal(w); err != nil {
		if _, exists, lookupErr := s.records.Withdrawal(requestID); lookupErr == nil && exists {
			return
		}
		s.log.Error("store automatic DEFEND transaction", zap.Error(err))
		return
	}
	if _, err := s.node.CM.AddV2PoolTransactions(w.Basis, []types.V2Transaction{txn}); err != nil {
		w.LastError = err.Error()
	} else if err := s.broadcast(w.Basis, []types.V2Transaction{txn}); err != nil && !errors.Is(err, syncer.ErrNoPeers) {
		w.LastError = err.Error()
	}
	_ = s.records.UpdateWithdrawal(w.RequestID, w.Basis, w.Transaction, w.LastError)
	s.log.Info("automatic DEFEND submitted", zap.String("transaction", txn.ID().String()), zap.Int("outputs", len(selected)))
}

func (s *Service) broadcast(basis types.ChainIndex, txns []types.V2Transaction) error {
	if s.node.Syncer == nil {
		return nil
	}
	return s.node.Syncer.BroadcastV2TransactionSet(basis, txns)
}
