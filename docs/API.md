# QDAY walletd API

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
