//go:build integration

package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/petoshi/qday-walletd/internal/meta"
	"go.sia.tech/core/consensus"
	qtypes "go.sia.tech/core/types"
	mining "go.sia.tech/coreutils/qday"
	qdaynode "go.sia.tech/walletd/v2/qday"
	"go.sia.tech/walletd/v2/wallet"
	"go.uber.org/zap"
)

const (
	litecoinRPCUser     = "qday-swap-test"
	litecoinRPCPassword = "qday-swap-test-only-password"
	litecoinWallet      = "qday-swap-test"
	litoshisPerLitecoin = int64(100_000_000)
)

type litecoinRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *litecoinRPCError) Error() string {
	return fmt.Sprintf("litecoin RPC %d: %s", e.Code, e.Message)
}

type litecoinNode struct {
	t          *testing.T
	binary     string
	dataDir    string
	rpcURL     string
	logPath    string
	client     *http.Client
	cmd        *exec.Cmd
	logFile    *os.File
	walletMade bool
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func newLitecoinNode(t *testing.T) *litecoinNode {
	t.Helper()
	binary := os.Getenv("LITECOIND")
	if binary == "" {
		t.Skip("LITECOIND is required for the Litecoin integration test")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("LITECOIND: %v", err)
	}
	port := freeTCPPort(t)
	dataDir := t.TempDir()
	n := &litecoinNode{
		t: t, binary: binary, dataDir: dataDir,
		rpcURL:  fmt.Sprintf("http://127.0.0.1:%d", port),
		logPath: filepath.Join(dataDir, "litecoind.console.log"),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
	n.start()
	if err := n.call(false, nil, "createwallet", litecoinWallet); err != nil {
		t.Fatal(err)
	}
	n.walletMade = true
	t.Cleanup(n.close)
	return n
}

func (n *litecoinNode) start() {
	n.t.Helper()
	logFile, err := os.OpenFile(n.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		n.t.Fatal(err)
	}
	port := strings.TrimPrefix(n.rpcURL, "http://127.0.0.1:")
	args := []string{
		"-datadir=" + n.dataDir,
		"-regtest=1",
		"-server=1",
		"-listen=0",
		"-discover=0",
		"-dnsseed=0",
		"-rpcbind=127.0.0.1",
		"-rpcallowip=127.0.0.1",
		"-rpcport=" + port,
		"-rpcuser=" + litecoinRPCUser,
		"-rpcpassword=" + litecoinRPCPassword,
		"-fallbackfee=0.0001",
		"-txindex=1",
		"-printtoconsole=1",
	}
	n.cmd = exec.Command(n.binary, args...)
	n.cmd.Stdout, n.cmd.Stderr = logFile, logFile
	n.logFile = logFile
	if err := n.cmd.Start(); err != nil {
		_ = logFile.Close()
		n.t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var info struct {
			Chain string `json:"chain"`
		}
		if err := n.call(false, &info, "getblockchaininfo"); err == nil {
			if info.Chain != "regtest" {
				n.t.Fatalf("litecoind started on %q", info.Chain)
			}
			if n.walletMade {
				var loaded []string
				if err := n.call(false, &loaded, "listwallets"); err != nil {
					n.t.Fatal(err)
				} else if !slices.Contains(loaded, litecoinWallet) {
					if err := n.call(false, nil, "loadwallet", litecoinWallet); err != nil {
						n.t.Fatal(err)
					}
				}
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	n.dumpLog()
	n.t.Fatal("litecoind did not become ready")
}

func (n *litecoinNode) endpoint(wallet bool) string {
	if !wallet {
		return n.rpcURL
	}
	return n.rpcURL + "/wallet/" + url.PathEscape(litecoinWallet)
}

func (n *litecoinNode) call(wallet bool, result any, method string, params ...any) error {
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}{JSONRPC: "1.0", ID: "qday-walletd", Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, n.endpoint(wallet), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(litecoinRPCUser, litecoinRPCPassword)
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	limited, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage   `json:"result"`
		Error  *litecoinRPCError `json:"error"`
	}
	if err := json.Unmarshal(limited, &envelope); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	} else if envelope.Error != nil {
		return envelope.Error
	} else if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}

func (n *litecoinNode) stop() {
	n.t.Helper()
	if n.cmd == nil {
		return
	}
	_ = n.call(false, nil, "stop")
	done := make(chan error, 1)
	go func() { done <- n.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			n.dumpLog()
			n.t.Fatalf("litecoind stop: %v", err)
		}
	case <-time.After(15 * time.Second):
		_ = n.cmd.Process.Kill()
		<-done
		n.dumpLog()
		n.t.Fatal("litecoind did not stop")
	}
	_ = n.logFile.Close()
	n.cmd, n.logFile = nil, nil
}

func (n *litecoinNode) restart() {
	n.t.Helper()
	n.stop()
	n.start()
}

func (n *litecoinNode) close() {
	if n.cmd != nil {
		n.stop()
	}
	if n.t.Failed() {
		n.dumpLog()
	}
}

func (n *litecoinNode) dumpLog() {
	b, err := os.ReadFile(n.logPath)
	if err != nil {
		return
	}
	const max = 16 << 10
	if len(b) > max {
		b = b[len(b)-max:]
	}
	n.t.Logf("litecoind log:\n%s", b)
}

func (n *litecoinNode) newAddress() string {
	n.t.Helper()
	var address string
	if err := n.call(true, &address, "getnewaddress", "", "bech32"); err != nil {
		n.t.Fatal(err)
	}
	return address
}

func (n *litecoinNode) addressScript(address string) []byte {
	n.t.Helper()
	var info struct {
		Script string `json:"scriptPubKey"`
	}
	if err := n.call(true, &info, "getaddressinfo", address); err != nil {
		n.t.Fatal(err)
	}
	script, err := hex.DecodeString(info.Script)
	if err != nil {
		n.t.Fatal(err)
	}
	return script
}

func (n *litecoinNode) height() uint32 {
	n.t.Helper()
	var height uint32
	if err := n.call(false, &height, "getblockcount"); err != nil {
		n.t.Fatal(err)
	}
	return height
}

func (n *litecoinNode) mine(blocks int) []string {
	n.t.Helper()
	var hashes []string
	if err := n.call(true, &hashes, "generatetoaddress", blocks, n.newAddress()); err != nil {
		n.t.Fatal(err)
	}
	return hashes
}

func (n *litecoinNode) mempool() []string {
	n.t.Helper()
	var txids []string
	if err := n.call(false, &txids, "getrawmempool"); err != nil {
		n.t.Fatal(err)
	}
	return txids
}

func encodeLitecoinTx(t *testing.T, txn *wire.MsgTx) string {
	t.Helper()
	var encoded bytes.Buffer
	if err := txn.Serialize(&encoded); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(encoded.Bytes())
}

func decodeLitecoinTx(t *testing.T, encoded string) *wire.MsgTx {
	t.Helper()
	b, err := hex.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	txn := wire.NewMsgTx(2)
	if err := txn.Deserialize(bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	return txn
}

type litecoinHTLC struct {
	fundingHash   chainhash.Hash
	vout          uint32
	amount        int64
	pkScript      []byte
	witnessScript []byte
	recipientKey  *btcec.PrivateKey
	refundKey     *btcec.PrivateKey
	refundHeight  uint32
}

func newLitecoinHTLC(t *testing.T, secretHash [32]byte, refundHeight uint32) *litecoinHTLC {
	t.Helper()
	recipientKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	refundKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	witnessScript, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_IF).
		AddOp(txscript.OP_SHA256).
		AddData(secretHash[:]).
		AddOp(txscript.OP_EQUALVERIFY).
		AddData(recipientKey.PubKey().SerializeCompressed()).
		AddOp(txscript.OP_CHECKSIG).
		AddOp(txscript.OP_ELSE).
		AddInt64(int64(refundHeight)).
		AddOp(txscript.OP_CHECKLOCKTIMEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(refundKey.PubKey().SerializeCompressed()).
		AddOp(txscript.OP_CHECKSIG).
		AddOp(txscript.OP_ENDIF).
		Script()
	if err != nil {
		t.Fatal(err)
	}
	witnessHash := sha256.Sum256(witnessScript)
	pkScript, err := txscript.NewScriptBuilder().AddOp(txscript.OP_0).AddData(witnessHash[:]).Script()
	if err != nil {
		t.Fatal(err)
	}
	return &litecoinHTLC{
		amount: 2 * litoshisPerLitecoin, pkScript: pkScript, witnessScript: witnessScript,
		recipientKey: recipientKey, refundKey: refundKey, refundHeight: refundHeight,
	}
}

func (h *litecoinHTLC) fund(t *testing.T, node *litecoinNode) string {
	t.Helper()
	template := wire.NewMsgTx(2)
	template.AddTxOut(wire.NewTxOut(h.amount, h.pkScript))
	var funded struct {
		Hex string `json:"hex"`
	}
	if err := node.call(true, &funded, "fundrawtransaction", encodeLitecoinTx(t, template)); err != nil {
		t.Fatal(err)
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := node.call(true, &signed, "signrawtransactionwithwallet", funded.Hex); err != nil {
		t.Fatal(err)
	} else if !signed.Complete {
		t.Fatal("litecoin funding transaction was not fully signed")
	}
	txn := decodeLitecoinTx(t, signed.Hex)
	h.fundingHash = txn.TxHash()
	h.vout = ^uint32(0)
	for i, output := range txn.TxOut {
		if output.Value == h.amount && bytes.Equal(output.PkScript, h.pkScript) {
			h.vout = uint32(i)
			break
		}
	}
	if h.vout == ^uint32(0) {
		t.Fatal("funded transaction does not contain the requested HTLC output")
	}
	var txid string
	if err := node.call(false, &txid, "sendrawtransaction", signed.Hex); err != nil {
		t.Fatal(err)
	} else if txid != txn.TxID() {
		t.Fatalf("funding txid = %s, decoded = %s", txid, txn.TxID())
	}
	return signed.Hex
}

func (h *litecoinHTLC) spend(t *testing.T, node *litecoinNode, secret *[32]byte) string {
	t.Helper()
	const fee = int64(2_000)
	destination := node.addressScript(node.newAddress())
	hash := h.fundingHash
	txn := wire.NewMsgTx(2)
	input := wire.NewTxIn(wire.NewOutPoint(&hash, h.vout), nil, nil)
	input.Sequence = 0xfffffffe
	txn.AddTxIn(input)
	txn.AddTxOut(wire.NewTxOut(h.amount-fee, destination))
	key := h.recipientKey
	if secret == nil {
		txn.LockTime = h.refundHeight
		key = h.refundKey
	}
	fetcher := txscript.NewCannedPrevOutputFetcher(h.pkScript, h.amount)
	sigHashes := txscript.NewTxSigHashes(txn, fetcher)
	sig, err := txscript.RawTxInWitnessSignature(txn, sigHashes, 0, h.amount, h.witnessScript, txscript.SigHashAll, key)
	if err != nil {
		t.Fatal(err)
	}
	if secret == nil {
		txn.TxIn[0].Witness = wire.TxWitness{sig, nil, h.witnessScript}
	} else {
		txn.TxIn[0].Witness = wire.TxWitness{sig, secret[:], []byte{1}, h.witnessScript}
	}
	return encodeLitecoinTx(t, txn)
}

func (n *litecoinNode) mempoolAccept(encoded string) (bool, string) {
	n.t.Helper()
	var result []struct {
		Allowed      bool   `json:"allowed"`
		RejectReason string `json:"reject-reason"`
	}
	if err := n.call(false, &result, "testmempoolaccept", []string{encoded}); err != nil {
		n.t.Fatal(err)
	} else if len(result) != 1 {
		n.t.Fatalf("testmempoolaccept returned %d entries", len(result))
	}
	return result[0].Allowed, result[0].RejectReason
}

func (n *litecoinNode) send(encoded string) string {
	n.t.Helper()
	var txid string
	if err := n.call(false, &txid, "sendrawtransaction", encoded); err != nil {
		n.t.Fatal(err)
	}
	return txid
}

func (n *litecoinNode) rawTransaction(txid string) string {
	n.t.Helper()
	var encoded string
	if err := n.call(false, &encoded, "getrawtransaction", txid, false); err != nil {
		n.t.Fatal(err)
	}
	return encoded
}

func extractLitecoinSecret(t *testing.T, encoded string, expectedHash [32]byte) [32]byte {
	t.Helper()
	txn := decodeLitecoinTx(t, encoded)
	if len(txn.TxIn) != 1 || len(txn.TxIn[0].Witness) != 4 || len(txn.TxIn[0].Witness[1]) != 32 {
		t.Fatal("claim transaction does not contain the canonical HTLC witness")
	}
	var secret [32]byte
	copy(secret[:], txn.TxIn[0].Witness[1])
	if sha256.Sum256(secret[:]) != expectedHash {
		t.Fatal("claim transaction revealed the wrong secret")
	}
	return secret
}

type qdayTestParty struct {
	service  *Service
	node     *Node
	records  *meta.Store
	keyPath  string
	password string
}

func newAdditionalQdayParty(t *testing.T, shared *Node, name string, master [32]byte) *qdayTestParty {
	t.Helper()
	dir := t.TempDir()
	custody, err := shared.WM.AddWallet(wallet.Wallet{Name: name + " custody"})
	if err != nil {
		t.Fatal(err)
	}
	swaps, err := shared.WM.AddWallet(wallet.Wallet{Name: name + " swaps"})
	if err != nil {
		t.Fatal(err)
	}
	records, err := meta.Open(filepath.Join(dir, "walletd.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := qtypes.QdayKeysFromSeed(master)
	if err != nil {
		t.Fatal(err)
	}
	password := "integration-test-password"
	keyPath := filepath.Join(dir, "master.key")
	if err := qdaynode.WriteKey(keyPath, password, master, keys.Public); err != nil {
		t.Fatal(err)
	}
	node := &Node{Manifest: shared.Manifest, CM: shared.CM, WM: shared.WM, WalletID: custody.ID, SwapWalletID: swaps.ID}
	party := &qdayTestParty{node: node, records: records, keyPath: keyPath, password: password}
	party.restart(t)
	t.Cleanup(func() {
		party.service.Close()
		_ = records.Close()
	})
	return party
}

func (p *qdayTestParty) restart(t *testing.T) {
	t.Helper()
	if p.service != nil {
		p.service.Close()
	}
	p.service = NewService(context.Background(), p.node, p.records, p.keyPath, zap.NewNop())
	if err := p.service.Unlock(p.password); err != nil {
		t.Fatal(err)
	}
}

func registerQdayPair(t *testing.T, refund, recipient *Service, swapID string, secretHash [32]byte, refundHeight uint64) (SwapView, SwapView) {
	t.Helper()
	refundKeys, _, err := refund.CreateSwapKeys(swapID)
	if err != nil {
		t.Fatal(err)
	}
	recipientKeys, _, err := recipient.CreateSwapKeys(swapID)
	if err != nil {
		t.Fatal(err)
	}
	hash := hex.EncodeToString(secretHash[:])
	refundView, _, err := refund.RegisterSwap(RegisterSwapRequest{
		SwapID: swapID, Role: "refund", Counterparty: recipientKeys.Keys,
		SecretHash: hash, RefundHeight: refundHeight,
	})
	if err != nil {
		t.Fatal(err)
	}
	recipientView, _, err := recipient.RegisterSwap(RegisterSwapRequest{
		SwapID: swapID, Role: "recipient", Counterparty: refundKeys.Keys,
		SecretHash: hash, RefundHeight: refundHeight,
	})
	if err != nil {
		t.Fatal(err)
	} else if refundView.Address != recipientView.Address {
		t.Fatalf("parties derived different QDAY contracts: %s != %s", refundView.Address, recipientView.Address)
	}
	return refundView, recipientView
}

func restartPrimaryService(t *testing.T, current *Service, node *Node, records *meta.Store) *Service {
	t.Helper()
	keyPath := current.keyPath
	current.Close()
	restarted := NewService(context.Background(), node, records, keyPath, zap.NewNop())
	if err := restarted.Unlock("test-walletd-password"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	return restarted
}

func mineQdaySideBranch(t *testing.T, node *Node, base qtypes.ChainIndex, blocks int) {
	t.Helper()
	state, ok := node.CM.State(base.ID)
	if !ok {
		t.Fatal("missing QDAY side-branch state")
	}
	miner, err := qtypes.QdayKeysFromSeed([32]byte{0x51, 0x44, 0x41, 0x59})
	if err != nil {
		t.Fatal(err)
	}
	for range blocks {
		block := mining.Candidate(state, miner.Public, nil, state.PrevTimestamps[0].Add(2*time.Second))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		block, err = mining.Mine(ctx, state, block, 2, nil)
		cancel()
		if err != nil {
			t.Fatal(err)
		} else if err := node.CM.AddBlocks([]qtypes.Block{block}); err != nil {
			t.Fatal(err)
		}
		state, _ = consensus.ApplyBlock(state, block, consensus.V1BlockSupplement{}, time.Time{})
	}
	waitIndexed(t, node)
}

func TestQdayLitecoinAtomicSwap(t *testing.T) {
	t.Run("complete swap survives pending transactions restarts and reorgs", testCompleteQdayLitecoinSwap)
	t.Run("refund boundaries are enforced on both chains", testQdayLitecoinRefunds)
	t.Run("simultaneous swaps remain isolated", testConcurrentQdayLitecoinSwaps)
}

func testCompleteQdayLitecoinSwap(t *testing.T) {
	litecoin := newLitecoinNode(t)
	litecoin.mine(101)

	refundService, qdayNode, refundRecords, _ := newServiceTestAtV1Height(t, 1)
	recipient := newAdditionalQdayParty(t, qdayNode, "recipient", [32]byte{8, 8, 8})
	deposit, err := refundService.NewDepositAddress("qday-swap-funder")
	if err != nil {
		t.Fatal(err)
	}
	depositAddress, err := qtypes.ParseQdayAddress(deposit.Address)
	if err != nil {
		t.Fatal(err)
	}
	fundAddressFromPremine(t, qdayNode, depositAddress, qtypes.Siacoins(20))

	secret := [32]byte{4, 2, 4, 2, 9, 1}
	secretHash := sha256.Sum256(secret[:])
	const swapID = "ltc-qday-happy-path"
	registerQdayPair(t, refundService, recipient.service, swapID, secretHash, qdayNode.CM.Tip().Height+20)
	unit := qdayNode.CM.TipState().QdayUnits(qdayNode.CM.Tip().Height)
	qdayFunding, fresh, err := refundService.FundSwap(context.Background(), swapID, FundSwapRequest{
		AmountAtomic: qtypes.Siacoins(5).ExactString(), ExpectedUnitAtomic: unit.ExactString(),
	})
	if err != nil || !fresh || qdayFunding.Status != "mempool" {
		t.Fatalf("QDAY funding: %#v, fresh=%v, err=%v", qdayFunding, fresh, err)
	}

	ltcContract := newLitecoinHTLC(t, secretHash, litecoin.height()+20)
	ltcFundingRaw := ltcContract.fund(t, litecoin)
	ltcFundingID := decodeLitecoinTx(t, ltcFundingRaw).TxID()
	if !slices.Contains(litecoin.mempool(), ltcFundingID) {
		t.Fatal("Litecoin funding is not in the mempool")
	}

	// Both funding transactions remain deliberately unconfirmed while the two
	// wallet services and the Litecoin process restart.
	refundService = restartPrimaryService(t, refundService, qdayNode, refundRecords)
	recipient.restart(t)
	litecoin.restart()
	if !slices.Contains(litecoin.mempool(), ltcFundingID) {
		t.Fatal("Litecoin lost an unconfirmed funding transaction across restart")
	}
	if view, ok, err := refundService.Swap(swapID); err != nil || !ok || view.Status != "funding" {
		t.Fatalf("QDAY lost pending funding across restart: %#v, %v", view, err)
	}

	miner, err := qtypes.QdayKeysFromSeed([32]byte{0x51, 0x44, 0x41, 0x59})
	if err != nil {
		t.Fatal(err)
	}
	mineBlock(t, qdayNode, miner.Public)
	litecoin.mine(1)
	qdayFundedTip := qdayNode.CM.Tip()
	recipientView, ok, err := recipient.service.Swap(swapID)
	if err != nil || !ok || recipientView.Status != "funded" || len(recipientView.Outputs) != 1 {
		t.Fatalf("recipient did not observe QDAY funding: %#v, %v", recipientView, err)
	}

	// The QDAY sender claims Litecoin and thereby reveals the common secret.
	wrongSecret := secret
	wrongSecret[0] ^= 0xff
	if allowed, _ := litecoin.mempoolAccept(ltcContract.spend(t, litecoin, &wrongSecret)); allowed {
		t.Fatal("Litecoin accepted a claim with the wrong secret")
	}
	ltcClaimRaw := ltcContract.spend(t, litecoin, &secret)
	if allowed, reason := litecoin.mempoolAccept(ltcClaimRaw); !allowed {
		t.Fatalf("valid Litecoin claim rejected: %s", reason)
	}
	ltcClaimID := litecoin.send(ltcClaimRaw)
	claimBlock := litecoin.mine(1)[0]
	var confirmations struct {
		Confirmations int `json:"confirmations"`
	}
	if err := litecoin.call(false, &confirmations, "getrawtransaction", ltcClaimID, true); err != nil || confirmations.Confirmations != 1 {
		t.Fatalf("Litecoin claim confirmations: %#v, %v", confirmations, err)
	}

	// Orphan the claim. Litecoin restores it to the mempool, where the secret
	// must still be extractable, then confirms it again on the replacement tip.
	if err := litecoin.call(false, nil, "invalidateblock", claimBlock); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(litecoin.mempool(), ltcClaimID) {
		t.Fatal("Litecoin claim was not restored to the mempool after reorg")
	}
	revealed := extractLitecoinSecret(t, litecoin.rawTransaction(ltcClaimID), secretHash)
	litecoin.mine(1)

	if _, _, err := recipient.service.ClaimSwap(context.Background(), swapID, SpendSwapRequest{
		OutputID: recipientView.Outputs[0].ID, Secret: hex.EncodeToString(wrongSecret[:]),
	}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("QDAY accepted the wrong claim secret: %v", err)
	}
	qdayClaim, fresh, err := recipient.service.ClaimSwap(context.Background(), swapID, SpendSwapRequest{
		OutputID: recipientView.Outputs[0].ID, Secret: hex.EncodeToString(revealed[:]),
	})
	if err != nil || !fresh || qdayClaim.Status != "mempool" {
		t.Fatalf("QDAY claim: %#v, fresh=%v, err=%v", qdayClaim, fresh, err)
	}
	mineBlock(t, qdayNode, miner.Public)
	if view, _, err := refundService.Swap(swapID); err != nil || view.Status != "claimed" || view.Outputs[0].RevealedSecret != hex.EncodeToString(secret[:]) {
		t.Fatalf("QDAY sender did not observe the claim secret: %#v, %v", view, err)
	}

	// Replace the QDAY claim block with a longer empty branch. walletd must
	// demote and rebroadcast the claim, preserving the observed secret.
	mineQdaySideBranch(t, qdayNode, qdayFundedTip, 2)
	recipient.service.rebroadcast()
	if view, _, err := recipient.service.Swap(swapID); err != nil || view.Status != "claiming" || view.Outputs[0].RevealedSecret != hex.EncodeToString(secret[:]) {
		t.Fatalf("QDAY claim was not restored after reorg: %#v, %v", view, err)
	}
	mineBlock(t, qdayNode, miner.Public)
	recipient.service.rebroadcast()
	if view, _, err := recipient.service.Swap(swapID); err != nil || view.Status != "claimed" {
		t.Fatalf("QDAY claim did not reconfirm: %#v, %v", view, err)
	}
}

func testQdayLitecoinRefunds(t *testing.T) {
	litecoin := newLitecoinNode(t)
	litecoin.mine(101)

	refundService, qdayNode, _, _ := newServiceTestAtV1Height(t, 1)
	recipient := newAdditionalQdayParty(t, qdayNode, "refund-recipient", [32]byte{6, 6, 6})
	deposit, err := refundService.NewDepositAddress("refund-funds")
	if err != nil {
		t.Fatal(err)
	}
	address, err := qtypes.ParseQdayAddress(deposit.Address)
	if err != nil {
		t.Fatal(err)
	}
	fundAddressFromPremine(t, qdayNode, address, qtypes.Siacoins(10))
	secret := [32]byte{7, 7, 7, 7}
	secretHash := sha256.Sum256(secret[:])
	qdayRefundHeight := qdayNode.CM.Tip().Height + 3
	const swapID = "ltc-qday-refund"
	registerQdayPair(t, refundService, recipient.service, swapID, secretHash, qdayRefundHeight)
	unit := qdayNode.CM.TipState().QdayUnits(qdayNode.CM.Tip().Height)
	if _, _, err := refundService.FundSwap(context.Background(), swapID, FundSwapRequest{
		AmountAtomic: qtypes.Siacoins(3).ExactString(), ExpectedUnitAtomic: unit.ExactString(),
	}); err != nil {
		t.Fatal(err)
	}
	miner, err := qtypes.QdayKeysFromSeed([32]byte{0x51, 0x44, 0x41, 0x59})
	if err != nil {
		t.Fatal(err)
	}
	mineBlock(t, qdayNode, miner.Public)
	qdayView, _, err := refundService.Swap(swapID)
	if err != nil || len(qdayView.Outputs) != 1 {
		t.Fatalf("QDAY refund output: %#v, %v", qdayView, err)
	}
	qdayRefundRequest := SpendSwapRequest{OutputID: qdayView.Outputs[0].ID}
	if _, _, err := refundService.RefundSwap(context.Background(), swapID, qdayRefundRequest); err == nil || !strings.Contains(err.Error(), "locked until height") {
		t.Fatalf("early QDAY refund was accepted: %v", err)
	}

	ltcRefundHeight := litecoin.height() + 3
	ltcContract := newLitecoinHTLC(t, secretHash, ltcRefundHeight)
	ltcContract.fund(t, litecoin)
	litecoin.mine(1)
	ltcRefundRaw := ltcContract.spend(t, litecoin, nil)
	if allowed, reason := litecoin.mempoolAccept(ltcRefundRaw); allowed || reason == "" {
		t.Fatalf("early Litecoin refund: allowed=%v reason=%q", allowed, reason)
	}

	for qdayNode.CM.Tip().Height < qdayRefundHeight {
		mineBlock(t, qdayNode, miner.Public)
	}
	qdayRefund, fresh, err := refundService.RefundSwap(context.Background(), swapID, qdayRefundRequest)
	if err != nil || !fresh || qdayRefund.Status != "mempool" {
		t.Fatalf("mature QDAY refund: %#v, fresh=%v, err=%v", qdayRefund, fresh, err)
	}
	mineBlock(t, qdayNode, miner.Public)
	if view, _, err := refundService.Swap(swapID); err != nil || view.Status != "refunded" || view.Outputs[0].RevealedSecret != "" {
		t.Fatalf("QDAY refund was not confirmed cleanly: %#v, %v", view, err)
	}

	for litecoin.height() < ltcRefundHeight {
		litecoin.mine(1)
	}
	if allowed, reason := litecoin.mempoolAccept(ltcRefundRaw); !allowed {
		t.Fatalf("mature Litecoin refund rejected: %s", reason)
	}
	ltcRefundID := litecoin.send(ltcRefundRaw)
	litecoin.mine(1)
	refunded := decodeLitecoinTx(t, litecoin.rawTransaction(ltcRefundID))
	if len(refunded.TxIn) != 1 || len(refunded.TxIn[0].Witness) != 3 || len(refunded.TxIn[0].Witness[1]) != 0 {
		t.Fatal("Litecoin refund unexpectedly revealed a secret")
	}
	var txInfo struct {
		Confirmations int `json:"confirmations"`
	}
	if err := litecoin.call(false, &txInfo, "getrawtransaction", ltcRefundID, true); err != nil || txInfo.Confirmations != 1 {
		t.Fatalf("Litecoin refund confirmations: %#v, %v", txInfo, err)
	}
}

func testConcurrentQdayLitecoinSwaps(t *testing.T) {
	litecoin := newLitecoinNode(t)
	litecoin.mine(101)

	refundService, qdayNode, _, _ := newServiceTestAtV1Height(t, 1)
	recipient := newAdditionalQdayParty(t, qdayNode, "concurrent-recipient", [32]byte{5, 5, 5})
	miner, err := qtypes.QdayKeysFromSeed([32]byte{0x51, 0x44, 0x41, 0x59})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		deposit, err := refundService.NewDepositAddress(fmt.Sprintf("parallel-funds-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		address, err := qtypes.ParseQdayAddress(deposit.Address)
		if err != nil {
			t.Fatal(err)
		}
		fundAddressFromPremine(t, qdayNode, address, qtypes.Siacoins(5))
	}

	type activeSwap struct {
		id         string
		secret     [32]byte
		secretHash [32]byte
		ltc        *litecoinHTLC
	}
	active := make([]activeSwap, 3)
	for i := range active {
		active[i].id = fmt.Sprintf("parallel-ltc-qday-%d", i)
		active[i].secret[0], active[i].secret[31] = byte(i+1), byte(100+i)
		active[i].secretHash = sha256.Sum256(active[i].secret[:])
		registerQdayPair(t, refundService, recipient.service, active[i].id, active[i].secretHash, qdayNode.CM.Tip().Height+30)
		unit := qdayNode.CM.TipState().QdayUnits(qdayNode.CM.Tip().Height)
		if _, fresh, err := refundService.FundSwap(context.Background(), active[i].id, FundSwapRequest{
			AmountAtomic: qtypes.Siacoins(2).ExactString(), ExpectedUnitAtomic: unit.ExactString(),
		}); err != nil || !fresh {
			t.Fatalf("fund concurrent QDAY swap %d: fresh=%v, err=%v", i, fresh, err)
		}
		active[i].ltc = newLitecoinHTLC(t, active[i].secretHash, litecoin.height()+30)
		active[i].ltc.fund(t, litecoin)
	}
	mineBlock(t, qdayNode, miner.Public)
	litecoin.mine(1)

	for i := range active {
		claimRaw := active[i].ltc.spend(t, litecoin, &active[i].secret)
		if allowed, reason := litecoin.mempoolAccept(claimRaw); !allowed {
			t.Fatalf("Litecoin concurrent claim %d: %s", i, reason)
		}
		claimID := litecoin.send(claimRaw)
		revealed := extractLitecoinSecret(t, litecoin.rawTransaction(claimID), active[i].secretHash)
		view, _, err := recipient.service.Swap(active[i].id)
		if err != nil || len(view.Outputs) != 1 {
			t.Fatalf("QDAY concurrent output %d: %#v, %v", i, view, err)
		}
		if _, fresh, err := recipient.service.ClaimSwap(context.Background(), active[i].id, SpendSwapRequest{
			OutputID: view.Outputs[0].ID, Secret: hex.EncodeToString(revealed[:]),
		}); err != nil || !fresh {
			t.Fatalf("QDAY concurrent claim %d: fresh=%v, err=%v", i, fresh, err)
		}
	}
	litecoin.mine(1)
	mineBlock(t, qdayNode, miner.Public)
	for i := range active {
		view, _, err := recipient.service.Swap(active[i].id)
		if err != nil || view.Status != "claimed" || view.Outputs[0].RevealedSecret != hex.EncodeToString(active[i].secret[:]) {
			t.Fatalf("concurrent swap %d crossed state: %#v, %v", i, view, err)
		}
	}
}
