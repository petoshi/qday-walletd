module github.com/petoshi/qday-walletd

go 1.26.0

require (
	github.com/mattn/go-sqlite3 v1.14.47
	go.sia.tech/core v0.21.6
	go.sia.tech/coreutils v0.23.5
	go.sia.tech/walletd/v2 v2.15.2
	go.uber.org/zap v1.28.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/cloudflare/circl v1.6.5 // indirect
	github.com/julienschmidt/httprouter v1.3.0 // indirect
	go.etcd.io/bbolt v1.5.0 // indirect
	go.sia.tech/jape v0.14.1 // indirect
	go.sia.tech/mux v1.5.2 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/exp v0.0.0-20240506185415-9bf2ced13842 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/tools v0.47.0 // indirect
	lukechampine.com/frand v1.5.1 // indirect
	lukechampine.com/upnp v0.3.0 // indirect
)

replace go.sia.tech/core => ./qday/core

replace go.sia.tech/coreutils => ./qday/coreutils

replace go.sia.tech/walletd/v2 => ./qday/node

replace lukechampine.com/upnp => ./qday/third_party/upnp
