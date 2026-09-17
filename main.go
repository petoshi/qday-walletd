package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	apihttp "github.com/petoshi/qday-walletd/internal/api"
	"github.com/petoshi/qday-walletd/internal/daemon"
	"github.com/petoshi/qday-walletd/internal/derive"
	"github.com/petoshi/qday-walletd/internal/meta"
	"go.sia.tech/core/types"
	seedwallet "go.sia.tech/coreutils/wallet"
	"go.sia.tech/walletd/v2/qday"
	"go.uber.org/zap"
)

var version = "dev"

var (
	//go:embed qday-mainnet.json
	assets embed.FS
)

const usage = `Usage:
  qday-walletd init [flags]       create or import the encrypted master wallet
  qday-walletd recovery [flags]   print the master seed phrase
  qday-walletd recover-addresses  rebuild a lost public address index
  qday-walletd run [flags]        run the QDAY node and custody API
  qday-walletd version            print version

Run qday-walletd with no command to start the daemon.`

func defaultDataDir() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "qday-walletd")
	case "darwin":
		return filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "qday-walletd")
	default:
		return filepath.Join(os.Getenv("HOME"), ".config", "qday-walletd")
	}
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func readSecret(path, name string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("-%s-file is required", name)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	} else if info.Size() > 16<<10 {
		return "", fmt.Errorf("%s file is too large", name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(b))
	clear(b)
	if value == "" {
		return "", fmt.Errorf("%s file is empty", name)
	}
	return value, nil
}

func initWallet(args []string) error {
	f := flag.NewFlagSet("init", flag.ContinueOnError)
	data := f.String("data", defaultDataDir(), "walletd data directory")
	passwordFile := f.String("password-file", "", "file containing the keystore passphrase")
	phraseFile := f.String("phrase-file", "", "optional file containing an existing qday-walletd seed phrase")
	if err := f.Parse(args); err != nil {
		return err
	} else if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	password, err := readSecret(*passwordFile, "password")
	if err != nil {
		return err
	}
	defer func() { password = "" }()
	if len(password) < 12 {
		return errors.New("wallet passphrase must contain at least 12 characters")
	}
	if err := privateDir(*data); err != nil {
		return err
	}
	var seed [32]byte
	if *phraseFile == "" {
		seed = seedwallet.NewQdaySeed()
	} else {
		phrase, err := readSecret(*phraseFile, "phrase")
		if err != nil {
			return err
		}
		seed, err = seedwallet.QdaySeedFromPhrase(phrase)
		phrase = ""
		if err != nil {
			return err
		}
	}
	defer clear(seed[:])
	keys, err := types.QdayKeysFromSeed(seed)
	if err != nil {
		return err
	}
	defer func() {
		clear(keys.Classical)
		keys = types.QdayPrivateKeys{}
	}()
	keyPath := filepath.Join(*data, "master.key")
	if err := qday.WriteKey(keyPath, password, seed, keys.Public); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("wallet already exists in this data directory")
		}
		return err
	}
	store, err := meta.Open(filepath.Join(*data, "walletd.sqlite3"))
	if err != nil {
		return err
	}
	_ = store.Close()
	fmt.Println("QDAY walletd initialized.")
	fmt.Println("Recovery seed phrase:")
	fmt.Println(seedwallet.QdaySeedPhrase(seed))
	fmt.Println("Store this phrase offline. It is the only recovery secret for every deterministic deposit address.")
	return nil
}

func recovery(args []string) error {
	f := flag.NewFlagSet("recovery", flag.ContinueOnError)
	data := f.String("data", defaultDataDir(), "walletd data directory")
	passwordFile := f.String("password-file", "", "file containing the keystore passphrase")
	if err := f.Parse(args); err != nil {
		return err
	} else if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	password, err := readSecret(*passwordFile, "password")
	if err != nil {
		return err
	}
	defer func() { password = "" }()
	seed, err := qday.ReadKey(filepath.Join(*data, "master.key"), password)
	if err != nil {
		return err
	}
	defer clear(seed[:])
	fmt.Println(seedwallet.QdaySeedPhrase(seed))
	return nil
}

