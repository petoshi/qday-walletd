// Package derive implements the documented QDAY walletd child-key scheme.
package derive

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"

	"go.sia.tech/core/types"
)

const domain = "QDAY/walletd/child/v1"

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
