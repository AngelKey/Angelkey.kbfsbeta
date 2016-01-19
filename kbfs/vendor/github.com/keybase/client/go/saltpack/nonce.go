package saltpack

import (
	"crypto/rand"
	"encoding/binary"
)

const NonceBytes = 24

// Nonce is a NaCl-style nonce, with 24 bytes of data, some of which can be
// counter values, and some of which can be random-ish values.
type Nonce [NonceBytes]byte

func nonceForSenderKeySecretBox() *Nonce {
	var n Nonce
	copy(n[:], "saltpack_sender_key_sbox")
	return &n
}

func nonceForPayloadKeyBox() *Nonce {
	var n Nonce
	copy(n[:], "saltpack_payload_key_box")
	return &n
}

func nonceForMACKeyBox(headerHash []byte) *Nonce {
	if len(headerHash) != 64 {
		panic("Header hash shorter than expected.")
	}
	var n Nonce
	copy(n[:], headerHash[:NonceBytes])
	return &n
}

// Construct the nonce for the ith block of payload.
func nonceForChunkSecretBox(i encryptionBlockNumber) *Nonce {
	var n Nonce
	copy(n[0:16], "saltpack_ploadsb")
	binary.BigEndian.PutUint64(n[16:], uint64(i))
	return &n
}

// SigNonce is a nonce for signatures.
type SigNonce [16]byte

// NewSigNonce creates a SigNonce with random bytes.
func NewSigNonce() (SigNonce, error) {
	var n SigNonce
	if _, err := rand.Read(n[:]); err != nil {
		return SigNonce{}, err
	}
	return n, nil
}