func recoverAddresses(args []string) error {
	f := flag.NewFlagSet("recover-addresses", flag.ContinueOnError)
	data := f.String("data", defaultDataDir(), "walletd data directory")
	passwordFile := f.String("password-file", "", "file containing the keystore passphrase")
	count := f.Uint64("count", 0, "number of deterministic child addresses to restore")
	if err := f.Parse(args); err != nil {
		return err
	} else if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	} else if *count == 0 || *count > 10_000_000 {
		return errors.New("count must be between 1 and 10000000")
	}
	password, err := readSecret(*passwordFile, "password")
	if err != nil {
		return err
	}
	defer func() { password = "" }()
	seed, err := qday.ReadKey(filepath.Join(*data, "master.key"), password)
	if err != nil {
		return err
	}
	defer clear(seed[:])
	store, err := meta.Open(filepath.Join(*data, "walletd.sqlite3"))
	if err != nil {
		return err
	}
	defer store.Close()
	var added uint64
	for index := uint64(0); index < *count; index++ {
		keys, err := derive.Keys(seed, index)
		if err != nil {
			return err
		}
		public := keys.Public
		clear(keys.Classical)
		keys = types.QdayPrivateKeys{}
		existing, ok, err := store.AddressByIndex(index)
		if err != nil {
			return err
		} else if ok {
			if existing.Public != public {
				return fmt.Errorf("stored child address %d does not match the master seed", index)
			}
			continue
		}
		a := meta.Address{Index: index, Address: public.Address(), Public: public, Kind: "change", CreatedAt: time.Now().UTC()}
		if err := store.AddAddress(a); err != nil {
			return err
		}
		added++
	}
	fmt.Printf("Restored %d address records through child index %d. Customer references require a database backup.\n", added, *count-1)
	return nil
}

func loadOrCreateToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		var entropy [32]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			return "", err
		}
		b = []byte(hex.EncodeToString(entropy[:]))
		clear(entropy[:])
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", err
		}
		if _, err = f.Write(b); err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	clear(b)
	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return "", errors.New("api.token must contain 32 random bytes encoded as hexadecimal")
	}
	clear(raw)
	return token, nil
}

func loopbackListener(address string, allowRemote bool) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if !allowRemote && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("remote API binding requires -allow-remote-api and transport security in front of walletd")
	}
	return net.Listen("tcp", address)
}

func runDaemon(args []string) error {
	f := flag.NewFlagSet("run", flag.ContinueOnError)
	data := f.String("data", defaultDataDir(), "walletd data directory")
	listen := f.String("listen", "127.0.0.1:19772", "authenticated HTTP API address")
	p2p := f.String("p2p", ":19771", "QDAY P2P listen address")
	advertise := f.String("advertise", "", "public QDAY P2P host:port, when reachable")
	peersRaw := f.String("peers", "seed1.pqday.com:19771,seed2.pqday.com:19771,seed3.pqday.com:19771", "comma-separated QDAY bootstrap peers")
	network := f.String("network", "", "optional QDAY mainnet manifest override")
	passwordFile := f.String("password-file", "", "optional file used to unlock signing at startup")
	allowRemote := f.Bool("allow-remote-api", false, "allow API to bind beyond loopback; use a TLS reverse proxy")
	if err := f.Parse(args); err != nil {
		return err
	} else if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err := privateDir(*data); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*data, "master.key")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("wallet is not initialized; run qday-walletd init first")
		}
		return err
	}
	peers, err := daemon.ParsePeers(*peersRaw)
	if err != nil {
		return err
	}
	manifestData, err := assets.ReadFile("qday-mainnet.json")
	if err != nil {
		return err
	}
	cfg := daemon.NodeConfig{DataDir: *data, ManifestData: manifestData, ManifestPath: *network, P2PAddress: *p2p, Advertise: *advertise, Peers: peers}
	if *network != "" {
		cfg.ManifestData = nil
	}
	logger, err := zap.NewProduction()
	if err != nil {
		return err
	}
	defer logger.Sync()
	records, err := meta.Open(filepath.Join(*data, "walletd.sqlite3"))
	if err != nil {
		return err
	}
	defer records.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	node, err := daemon.OpenNode(ctx, cfg, records, logger)
	if err != nil {
		return err
	}
	defer node.Close()
	service := daemon.NewService(ctx, node, records, filepath.Join(*data, "master.key"), logger.Named("walletd"))
	defer service.Close()
	if *passwordFile != "" {
		password, err := readSecret(*passwordFile, "password")
		if err != nil {
			return err
		}
		err = service.Unlock(password)
		password = ""
		if err != nil {
			return fmt.Errorf("unlock wallet: %w", err)
		}
	}
	tokenPath := filepath.Join(*data, "api.token")
	token, err := loadOrCreateToken(tokenPath)
	if err != nil {
		return err
	}
	listener, err := loopbackListener(*listen, *allowRemote)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{
		Handler: apihttp.New(service, token), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()
	logger.Info("QDAY walletd started", zap.String("version", version), zap.String("http", listener.Addr().String()), zap.String("p2p", *p2p), zap.String("genesis", node.Manifest.Genesis.ID().String()), zap.Bool("unlocked", service.Unlocked()), zap.String("tokenFile", tokenPath))
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

func run(args []string) error {
	if len(args) == 0 {
		return runDaemon(nil)
	}
	switch args[0] {
	case "init":
		return initWallet(args[1:])
	case "recovery":
		return recovery(args[1:])
	case "recover-addresses":
		return recoverAddresses(args[1:])
	case "run":
		return runDaemon(args[1:])
	case "version", "--version", "-version":
		fmt.Println("qday-walletd", version)
		return nil
	case "help", "--help", "-h":
		fmt.Println(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "qday-walletd:", err)
		os.Exit(1)
	}
}
