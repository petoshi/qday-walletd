package daemon

import (
	"context"
	"path/filepath"
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

func mineBlock(t *testing.T, node *Node, miner types.QdayKeys) {
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
}

func newServiceTest(t *testing.T) (*Service, *Node, *meta.Store, [32]byte) {
	t.Helper()
	dir := t.TempDir()
	manifest := chain.QdayDevnet()
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
	records, err := meta.Open(filepath.Join(dir, "walletd.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	node := &Node{Manifest: manifest, CM: cm, WM: wm, WalletID: w.ID}
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
