# QDAY walletd API

This API documentation applies to qday-walletd v0.2.0 and newer, which follows
QDAY's block 9,100 consensus upgrade. Older walletd builds must not be used on
current mainnet.

The API defaults to `http://127.0.0.1:19772`. Every `/v1` request requires:

```http
Authorization: Bearer <contents of api.token>
```

POST requests require `Content-Type: application/json`. Responses use
`Cache-Control: no-store`. Amounts under `atomic` are decimal integers; display
amounts under `qday` use the denomination active at the reported height.

## Health

### `GET /healthz`

Returns HTTP 200 while the process can serve requests. No authentication is
required.

### `GET /readyz`

Returns HTTP 200 only when the node has a synchronized peer and the address
index has reached the selected chain tip. Otherwise it returns HTTP 503 with
the current status. No authentication is required.

## Status

### `GET /v1/status`

Returns the genesis ID, chain and scan heights, synchronization state,
connections, mempool size, active atomic unit, address count, lock state and
PQ Day state.

Never submit a withdrawal while `synced` is false. Copy `unitAtomic` into
`expectedUnitAtomic` when creating a withdrawal.

## Wallet lock

### `POST /v1/wallet/unlock`

```json
{"password":"keystore passphrase"}
```

Loads the master seed into daemon memory. Address generation,
withdrawals and automatic DEFEND require an unlocked wallet.

### `POST /v1/wallet/lock`

```json
{}
```

Clears the master seed from daemon memory. Chain synchronization and deposit
indexing continue.

## Addresses

### `POST /v1/addresses`

```json
{"reference":"customer-1841"}
```

Creates a deterministic QDAY deposit address. `reference` must be 1 through
128 printable ASCII characters and should be the exchange's stable customer or
deposit-account ID. It is unique. Repeating it returns the existing address.

### `GET /v1/addresses?limit=50&offset=0`

Returns deposit and internal change addresses with their deterministic child
indices. `limit` is 1 through 200.

## Balance

### `GET /v1/balance`

Returns:

- `spendable`: confirmed, mature value available to a new transaction;
- `immature`: confirmed miner payouts inside their maturity delay;
- `pendingIn`: unspent outputs created by the local mempool;
- `shieldUntil`: earliest shield expiry among managed outputs after PQ Day;
- `unitAtomic`, `height` and `synced` for the accounting snapshot.

Values account for protocol decay at the selected height. Do not sum stored
output values yourself after PQ Day.

## Deposits

### `GET /v1/deposits?limit=50&offset=0`

Returns confirmed transaction outputs and miner payouts sent to addresses
created with `/v1/addresses`. Each record contains a stable deposit `id`, event
type, customer reference, exact amount, selected-chain block ID, height and
confirmations. Transaction deposits also include `transactionID` and
`outputIndex`; miner payouts use their output ID as `id`.
`creditable` is false when the transaction also spends a managed walletd
input. This prevents an internal transfer or a withdrawal back to a deposit
address from being credited as new customer money.

`nextOffset` paginates the current newest-first event view. It is not a durable
incremental checkpoint because new blocks insert events at the front. Poll from
offset zero, deduplicate by `id`, and follow records until the exchange's own
confirmation threshold. A response may contain a few more than `limit` records
when one transaction pays many managed addresses; the API always finishes that
transaction instead of losing outputs between pages.

Mempool transactions are not deposits. Choose a confirmation policy based on
accumulated-work reorganization risk.

## Withdrawals

### `POST /v1/withdrawals`

```json
{
  "requestID":"withdrawal-91827",
  "destination":"qday1p...",
  "amountAtomic":"1000000000000000000000000",
  "feeAtomic":"1000000000000000000000",
  "expectedUnitAtomic":"1000000000000000000000000"
}
```

The daemon selects at most 128 mature inputs, creates an internal change
address, performs any required DEFEND work, signs every input with Ed25519 and
SLH-DSA, stores the complete transaction and submits it to the local mempool.

`requestID` is mandatory and unique. An identical retry returns the original
transaction. A retry with another destination, amount or fee fails. This rule
prevents an HTTP timeout from becoming a double withdrawal.

`expectedUnitAtomic` must equal the current `/v1/status` value. This forces the
calling accounting system to review a request if PQ Day changes the displayed
denomination between preparation and submission.

`feeAtomic` is optional. The default is `10^21` atomic units: 0.001 QDAY before
PQ Day and 1,000 QDAY after the denomination change.

Statuses:

- `queued`: stored locally and waiting for mempool acceptance;
- `retrying`: the last submission failed temporarily and remains scheduled;
- `mempool`: accepted by the local transaction pool;
- `confirmed`: present in the selected chain, with `confirmations` and
  `blockHeight` and `blockID`.

