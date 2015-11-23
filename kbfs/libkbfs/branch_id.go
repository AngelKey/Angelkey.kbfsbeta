package libkbfs

import (
	"encoding"
	"encoding/hex"
	"errors"
)

const (
	// BranchIDByteLen is the number of bytes in a per-device per-TLF branch ID.
	BranchIDByteLen = 16
	// BranchIDStringLen is the number of characters in the string
	// representation of a per-device pr-TLF branch ID.
	BranchIDStringLen = 2 * BranchIDByteLen
)

// BranchID encapsulates a per-device per-TLF branch ID.
type BranchID struct {
	id [BranchIDByteLen]byte
}

var _ encoding.BinaryMarshaler = (*BranchID)(nil)
var _ encoding.BinaryUnmarshaler = (*BranchID)(nil)

// NullBranchID is an empty BranchID
var NullBranchID = BranchID{}

// Bytes returns the bytes of the BranchID.
func (id BranchID) Bytes() []byte {
	return id.id[:]
}

// String implements the Stringer interface for BranchID.
func (id BranchID) String() string {
	return hex.EncodeToString(id.id[:])
}

// MarshalBinary implements the encoding.BinaryMarshaler interface for BranchID.
func (id BranchID) MarshalBinary() (data []byte, err error) {
	return id.id[:], nil
}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface
// for BranchID.
func (id *BranchID) UnmarshalBinary(data []byte) error {
	if len(data) != BranchIDByteLen {
		return errors.New("invalid BranchID")
	}
	copy(id.id[:], data)
	return nil
}

// ParseBranchID parses a hex encoded BranchID. Returns NullBranchID on failure.
func ParseBranchID(s string) BranchID {
	if len(s) != BranchIDStringLen {
		return NullBranchID
	}
	bytes, err := hex.DecodeString(s)
	if err != nil {
		return NullBranchID
	}
	var id BranchID
	err = id.UnmarshalBinary(bytes)
	if err != nil {
		id = NullBranchID
	}
	return id
}
