// Copyright 2015 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package kbcmf

import ()

type receiverKeysPlaintext struct {
	_struct    bool   `codec:",toarray"`
	GroupID    uint32 `codec:"gid"`
	MACKey     []byte `codec:"mac,omitempty"`
	SessionKey []byte `codec:"sess"`
}

type receiverKeysCiphertexts struct {
	_struct bool   `codec:",toarray"`
	KID     []byte `codec:"key_id"`
	Keys    []byte `codec:"keys"`
	Sender  []byte `codec:"sender"`
}

// EncryptionHeader is the first packet in an encrypted message.
// It contains the encryptions of the session keys, and various
// message metadata.
type EncryptionHeader struct {
	_struct   bool                      `codec:",toarray"`
	Version   PacketVersion             `codec:"vers"`
	Tag       PacketTag                 `codec:"tag"`
	Receivers []receiverKeysCiphertexts `codec:"rcvrs"`
	Sender    []byte                    `codec:"sender"`
	seqno     PacketSeqno
}

// EncryptionBlock contains a block of encrypted data. It cointains
// the ciphertext, and any necessary MACs.
type EncryptionBlock struct {
	_struct    bool          `codec:",toarray"`
	Version    PacketVersion `codec:"vers"`
	Tag        PacketTag     `codec:"tag"`
	Ciphertext []byte        `codec:"ctext"`
	MACs       [][]byte      `codec:"macs"`
	seqno      PacketSeqno
}

func verifyRawKey(k []byte) error {
	if len(k) != len(RawBoxKey{}) {
		return ErrBadSenderKey
	}
	return nil
}

func (h *EncryptionHeader) validate() error {
	if h.Tag != PacketTagEncryptionHeader {
		return ErrWrongPacketTag{h.seqno, PacketTagEncryptionHeader, h.Tag}
	}
	if h.Version != PacketVersion1 {
		return ErrBadVersion{h.seqno, h.Version}
	}

	if err := verifyRawKey(h.Sender); err != nil {
		return err
	}

	return nil
}

func (b *EncryptionBlock) validate() error {
	if b.Tag != PacketTagEncryptionBlock {
		return ErrWrongPacketTag{b.seqno, PacketTagEncryptionBlock, b.Tag}
	}
	if b.Version != PacketVersion1 {
		return ErrBadVersion{b.seqno, b.Version}
	}
	return nil
}
