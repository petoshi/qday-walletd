// Package derive implements the documented QDAY walletd child-key scheme.
package derive

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"

	"go.sia.tech/core/types"
)

const (
	domain     = "QDAY/walletd/child/v1"
	swapDomain = "QDAY/walletd/swap/v1"
)

// Seed derives one independent 256-bit child seed from a wallet master seed.
// The index is encoded big-endian to keep the construction reproducible across
// implementations.
func Seed(master [32]byte, index uint64) (child [32]byte) {
	mac := hmac.New(sha512.New, master[:])
	_, _ = mac.Write([]byte(domain))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], index)
	_, _ = mac.Write(encoded[:])
	sum := mac.Sum(nil)
	copy(child[:], sum[:32])
	clear(sum)
	return
}

// Keys derives the QDAY Ed25519 and SLH-DSA key pair at index.
func Keys(master [32]byte, index uint64) (types.QdayPrivateKeys, error) {
	child := Seed(master, index)
	defer clear(child[:])
	return types.QdayKeysFromSeed(child)
}

// SwapSeed derives a session-specific seed in a namespace separate from
// custody child addresses. Length-prefixing the external session identifier
// keeps the derivation unambiguous for independent implementations.
func SwapSeed(master [32]byte, swapID string) (child [32]byte) {
	mac := hmac.New(sha512.New, master[:])
	_, _ = mac.Write([]byte(swapDomain))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(len(swapID)))
	_, _ = mac.Write(encoded[:])
	_, _ = mac.Write([]byte(swapID))
	sum := mac.Sum(nil)
	copy(child[:], sum[:32])
	clear(sum)
	return
}

// SwapKeys derives the independent Ed25519 and SLH-DSA pair for a swap ID.
func SwapKeys(master [32]byte, swapID string) (types.QdayPrivateKeys, error) {
	child := SwapSeed(master, swapID)
	defer clear(child[:])
	return types.QdayKeysFromSeed(child)
}
