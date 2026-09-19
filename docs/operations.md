# Operations

## Mainnet upgrade at block 9,100

QDAY v1 block validation begins at height 9,100. Mainnet operators must run
qday-walletd v0.2.0 or newer. Version 0.1.0 cannot follow the upgraded chain.

Upgrade in place:

1. Stop qday-walletd cleanly.
2. Keep the complete data directory, especially `master.key`,
   `walletd.sqlite3`, `index.sqlite3` and `consensus.db`.
3. Replace the binary with qday-walletd v0.2.0 or newer.
4. Start the daemon with the same flags and data directory.
5. Check `qday-walletd version`, then wait for `/readyz` to return HTTP 200.

The upgrade preserves the master seed, child addresses, withdrawal journal
and chain data. It requires no new seed, address regeneration or rescan.

## Files

The data directory contains:

| File | Purpose | Backup |
| --- | --- | --- |
| `master.key` | encrypted 32-byte master seed | yes |
| `walletd.sqlite3` | address references, withdrawal and swap journals | yes |
| `index.sqlite3` | rebuildable selected-chain address index | optional |
| `consensus.db` | rebuildable QDAY chain state | optional |
| `api.token` | API bearer credential | rotate if exposed |

Stop the daemon or use SQLite's online backup mechanism when copying a live
database. A plain copy of a WAL database without its `-wal` file is not a
backup.

## Network exposure

Keep the HTTP API on loopback. If another host must access it, place an
authenticated TLS reverse proxy or a private network in front and start with
`-allow-remote-api`. Possession of `api.token` plus an unlocked daemon permits
withdrawals.

Expose `19771/TCP` only when the node should accept inbound QDAY peers. Outbound
connections to the three seeds are sufficient for synchronization. Set
`-advertise public-host:19771` only when that address is actually reachable.

The Docker image listens on `0.0.0.0:19772` inside its network namespace. Bind
that port to host loopback:

```sh
-p 127.0.0.1:19772:19772
```

Publishing it as `-p 19772:19772` exposes the custody API on every host
interface. The bearer token does not replace TLS on an untrusted network.

## Lock state

Locked walletd continues validating blocks and recording deposits. It cannot:

- derive a new deposit address;
- sign a withdrawal;
- renew shields after PQ Day.

For an online hot wallet, provide a root-readable password file to `run` and
protect the host. For a manually unlocked deployment, monitor `unlocked` and
`shieldUntil` from the authenticated API.

## DEFEND

After PQ Day, walletd checks managed outputs whenever the chain changes and at
least every two minutes. When an output enters the final quarter of its
1,440-block shield, walletd groups up to 128 due outputs into a renewal paying
an internal deterministic address. The transaction uses the normal wallet fee
and is stored in the same persistent journal as a withdrawal.

An operator should alert when:

- `pqDay` is true and `unlocked` is false;
- `shieldUntil - height` falls below 360 blocks;
- a `defend` record remains `retrying`;
- `/readyz` remains unavailable.

## Atomic accounting

Persist atomic integers. Do not store only the displayed QDAY string.

Before PQ Day:

```text
1 QDAY = 10^24 atomic units
```

Starting at the PQ Day block:

```text
1 QDAY = 10^18 atomic units
```

The atomic UTXO set does not multiply. The display unit changes. The
`expectedUnitAtomic` withdrawal field prevents a stale accounting request from
crossing that boundary silently.

## Reorganizations

Deposit and withdrawal confirmations refer to the node's selected chain. A
reorganization can remove either. Poll the records until they reach the
exchange's own confirmation threshold, and continue monitoring credited
deposits according to the exchange's reorganization policy.

walletd retains the signed withdrawal and rebroadcasts it when a confirmation
is removed by a reorganization, provided its inputs remain valid.

Confirmed withdrawals remain under automatic reorganization monitoring for 144
blocks. An operator should stop credits and withdrawals during any deeper
reorganization and reconcile records against the new selected chain.

## Deterministic derivation

For child index `i`:

```text
childSeed = HMAC-SHA512(
  key  = masterSeed,
  data = "QDAY/walletd/child/v1" || uint64_big_endian(i)
)[0:32]
```

`childSeed` then enters QDAY's domain-separated Ed25519 and
SLH-DSA-SHA2-128s key derivation. The resulting native spending policy requires
both signatures. The construction changes no consensus rule.

Test vector for child index zero:

```text
masterSeed = 0102030400000000000000000000000000000000000000000000000000000000
childSeed  = a44783bfd58305c7f2003aaca0cbac28790c4aff46573e12d99680dee25d03d3
address    = qday1phvrc8fs0jauhhxhcq0yktyheu35adxcy7areps987zpw327043tq6s7wkk
```

The 24-word phrase encodes `masterSeed`. The public metadata database records
which child indices were issued and associates deposit references with them.

For an atomic-swap session ID `s`, walletd uses a separate derivation namespace:

```text
swapSeed = HMAC-SHA512(
  key  = masterSeed,
  data = "QDAY/walletd/swap/v1" || uint64_big_endian(len(s)) || bytes(s)
)[0:32]
```

`swapID` is restricted to 1 through 128 ASCII letters, digits, `.`, `_`, `:`
or `-`. The seed enters the same domain-separated Ed25519 and SLH-DSA-SHA2-128s
key derivation as a custody child, but it can never collide with the child-index
namespace. Back up `walletd.sqlite3`: the phrase controls the keys, while the
database records the session IDs and immutable contracts needed to reproduce
which swap key belongs to which trade.

Test vector:

```text
masterSeed = 0102030400000000000000000000000000000000000000000000000000000000
swapID     = dex-order-17
swapSeed   = aa2605584ef7f795bb4be46189ee4c614872e595a6bc30cf50e53ce9fa4bb308
address    = qday1p2etdds8uzx7r2a60n6la4peu02raw4fxkdpcwtkd8sn7l5dyn3cqhl9lwm
```

## Litecoin atomic-swap integration test

The integration suite starts a fresh official Litecoin Core regtest process
and a fresh in-process QDAY development chain. It mines test funds from both
chains and uses one SHA-256 secret in a real Litecoin P2WSH HTLC and a real
QDAY atomic-swap output. Neither process connects to a public network.

Set `LITECOIND` to Litecoin Core v0.21.5.8 or a compatible binary and run:

```sh
LITECOIND=/path/to/litecoind \
  go test -tags=integration ./internal/daemon \
  -run '^TestQdayLitecoinAtomicSwap$' -count=1 -v -timeout=5m
```

The suite verifies:

- unconfirmed funding on both chains and clean process/service restarts;
- Litecoin claim-secret extraction followed by the QDAY claim;
- Litecoin and QDAY claim recovery through reorganizations;
- rejection of early refunds and confirmation of mature refunds;
- three concurrent swaps with independent keys, contracts and secrets.

GitHub Actions downloads the pinned Litecoin archive from the official
`litecoin-project/litecoin` release and verifies its SHA-256 before running the
same command.
