package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/petoshi/qday-walletd/internal/derive"
	"github.com/petoshi/qday-walletd/internal/meta"
	"go.sia.tech/core/consensus"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/chain"
	mining "go.sia.tech/coreutils/qday"
	"go.sia.tech/walletd/v2/persist/sqlite"
	"go.sia.tech/walletd/v2/qday"
	"go.sia.tech/walletd/v2/wallet"
	"go.uber.org/zap"
)

func waitIndexed(t *testing.T, node *Node) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		index, err := node.WM.Tip()
		if err != nil {
			t.Fatal(err)
		} else if index == node.CM.Tip() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("wallet index did not reach the chain tip")
}

func mineBlock(t *testing.T, node *Node, miner types.QdayKeys) types.Block {
	t.Helper()
	cs := node.CM.TipState()
	b := mining.Candidate(cs, miner, node.CM.V2PoolTransactions(), cs.PrevTimestamps[0].Add(2*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	b, err = mining.Mine(ctx, cs, b, 2, nil)
	if err != nil {
		t.Fatal(err)
	} else if err := node.CM.AddBlocks([]types.Block{b}); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, node)
	return b
}

func newServiceTest(t *testing.T) (*Service, *Node, *meta.Store, [32]byte) {
	return newServiceTestAtV1Height(t, chain.QdayV1ActivationHeight)
}

func newServiceTestAtV1Height(t *testing.T, height uint64) (*Service, *Node, *meta.Store, [32]byte) {
	t.Helper()
	dir := t.TempDir()
	manifest := chain.QdayDevnet()
	manifest.Network.Qday.V1Height = height
	db, tip, err := chain.NewDBStore(chain.NewMemDB(), &manifest.Network, manifest.Genesis, nil)
	if err != nil {
		t.Fatal(err)
	}
	cm := chain.NewManager(db, tip)
	index, err := sqlite.OpenDatabase(filepath.Join(dir, "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	wm, err := wallet.NewManager(cm, index, wallet.WithIndexMode(wallet.IndexModeFull))
	if err != nil {
		t.Fatal(err)
	}
	w, err := wm.AddWallet(wallet.Wallet{Name: "QDAY Custody"})
	if err != nil {
		t.Fatal(err)
	}
	swapIndex, err := wm.AddWallet(wallet.Wallet{Name: "QDAY Atomic Swaps"})
	if err != nil {
		t.Fatal(err)
	}
	records, err := meta.Open(filepath.Join(dir, "walletd.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	node := &Node{Manifest: manifest, CM: cm, WM: wm, WalletID: w.ID, SwapWalletID: swapIndex.ID}
	master := [32]byte{7, 7, 7}
	masterKeys, err := types.QdayKeysFromSeed(master)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "master.key")
	if err := qday.WriteKey(keyPath, "test-walletd-password", master, masterKeys.Public); err != nil {
		t.Fatal(err)
	}
	service := NewService(context.Background(), node, records, keyPath, zap.NewNop())
	if err := service.Unlock("test-walletd-password"); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, node)
	t.Cleanup(func() {
		service.Close()
		_ = wm.Close()
		_ = index.Close()
		_ = records.Close()
	})
	return service, node, records, master
}

func fundAddressFromPremine(t *testing.T, node *Node, destination types.QdayAddress, value types.Currency) types.V2Transaction {
	t.Helper()
	premineKeys, err := types.QdayKeysFromSeed([32]byte{0x51, 0x44, 0x41, 0x59})
	if err != nil {
		t.Fatal(err)
	}
	outputs, basis, err := node.WM.AddressSiacoinOutputs(types.Address(node.Manifest.Premine), false, 0, 10)
	if err != nil || len(outputs) != 1 {
		t.Fatalf("premine output: %d, %v", len(outputs), err)
	} else if basis != node.CM.Tip() {
		t.Fatal("wallet index is behind before funding")
	}
	fee := types.HastingsPerSiacoin.Div64(1000)
	required, overflow := value.AddWithOverflow(fee)
	if overflow || outputs[0].SiacoinOutput.Value.Cmp(required) <= 0 {
		t.Fatal("premine fixture cannot fund test")
	}
	txn := types.V2Transaction{
		SiacoinInputs: []types.V2SiacoinInput{{Parent: outputs[0].SiacoinElement}},
		SiacoinOutputs: []types.SiacoinOutput{
			{Value: value, Address: types.Address(destination)},
			{Value: outputs[0].SiacoinOutput.Value.Sub(required), Address: types.Address(node.Manifest.Premine)},
		},
		MinerFee: fee, ArbitraryData: (consensus.QdayEnvelope{Kind: consensus.QdayTransfer}).Encode(),
	}
	if err := mining.SignTransfer(context.Background(), node.CM.TipState(), &txn, &premineKeys); err != nil {
		t.Fatal(err)
	} else if _, err := node.CM.AddV2PoolTransactions(node.CM.Tip(), []types.V2Transaction{txn}); err != nil {
		t.Fatal(err)
	}
	mineBlock(t, node, premineKeys.Public)
	return txn
}

func TestWithdrawalsAcrossV1Activation(t *testing.T) {
	service, node, records, master := newServiceTestAtV1Height(t, 3)
	deposit, err := service.NewDepositAddress("activation-funds")
	if err != nil {
		t.Fatal(err)
	}
	premineSeed := [32]byte{0x51, 0x44, 0x41, 0x59}
	premineKeys, err := types.QdayKeysFromSeed(premineSeed)
	if err != nil {
		t.Fatal(err)
	}
	outputs, _, err := node.WM.AddressSiacoinOutputs(types.Address(node.Manifest.Premine), false, 0, 10)
	if err != nil || len(outputs) != 1 {
		t.Fatalf("premine output: %d, %v", len(outputs), err)
	}
	fee := types.HastingsPerSiacoin.Div64(1000)
	destination, err := types.ParseQdayAddress(deposit.Address)
	if err != nil {
		t.Fatal(err)
	}
	funding := types.V2Transaction{
		SiacoinInputs: []types.V2SiacoinInput{{Parent: outputs[0].SiacoinElement}},
		SiacoinOutputs: []types.SiacoinOutput{
			{Value: types.Siacoins(20), Address: types.Address(destination)},
			{Value: outputs[0].SiacoinOutput.Value.Sub(types.Siacoins(20)).Sub(fee), Address: types.Address(node.Manifest.Premine)},
		},
		MinerFee: fee, ArbitraryData: (consensus.QdayEnvelope{Kind: consensus.QdayTransfer}).Encode(),
	}
	if err := mining.SignTransfer(context.Background(), node.CM.TipState(), &funding, &premineKeys); err != nil {
		t.Fatal(err)
	} else if _, err := node.CM.AddV2PoolTransactions(node.CM.Tip(), []types.V2Transaction{funding}); err != nil {
		t.Fatal(err)
	}
	mineBlock(t, node, premineKeys.Public)
	mineBlock(t, node, premineKeys.Public)
	if node.CM.Tip().Height != 2 {
		t.Fatalf("tip height = %d", node.CM.Tip().Height)
	}

	recipientKeys, err := types.QdayKeysFromSeed([32]byte{9, 1, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	request := WithdrawalRequest{
		RequestID: "activation-withdrawal", Destination: recipientKeys.Public.Address().String(),
		AmountAtomic: types.Siacoins(3).ExactString(), ExpectedUnitAtomic: node.CM.TipState().QdayUnits(node.CM.Tip().Height).ExactString(),
	}
	created, fresh, err := service.CreateWithdrawal(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	} else if !fresh || created.Status != "mempool" {
		t.Fatalf("unexpected pre-activation withdrawal: %#v, fresh=%v", created, fresh)
	}
	activation := mineBlock(t, node, premineKeys.Public)
	if activation.V2 == nil {
		t.Fatal("activation block has no v2 data")
	} else if len(activation.V2.Transactions) != 3 {
		t.Fatalf("activation block has %d transactions", len(activation.V2.Transactions))
	}
	marker, err := consensus.ParseQdayEnvelope(activation.V2.Transactions[len(activation.V2.Transactions)-1].ArbitraryData)
	if err != nil || marker.Kind != consensus.QdayMiningWork {
		t.Fatalf("activation block has no compact work marker: %v", err)
	}
	service.rebroadcast()
	stored, ok, err := records.Withdrawal(request.RequestID)
	if err != nil || !ok || stored.Confirmed == nil || stored.Confirmed.Height != 3 {
		t.Fatalf("activation withdrawal was not confirmed: %#v, %v", stored, err)
	}

	request = WithdrawalRequest{
		RequestID: "post-activation-withdrawal", Destination: recipientKeys.Public.Address().String(),
		AmountAtomic: types.Siacoins(2).ExactString(), ExpectedUnitAtomic: node.CM.TipState().QdayUnits(node.CM.Tip().Height).ExactString(),
	}
	created, fresh, err = service.CreateWithdrawal(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	} else if !fresh || created.Status != "mempool" {
		t.Fatalf("unexpected post-activation withdrawal: %#v, fresh=%v", created, fresh)
	}
	mineBlock(t, node, premineKeys.Public)
	service.rebroadcast()
	stored, ok, err = records.Withdrawal(request.RequestID)
	if err != nil || !ok || stored.Confirmed == nil || stored.Confirmed.Height != 4 {
		t.Fatalf("post-activation withdrawal was not confirmed: %#v, %v", stored, err)
	}
	clear(master[:])
}

func TestMultiAddressWithdrawalAndIdempotency(t *testing.T) {
	service, node, records, master := newServiceTest(t)
	deposit, err := service.NewDepositAddress("customer-1")
	if err != nil {
		t.Fatal(err)
	}
	depositKeys, err := derive.Keys(master, 0)
	if err != nil {
		t.Fatal(err)
	}
	mineBlock(t, node, depositKeys.Public)
	deposits, _, err := service.Deposits(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundPayout := false
	for _, event := range deposits {
		if event.Type == wallet.EventTypeMinerPayout && event.Address == deposit.Address && event.Creditable {
			foundPayout = true
		}
	}
	if !foundPayout {
		t.Fatalf("miner payout to a deposit address was not reported: %#v", deposits)
	}
	to, err := types.ParseQdayAddress(deposit.Address)
	if err != nil {
		t.Fatal(err)
	}
	premineSeed := [32]byte{0x51, 0x44, 0x41, 0x59}
	premineKeys, err := types.QdayKeysFromSeed(premineSeed)
	if err != nil {
		t.Fatal(err)
	}
	if premineKeys.Public.Address() != node.Manifest.Premine {
		t.Fatal("devnet premine fixture changed")
	}
	outputs, _, err := node.WM.AddressSiacoinOutputs(types.Address(node.Manifest.Premine), false, 0, 10)
	if err != nil || len(outputs) != 1 {
		t.Fatalf("premine output: %d, %v", len(outputs), err)
	}
	fee := types.HastingsPerSiacoin.Div64(1000)
	txn := types.V2Transaction{
		SiacoinInputs: []types.V2SiacoinInput{{Parent: outputs[0].SiacoinElement}},
		SiacoinOutputs: []types.SiacoinOutput{
			{Value: types.Siacoins(10), Address: types.Address(to)},
			{Value: outputs[0].SiacoinOutput.Value.Sub(types.Siacoins(10)).Sub(fee), Address: types.Address(node.Manifest.Premine)},
		},
		MinerFee: fee, ArbitraryData: (consensus.QdayEnvelope{Kind: consensus.QdayTransfer}).Encode(),
	}
	if err := mining.SignTransfer(context.Background(), node.CM.TipState(), &txn, &premineKeys); err != nil {
		t.Fatal(err)
	} else if _, err := node.CM.AddV2PoolTransactions(node.CM.Tip(), []types.V2Transaction{txn}); err != nil {
		t.Fatal(err)
	}
	mineBlock(t, node, premineKeys.Public)
	deposits, _, err = service.Deposits(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundTransfer := false
	for _, event := range deposits {
		if event.TransactionID == txn.ID().String() && event.OutputIndex != nil && *event.OutputIndex == 0 && event.Creditable {
			foundTransfer = true
		}
	}
	if !foundTransfer {
		t.Fatalf("transaction deposit was not reported: %#v", deposits)
	}
	balance, err := service.Balance()
	if err != nil {
		t.Fatal(err)
	} else if balance.Spendable.Atomic != types.Siacoins(10).ExactString() {
		t.Fatalf("deposit balance = %s", balance.Spendable.Atomic)
	}

	recipient, err := service.NewDepositAddress("customer-2")
	if err != nil {
		t.Fatal(err)
	}
	request := WithdrawalRequest{
		RequestID: "exchange-order-17", Destination: recipient.Address,
		AmountAtomic: types.Siacoins(3).ExactString(), ExpectedUnitAtomic: node.CM.TipState().QdayUnits(node.CM.Tip().Height).ExactString(),
	}
	created, fresh, err := service.CreateWithdrawal(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	} else if !fresh || created.Status != "mempool" {
		t.Fatalf("unexpected withdrawal result: %#v, fresh=%v", created, fresh)
	}
	pool := node.CM.V2PoolTransactions()
	if len(pool) != 1 {
		t.Fatalf("mempool contains %d transactions", len(pool))
	}
	if err := consensus.ValidateV2Transaction(consensus.NewMidState(node.CM.TipState()), pool[0]); err != nil {
		t.Fatal(err)
	}
	replayed, fresh, err := service.CreateWithdrawal(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	} else if fresh || replayed.TransactionID != created.TransactionID || len(node.CM.V2PoolTransactions()) != 1 {
		t.Fatal("idempotent retry created another withdrawal")
	}
	request.AmountAtomic = types.Siacoins(4).ExactString()
	if _, _, err := service.CreateWithdrawal(context.Background(), request); err == nil {
		t.Fatal("requestID was reused for a different amount")
	}
	mineBlock(t, node, premineKeys.Public)
	service.rebroadcast()
	stored, ok, err := records.Withdrawal(request.RequestID)
	if err != nil || !ok {
		t.Fatal(err)
	} else if stored.Confirmed == nil || stored.Confirmed.Height != node.CM.Tip().Height {
		t.Fatalf("withdrawal confirmation was not persisted: %#v", stored.Confirmed)
	}
	deposits, _, err = service.Deposits(20, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundInternal := false
	for _, event := range deposits {
		if event.TransactionID == created.TransactionID && event.Address == recipient.Address {
			foundInternal = true
			if event.Creditable {
				t.Fatal("wallet-funded transfer was reported as a creditable deposit")
			}
		}
	}
	if !foundInternal {
		t.Fatalf("wallet-funded transfer was not reported: %#v", deposits)
	}
}

func TestAtomicSwapFundingClaimAndSecretDetection(t *testing.T) {
	service, node, records, _ := newServiceTestAtV1Height(t, 1)
	deposit, err := service.NewDepositAddress("swap-funding")
	if err != nil {
		t.Fatal(err)
	}
	depositAddress, err := types.ParseQdayAddress(deposit.Address)
	if err != nil {
		t.Fatal(err)
	}
	fundAddressFromPremine(t, node, depositAddress, types.Siacoins(20))

	const swapID = "basic-swap-order-17"
	keyView, created, err := service.CreateSwapKeys(swapID)
	if err != nil || !created {
		t.Fatalf("create swap keys: created=%v, err=%v", created, err)
	}
	replayedKeys, created, err := service.CreateSwapKeys(swapID)
	if err != nil || created || replayedKeys.Keys != keyView.Keys {
		t.Fatalf("swap keys are not idempotent: %#v, created=%v, err=%v", replayedKeys, created, err)
	}
	refundKeys, err := types.QdayKeysFromSeed([32]byte{8, 8, 8})
	if err != nil {
		t.Fatal(err)
	}
	secret := [32]byte{4, 2, 4, 2}
	secretHash := sha256.Sum256(secret[:])
	registration := RegisterSwapRequest{
		SwapID: swapID, Role: "recipient", Counterparty: swapKeysView(refundKeys.Public),
		SecretHash: hex.EncodeToString(secretHash[:]), RefundHeight: node.CM.Tip().Height + 10,
	}
	registered, fresh, err := service.RegisterSwap(registration)
	if err != nil || !fresh {
		t.Fatalf("register swap: fresh=%v, err=%v", fresh, err)
	} else if registered.Local != keyView.Keys || registered.Address == "" || registered.Status != "waiting" {
		t.Fatalf("unexpected registered swap: %#v", registered)
	}
	replayed, fresh, err := service.RegisterSwap(registration)
	if err != nil || fresh || replayed.Address != registered.Address {
		t.Fatalf("swap registration is not idempotent: %#v, fresh=%v, err=%v", replayed, fresh, err)
	}
	keyPath := service.keyPath
	service.Close()
	service = NewService(context.Background(), node, records, keyPath, zap.NewNop())
	if err := service.Unlock("test-walletd-password"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if restored, ok, err := service.Swap(swapID); err != nil || !ok || restored.Address != registered.Address {
		t.Fatalf("swap journal did not survive service restart: %#v, %v", restored, err)
	}

	unit := node.CM.TipState().QdayUnits(node.CM.Tip().Height)
	funding, fresh, err := service.FundSwap(context.Background(), swapID, FundSwapRequest{
		AmountAtomic: types.Siacoins(5).ExactString(), ExpectedUnitAtomic: unit.ExactString(),
	})
	if err != nil || !fresh || funding.Status != "mempool" {
		t.Fatalf("fund swap: %#v, fresh=%v, err=%v", funding, fresh, err)
	}
	premineKeys, err := types.QdayKeysFromSeed([32]byte{0x51, 0x44, 0x41, 0x59})
	if err != nil {
		t.Fatal(err)
	}
	mineBlock(t, node, premineKeys.Public)
	view, ok, err := service.Swap(swapID)
	if err != nil || !ok {
		t.Fatal(err)
	} else if view.Status != "funded" || len(view.Outputs) != 1 || view.Outputs[0].Status != "funded" {
		t.Fatalf("funded swap was not observed: %#v", view)
	}
	balance, err := service.Balance()
	wantCustody := types.Siacoins(15).Sub(types.HastingsPerSiacoin.Div64(1000)).ExactString()
	if err != nil {
		t.Fatal(err)
	} else if balance.Spendable.Atomic != wantCustody {
		t.Fatalf("contract output leaked into custody balance: got %s, want %s", balance.Spendable.Atomic, wantCustody)
	}
	fundedTip := node.CM.Tip()
	outputID := view.Outputs[0].ID
	claim, fresh, err := service.ClaimSwap(context.Background(), swapID, SpendSwapRequest{
		OutputID: outputID, Secret: hex.EncodeToString(secret[:]),
	})
	if err != nil || !fresh || claim.Status != "mempool" {
		t.Fatalf("claim swap: %#v, fresh=%v, err=%v", claim, fresh, err)
	}
	view, _, err = service.Swap(swapID)
	if err != nil {
		t.Fatal(err)
	} else if view.Status != "claiming" || len(view.Outputs) != 1 || view.Outputs[0].RevealedSecret != hex.EncodeToString(secret[:]) {
		t.Fatalf("mempool claim secret was not detected: %#v", view)
	}
	mineBlock(t, node, premineKeys.Public)
	service.rebroadcast()
	view, _, err = service.Swap(swapID)
	if err != nil {
		t.Fatal(err)
	} else if view.Status != "claimed" || view.Outputs[0].Status != "claimed" || view.Outputs[0].RevealedSecret != hex.EncodeToString(secret[:]) {
		t.Fatalf("confirmed claim was not detected: %#v", view)
	}
	replayedClaim, fresh, err := service.ClaimSwap(context.Background(), swapID, SpendSwapRequest{
		OutputID: outputID, Secret: hex.EncodeToString(secret[:]),
	})
	if err != nil || fresh || replayedClaim.TransactionID != claim.TransactionID {
		t.Fatalf("claim is not idempotent: %#v, fresh=%v, err=%v", replayedClaim, fresh, err)
	}
	stored, ok, err := records.SwapAction(swapID, "claim", outputID)
	if err != nil || !ok || stored.Confirmed == nil {
		t.Fatalf("claim journal was not confirmed: %#v, %v", stored, err)
	}

	// Replace the claim block with a longer branch. The selected-chain view
	// must demote the claim to mempool, clear its persisted confirmation and
	// keep the revealed secret available from the rebroadcast transaction.
	sideState, ok := node.CM.State(fundedTip.ID)
	if !ok {
		t.Fatal("missing funded-tip state")
	}
	for range 2 {
		block := mining.Candidate(sideState, premineKeys.Public, nil, sideState.PrevTimestamps[0].Add(2*time.Second))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		block, err = mining.Mine(ctx, sideState, block, 2, nil)
		cancel()
		if err != nil {
			t.Fatal(err)
		} else if err := node.CM.AddBlocks([]types.Block{block}); err != nil {
			t.Fatal(err)
		}
		// Manager stores header state for a non-selected branch. Build the full
		// accumulator state locally so the next block commits to side-chain
		// outputs instead of the selected chain's accumulator.
		sideState, _ = consensus.ApplyBlock(sideState, block, consensus.V1BlockSupplement{}, time.Time{})
	}
	waitIndexed(t, node)
	service.rebroadcast()
	stored, _, err = records.SwapAction(swapID, "claim", outputID)
	if err != nil || stored.Confirmed != nil {
		t.Fatalf("orphaned claim confirmation was not cleared: %#v, %v", stored.Confirmed, err)
	}
	view, _, err = service.Swap(swapID)
	if err != nil {
		t.Fatal(err)
	} else if view.Status != "claiming" || view.Outputs[0].SpendHeight != nil || view.Outputs[0].RevealedSecret != hex.EncodeToString(secret[:]) {
		t.Fatalf("orphaned claim was not restored to mempool: %#v", view)
	}
	mineBlock(t, node, premineKeys.Public)
	service.rebroadcast()
	view, _, err = service.Swap(swapID)
	if err != nil || view.Status != "claimed" {
		t.Fatalf("rebroadcast claim did not reconfirm: %#v, %v", view, err)
	}
}

func TestAtomicSwapRefundLifecycle(t *testing.T) {
	service, node, _, _ := newServiceTestAtV1Height(t, 1)
	const swapID = "basic-swap-order-refund"
	keyView, _, err := service.CreateSwapKeys(swapID)
	if err != nil {
		t.Fatal(err)
	}
	recipientKeys, err := types.QdayKeysFromSeed([32]byte{6, 6, 6})
	if err != nil {
		t.Fatal(err)
	}
	secret := [32]byte{7, 7, 7}
	secretHash := sha256.Sum256(secret[:])
	view, fresh, err := service.RegisterSwap(RegisterSwapRequest{
		SwapID: swapID, Role: "refund", Counterparty: swapKeysView(recipientKeys.Public),
		SecretHash: hex.EncodeToString(secretHash[:]), RefundHeight: 3,
	})
	if err != nil || !fresh || view.Local != keyView.Keys {
		t.Fatalf("register refund swap: %#v, fresh=%v, err=%v", view, fresh, err)
	}
	address, err := types.ParseQdayAddress(view.Address)
	if err != nil {
		t.Fatal(err)
	}
	fundAddressFromPremine(t, node, address, types.Siacoins(4))
	view, _, err = service.Swap(swapID)
	if err != nil || len(view.Outputs) != 1 {
		t.Fatalf("observe refund funding: %#v, %v", view, err)
	}
	request := SpendSwapRequest{OutputID: view.Outputs[0].ID}
	if _, _, err := service.RefundSwap(context.Background(), swapID, request); err == nil || !strings.Contains(err.Error(), "locked until height 3") {
		t.Fatalf("early refund was accepted: %v", err)
	}
	premineKeys, err := types.QdayKeysFromSeed([32]byte{0x51, 0x44, 0x41, 0x59})
	if err != nil {
		t.Fatal(err)
	}
	mineBlock(t, node, premineKeys.Public)
	if _, _, err := service.RefundSwap(context.Background(), swapID, request); err == nil || !strings.Contains(err.Error(), "locked until height 3") {
		t.Fatalf("refund at height 2 was accepted: %v", err)
	}
	mineBlock(t, node, premineKeys.Public)
	refund, fresh, err := service.RefundSwap(context.Background(), swapID, request)
	if err != nil || !fresh || refund.Status != "mempool" {
		t.Fatalf("refund swap: %#v, fresh=%v, err=%v", refund, fresh, err)
	}
	mineBlock(t, node, premineKeys.Public)
	view, _, err = service.Swap(swapID)
	if err != nil || view.Status != "refunded" || view.Outputs[0].RevealedSecret != "" {
		t.Fatalf("confirmed refund was not detected: %#v, %v", view, err)
	}
}
