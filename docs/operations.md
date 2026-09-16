# Operations

## Files

The data directory contains:

| File | Purpose | Backup |
| --- | --- | --- |
| `master.key` | encrypted 32-byte master seed | yes |
| `walletd.sqlite3` | address references and withdrawal journal | yes |
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
