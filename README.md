# QDAY WALLETD

## YOUR EXCHANGE ASKED FOR AN API.

Here it is.

`qday-walletd` is the headless custody daemon for QDAY. It runs its own fully
validating mainnet node, derives independent deposit addresses from one master
seed, indexes the selected chain, builds native dual-signature withdrawals and
keeps accepted transactions alive across restarts and reorganizations.

It is not the desktop wallet wearing a server flag. It has no browser UI, no
CPU miner and no reason to hold one wallet process per customer.

```text
exchange backend
      │ authenticated HTTP
      ▼
qday-walletd ───── QDAY P2P
      │
      ├── full selected-chain address index
      ├── encrypted deterministic master seed
      └── Ed25519 + SLH-DSA signing
```

## What it does

- idempotent deposit-address allocation using the exchange's customer or
  account reference;
- an indexed deposit feed with block IDs, heights and confirmations;
- exact atomic-unit balances with current QDAY display values;
- idempotent withdrawals identified by the exchange's request ID;
- dual Ed25519 and SLH-DSA signatures for every input;
- persistent withdrawal storage and proof updates after chain progress or a
  reorganization;
- automatic transaction rebroadcast;
- automatic DEFEND renewal when managed outputs approach their shield expiry;
- a local bearer-authenticated API and explicit readiness endpoint;
- Linux Docker and systemd deployment files.

QDAY consensus remains in [`petoshi/qday`](https://github.com/petoshi/qday).
This repository pins that source as a submodule instead of maintaining a
second copy of consensus.

## Required version

QDAY changed block validation at block 9,100. Use **qday-walletd v0.2.0 or
newer** on mainnet. Version 0.1.0 follows the old block format and cannot
validate the upgraded chain.

Upgrading does not change the seed, deposit addresses or database format. Stop
the daemon, keep the complete data directory, replace the executable and start
it again. Confirm `qday-walletd version` reports `0.2.0` or newer and wait for
`/readyz` before accepting deposits or withdrawals. No rescan is required.

## Build

You need Go 1.26, a C compiler and SQLite development headers.

```sh
git clone --recurse-submodules https://github.com/petoshi/qday-walletd.git
cd qday-walletd
go test ./...
go build -trimpath -o qday-walletd .
```

Tagged source and Linux x86_64/ARM64 archives are published on the
[releases page](https://github.com/petoshi/qday-walletd/releases/latest).

## Initialize

Put a passphrase of at least 12 characters in a private file:

```sh
install -m 600 /dev/null /etc/qday-walletd/password
editor /etc/qday-walletd/password

./qday-walletd init \
  -data /var/lib/qday-walletd \
  -password-file /etc/qday-walletd/password
```

The command prints one 24-word QDAY seed phrase exactly once. Store it offline.
To import an existing walletd phrase, pass `-phrase-file` containing those 24
words.

Start the daemon:

```sh
./qday-walletd run \
  -data /var/lib/qday-walletd \
  -password-file /etc/qday-walletd/password
```

The API listens on `127.0.0.1:19772`. Its bearer token is created at
`/var/lib/qday-walletd/api.token`. The QDAY P2P listener uses `19771/TCP`.

## First deposit address

```sh
TOKEN=$(cat /var/lib/qday-walletd/api.token)

curl -sS http://127.0.0.1:19772/v1/addresses \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"reference":"customer-1841"}'
```

Repeating the same reference returns the same address. It never consumes
another child index.

## Withdrawal

Read `unitAtomic` from `/v1/status`, then send atomic integers:

```sh
curl -sS http://127.0.0.1:19772/v1/withdrawals \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "requestID":"withdrawal-91827",
    "destination":"qday1p...",
    "amountAtomic":"1000000000000000000000000",
    "feeAtomic":"1000000000000000000000",
    "expectedUnitAtomic":"1000000000000000000000000"
  }'
```

`requestID` is an idempotency key. Repeating the same request returns the same
transaction. Reusing it for another amount, fee or destination is rejected.

## Backups

The seed phrase recovers the keys for every child index. Back up
`walletd.sqlite3` as well: it contains customer references, the highest issued
child index and the withdrawal journal. It contains no private keys.

If that database is lost, restore the encrypted master seed and regenerate a
known number of child addresses:

```sh
./qday-walletd recover-addresses \
  -data /var/lib/qday-walletd \
  -password-file /etc/qday-walletd/password \
  -count 100000
```

This recovers control of the coins. Customer references still come from the
database backup.

Read [API.md](docs/API.md) before connecting accounting software and
[operations.md](docs/operations.md) before putting the daemon on a server.

## Links

- [QDAY](https://pqday.com)
- [QDAY source](https://github.com/petoshi/qday)
- [Explorer](https://explorer.pqday.com)
- [Integration rules](https://github.com/petoshi/qday/blob/main/docs/integrations.md)
- [@_petoshi](https://x.com/_petoshi)

## License

MIT.