### `GET /v1/withdrawals/{requestID}`

Returns one withdrawal or automatic DEFEND transaction.

### `GET /v1/withdrawals?limit=50&offset=0`

Returns recent records. `kind` is `withdrawal` for an exchange request and
`defend` for automatic shield renewal.

The daemon persists signed transactions, updates their accumulator proofs as
the chain moves and retries after restarts and reorganizations. Transaction IDs
do not change when accumulator proofs are updated.

## Atomic swaps

The swap API uses QDAY's native SHA-256 hashlock and block-height refund policy.
Each `swapID` has a separate deterministic Ed25519 and SLH-DSA key pair. It is
also the idempotency key for the contract: a registered ID cannot be reused
with another role, counterparty, hash or refund height.

The local role is one of:

- `recipient`: walletd can claim with the secret;
- `refund`: walletd can refund after `refundHeight`.

Contract outputs live in a separate internal index partition. They are absent
from `/v1/balance` and can never be selected by `/v1/withdrawals` or automatic
DEFEND. Claim and refund proceeds return to a normal managed change address.

### `POST /v1/swap-keys`

```json
{"swapID":"basicswap-order-17"}
```

The first call requires an unlocked wallet and creates the public descriptor
for this session. An identical retry returns the same descriptor. Private keys
never leave walletd and are not stored in `walletd.sqlite3`.

```json
{
  "swapID":"basicswap-order-17",
  "keys":{
    "classical":"32-byte lowercase hex",
    "reserve":"32-byte lowercase hex",
    "address":"qday1p..."
  },
  "createdAt":"2026-09-18T12:00:00Z"
}
```

### `POST /v1/swaps`

Register the immutable contract after exchanging public descriptors with the
counterparty:

```json
{
  "swapID":"basicswap-order-17",
  "role":"recipient",
  "counterparty":{
    "classical":"32-byte lowercase hex",
    "reserve":"32-byte lowercase hex",
    "address":"qday1p..."
  },
  "secretHash":"32-byte lowercase SHA-256 hex",
  "refundHeight":15000
}
```

The `address` inside `counterparty` is optional, but when supplied it must match
the two public keys. walletd combines the local session key with the
counterparty descriptor, derives the contract address and stores the complete
contract. Create `/v1/swap-keys` first. An identical retry returns the original
contract; changed terms are rejected.

### `POST /v1/swaps/{swapID}/fund`

```json
{
  "amountAtomic":"5000000000000000000000000",
  "feeAtomic":"1000000000000000000000",
  "expectedUnitAtomic":"1000000000000000000000000"
}
```

Builds a custody-funded transaction to the exact registered contract. Only one
local funding action is allowed per session. An identical retry returns the
same transaction. walletd refuses to fund at a height where the refund branch
can already be used. A counterparty may fund the address independently; the
status endpoint discovers that output without a local funding action.

### `POST /v1/swaps/{swapID}/claim`

Available only for a session registered with role `recipient`:

```json
{
  "outputID":"32-byte siacoin output ID",
  "feeAtomic":"1000000000000000000000",
  "secret":"32-byte lowercase hex"
}
```

The secret must match `secretHash`. walletd spends one confirmed contract
output, signs it with both QDAY key algorithms, persists the complete
transaction and submits it. Repeating the same output, fee and secret is
idempotent.

### `POST /v1/swaps/{swapID}/refund`

Available only for a session registered with role `refund` and only when the
selected tip has reached `refundHeight`:

```json
{
  "outputID":"32-byte siacoin output ID",
  "feeAtomic":"1000000000000000000000"
}
```

The request must not contain `secret`. It has the same persistence,
idempotency, proof update and rebroadcast behavior as a claim.

### `GET /v1/swaps/{swapID}`

Returns the immutable contract, current height, local role, every observed
contract output and all locally created funding, claim and refund actions.
Output statuses are:

- `funding`: output is in the local mempool;
- `funded`: output is confirmed and unspent;
- `claiming` or `refunding`: its spend is in the mempool;
- `claimed` or `refunded`: its spend is confirmed.

A claim includes `revealedSecret` as soon as its valid witness is visible in
the mempool or selected chain. Poll this endpoint instead of parsing witness
layout in the exchange integration. A reorganization removes orphaned output,
spend and secret observations from the returned selected-chain view.

### `GET /v1/swaps?limit=50&offset=0`

Returns registered sessions newest first. The aggregate session `status` uses
the same lifecycle names. Both read endpoints require the bearer token.
