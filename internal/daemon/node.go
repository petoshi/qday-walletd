package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/petoshi/qday-walletd/internal/meta"
	"go.sia.tech/core/gateway"
	"go.sia.tech/coreutils"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/walletd/v2/persist/sqlite"
	"go.sia.tech/walletd/v2/wallet"
	"go.uber.org/zap"
)

type NodeConfig struct {
	DataDir      string
	ManifestPath string
	ManifestData []byte
	P2PAddress   string
	Advertise    string
	Peers        []string
}

type Node struct {
	Manifest chain.QdayManifest
	CM       *chain.Manager
	WM       *wallet.Manager
	Syncer   *syncer.Syncer
	WalletID wallet.ID

	bdb      io.Closer
	store    *sqlite.Store
	listener net.Listener
}

func decodeManifest(r io.Reader) (m chain.QdayManifest, err error) {
	d := json.NewDecoder(io.LimitReader(r, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&m); err != nil {
		return m, err
	} else if d.Decode(new(any)) != io.EOF {
		return m, errors.New("trailing manifest content")
	} else if err = m.Validate(); err != nil {
		return m, err
	} else if m.Development {
		return m, errors.New("qday-walletd accepts QDAY mainnet only")
	}
	return m, nil
}

func LoadManifest(path string) (m chain.QdayManifest, err error) {
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	return decodeManifest(f)
}

func ParsePeers(value string) ([]string, error) {
	seen := make(map[string]bool)
	var peers []string
	for _, value := range strings.Split(value, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		host, port, err := net.SplitHostPort(value)
		portNumber, portErr := strconv.ParseUint(port, 10, 16)
		if err != nil || host == "" || portErr != nil || portNumber == 0 {
			return nil, fmt.Errorf("invalid peer %q: expected host:port", value)
		}
		if !seen[value] {
			seen[value] = true
			peers = append(peers, value)
		}
	}
	if len(peers) == 0 {
		return nil, errors.New("at least one QDAY peer is required")
	}
	return peers, nil
}

func OpenNode(ctx context.Context, cfg NodeConfig, records *meta.Store, log *zap.Logger) (*Node, error) {
	var manifest chain.QdayManifest
	var err error
	if len(cfg.ManifestData) != 0 {
		manifest, err = decodeManifest(strings.NewReader(string(cfg.ManifestData)))
	} else {
		manifest, err = LoadManifest(cfg.ManifestPath)
	}
	if err != nil {
		return nil, fmt.Errorf("load mainnet manifest: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, err
	} else if err := os.Chmod(cfg.DataDir, 0700); err != nil {
		return nil, err
	}

	bdb, err := coreutils.OpenBoltChainDB(filepath.Join(cfg.DataDir, "consensus.db"))
	if err != nil {
		return nil, err
	}
	cleanupBDB := func() { _ = bdb.Close() }
	db, tip, err := chain.NewDBStore(bdb, &manifest.Network, manifest.Genesis, nil)
	if err != nil {
		cleanupBDB()
		return nil, err
	}
	cm := chain.NewManager(db, tip, chain.WithLog(log.Named("chain")))
	if index, ok := cm.BestIndex(0); !ok || index.ID != manifest.Genesis.ID() {
		cleanupBDB()
		return nil, errors.New("data directory belongs to another genesis")
	}

	store, err := sqlite.OpenDatabase(filepath.Join(cfg.DataDir, "index.sqlite3"), sqlite.WithLog(log.Named("index")))
	if err != nil {
		cleanupBDB()
		return nil, err
	}
	fail := func() {
		_ = store.Close()
		cleanupBDB()
	}
	for _, peer := range cfg.Peers {
		if err := store.AddPeer(peer); err != nil {
			fail()
			return nil, err
		}
	}
	peerStore, err := sqlite.NewPeerStore(store)
	if err != nil {
		fail()
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.P2PAddress)
	if err != nil {
		fail()
		return nil, fmt.Errorf("listen for QDAY peers: %w", err)
	}
	advertise := cfg.Advertise
	if advertise == "" {
		host, port, _ := net.SplitHostPort(listener.Addr().String())
		if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
			advertise = net.JoinHostPort("127.0.0.1", port)
		} else {
			advertise = listener.Addr().String()
		}
	} else if peers, err := ParsePeers(advertise); err != nil || len(peers) != 1 {
		_ = listener.Close()
		fail()
		if err == nil {
			err = errors.New("advertised address must contain one host:port")
		}
		return nil, fmt.Errorf("invalid advertised address: %w", err)
	}
	sy := syncer.New(listener, cm, peerStore, gateway.Header{
		GenesisID:  manifest.Genesis.ID(),
		UniqueID:   gateway.GenerateUniqueID(),
		NetAddress: advertise,
	},
		syncer.WithBootstrapPeers(cfg.Peers),
		syncer.WithMaxInboundPeers(16),
		syncer.WithMaxOutboundPeers(8),
		syncer.WithLogger(log.Named("p2p")),
	)
	go func() {
		if err := sy.Run(); err != nil && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
			log.Error("QDAY syncer stopped", zap.Error(err))
		}
	}()

	wm, err := wallet.NewManager(cm, store, wallet.WithLogger(log.Named("wallet")), wallet.WithIndexMode(wallet.IndexModeFull))
	if err != nil {
		_ = sy.Close()
		_ = listener.Close()
		fail()
		return nil, err
	}
	wallets, err := wm.Wallets()
	if err != nil {
		_ = wm.Close()
		_ = sy.Close()
		_ = listener.Close()
		fail()
		return nil, err
	}
	var custody wallet.Wallet
	for _, candidate := range wallets {
		if candidate.Name == "QDAY Custody" {
			custody = candidate
			break
		}
	}
	if custody.ID == 0 {
		custody, err = wm.AddWallet(wallet.Wallet{Name: "QDAY Custody", Description: "qday-walletd managed addresses"})
		if err != nil {
			_ = wm.Close()
			_ = sy.Close()
			_ = listener.Close()
			fail()
			return nil, err
		}
	}
	for offset := 0; ; offset += 1000 {
		addresses, err := records.AddressesPage(1000, offset)
		if err != nil {
			_ = wm.Close()
			_ = sy.Close()
			_ = listener.Close()
			fail()
			return nil, err
		}
		if len(addresses) == 0 {
			break
		}
		batch := make([]wallet.Address, len(addresses))
		for i, address := range addresses {
			policy := address.Public.Policy()
			batch[i] = wallet.Address{Address: policy.Address(), SpendPolicy: &policy, Description: address.Reference}
		}
		if err := wm.AddAddresses(custody.ID, batch...); err != nil {
			_ = wm.Close()
			_ = sy.Close()
			_ = listener.Close()
			fail()
			return nil, fmt.Errorf("register address batch at offset %d: %w", offset, err)
		}
		if len(addresses) < 1000 {
			break
		}
	}
	return &Node{Manifest: manifest, CM: cm, WM: wm, Syncer: sy, WalletID: custody.ID, bdb: bdb, store: store, listener: listener}, nil
}

func (n *Node) Close() error {
	var result error
	if n.Syncer != nil {
		if err := n.Syncer.Close(); err != nil {
			result = err
		}
	}
	if n.listener != nil {
		if err := n.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) && result == nil {
			result = err
		}
	}
	if err := n.WM.Close(); err != nil && result == nil {
		result = err
	}
	if err := n.store.Close(); err != nil && result == nil {
		result = err
	}
	if err := n.bdb.Close(); err != nil && result == nil {
		result = err
	}
	return result
}

func (n *Node) NetworkSynced() bool {
	if n.Manifest.Development {
		return true
	} else if n.Syncer == nil {
		return false
	}
	for _, peer := range n.Syncer.Peers() {
		if peer.Err() == nil && peer.Synced() {
			return true
		}
	}
	return false
}
