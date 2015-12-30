package libkbfs

import keybase1 "github.com/keybase/client/go/protocol"

// All section references below are to https://keybase.io/blog/crypto
// (version 1.3).

// TLFCryptKeyServerHalfID is the identifier type for a server-side key half.
type TLFCryptKeyServerHalfID struct {
	ID HMAC // Exported for serialization.
}

// DeepCopy returns a complete copy of a TLFCryptKeyServerHalfID.
func (id TLFCryptKeyServerHalfID) DeepCopy() TLFCryptKeyServerHalfID {
	return id
}

// String implements the Stringer interface for TLFCryptKeyServerHalfID.
func (id TLFCryptKeyServerHalfID) String() string {
	return id.ID.String()
}

// TLFCryptKeyInfo is a per-device key half entry in the TLFKeyBundle.
type TLFCryptKeyInfo struct {
	ClientHalf   EncryptedTLFCryptKeyClientHalf
	ServerHalfID TLFCryptKeyServerHalfID
	EPubKeyIndex int `codec:"i,omitempty"`
}

// DeepCopy returns a complete copy of a TLFCryptKeyInfo.
func (info TLFCryptKeyInfo) DeepCopy() TLFCryptKeyInfo {
	return TLFCryptKeyInfo{
		ClientHalf:   info.ClientHalf.DeepCopy(),
		ServerHalfID: info.ServerHalfID.DeepCopy(),
		EPubKeyIndex: info.EPubKeyIndex,
	}
}

// UserCryptKeyBundle is a map from a user devices (identified by the
// KID of the corresponding device CryptPublicKey) to the
// TLF's symmetric secret key information.
type UserCryptKeyBundle map[keybase1.KID]TLFCryptKeyInfo

// DeepCopy returns a complete copy of a UserCryptKeyBundle
func (uckb UserCryptKeyBundle) DeepCopy() UserCryptKeyBundle {
	newUckb := UserCryptKeyBundle{}
	for k, b := range uckb {
		newUckb[k] = b.DeepCopy()
	}
	return newUckb
}

func (uckb UserCryptKeyBundle) fillInDeviceInfo(crypto Crypto,
	uid keybase1.UID, tlfCryptKey TLFCryptKey,
	ePrivKey TLFEphemeralPrivateKey, ePubIndex int,
	publicKeys []CryptPublicKey) (
	serverMap map[keybase1.KID]TLFCryptKeyServerHalf, err error) {
	serverMap = make(map[keybase1.KID]TLFCryptKeyServerHalf)
	// for each device:
	//    * create a new random server half
	//    * mask it with the key to get the client half
	//    * encrypt the client half
	//
	// TODO: parallelize
	for _, k := range publicKeys {
		// Skip existing entries, only fill in new ones
		if _, ok := uckb[k.KID]; ok {
			continue
		}

		var serverHalf TLFCryptKeyServerHalf
		serverHalf, err = crypto.MakeRandomTLFCryptKeyServerHalf()
		if err != nil {
			return nil, err
		}

		var clientHalf TLFCryptKeyClientHalf
		clientHalf, err = crypto.MaskTLFCryptKey(serverHalf, tlfCryptKey)
		if err != nil {
			return nil, err
		}

		var encryptedClientHalf EncryptedTLFCryptKeyClientHalf
		encryptedClientHalf, err =
			crypto.EncryptTLFCryptKeyClientHalf(ePrivKey, k, clientHalf)
		if err != nil {
			return nil, err
		}

		var serverHalfID TLFCryptKeyServerHalfID
		serverHalfID, err =
			crypto.GetTLFCryptKeyServerHalfID(uid, k.KID, serverHalf)
		if err != nil {
			return nil, err
		}

		uckb[k.KID] = TLFCryptKeyInfo{
			ClientHalf:   encryptedClientHalf,
			ServerHalfID: serverHalfID,
			EPubKeyIndex: ePubIndex,
		}
		serverMap[k.KID] = serverHalf
	}

	return serverMap, nil
}

// GetKIDs returns the KIDs for the given bundle.
func (uckb UserCryptKeyBundle) GetKIDs() []keybase1.KID {
	var keys []keybase1.KID
	for k := range uckb {
		keys = append(keys, k)
	}
	return keys
}

// TLFKeyBundle is a bundle of all the keys for a top-level folder.
type TLFKeyBundle struct {
	// Maps from each writer to their crypt key bundle.
	WKeys map[keybase1.UID]UserCryptKeyBundle
	// Maps from each reader to their crypt key bundle.
	RKeys map[keybase1.UID]UserCryptKeyBundle

	// M_f as described in 4.1.1 of https://keybase.io/blog/crypto.
	TLFPublicKey TLFPublicKey `codec:"pubKey"`

	// M_e as described in 4.1.1 of https://keybase.io/blog/crypto.
	// Because devices can be added into the key generation after it
	// is initially created (so those devices can get access to
	// existing data), we track multiple ephemeral public keys; the
	// one used by a particular device is specified by EPubKeyIndex in
	// its TLFCryptoKeyInfo struct.
	TLFEphemeralPublicKeys []TLFEphemeralPublicKey `codec:"ePubKey"`
}

// DeepCopy returns a complete copy of this TLFKeyBundle.
func (tkb TLFKeyBundle) DeepCopy() TLFKeyBundle {
	newTkb := tkb
	newTkb.WKeys = make(map[keybase1.UID]UserCryptKeyBundle)
	for u, m := range tkb.WKeys {
		newTkb.WKeys[u] = m.DeepCopy()
	}
	newTkb.RKeys = make(map[keybase1.UID]UserCryptKeyBundle)
	for u, m := range tkb.RKeys {
		newTkb.RKeys[u] = m.DeepCopy()
	}
	newTkb.TLFPublicKey = tkb.TLFPublicKey.DeepCopy()
	newTkb.TLFEphemeralPublicKeys =
		make([]TLFEphemeralPublicKey, len(tkb.TLFEphemeralPublicKeys))
	for i, k := range tkb.TLFEphemeralPublicKeys {
		newTkb.TLFEphemeralPublicKeys[i] = k.DeepCopy()
	}
	return newTkb
}

// IsWriter returns true if the given user device is in the writer set.
func (tkb *TLFKeyBundle) IsWriter(user keybase1.UID, deviceKID keybase1.KID) bool {
	_, ok := tkb.WKeys[user][deviceKID]
	return ok
}

// IsReader returns true if the given user device is in the reader set.
func (tkb *TLFKeyBundle) IsReader(user keybase1.UID, deviceKID keybase1.KID) bool {
	_, ok := tkb.RKeys[user][deviceKID]
	return ok
}

type serverKeyMap map[keybase1.UID]map[keybase1.KID]TLFCryptKeyServerHalf

func fillInDevicesAndServerMap(crypto Crypto, newIndex int,
	cryptKeys map[keybase1.UID][]CryptPublicKey,
	cryptBundles map[keybase1.UID]UserCryptKeyBundle,
	ePubKey TLFEphemeralPublicKey, ePrivKey TLFEphemeralPrivateKey,
	tlfCryptKey TLFCryptKey, newServerKeys serverKeyMap) error {
	for u, keys := range cryptKeys {
		if _, ok := cryptBundles[u]; !ok {
			cryptBundles[u] = UserCryptKeyBundle{}
		}

		serverMap, err := cryptBundles[u].fillInDeviceInfo(
			crypto, u, tlfCryptKey, ePrivKey, newIndex, keys)
		if err != nil {
			return err
		}
		if len(serverMap) > 0 {
			newServerKeys[u] = serverMap
		}
	}
	return nil
}

// fillInDevices ensures that every device for every writer and reader
// in the provided lists has complete TLF crypt key info, and uses the
// new ephemeral key pair to generate the info if it doesn't yet
// exist.
func (tkb *TLFKeyBundle) fillInDevices(crypto Crypto,
	wKeys map[keybase1.UID][]CryptPublicKey,
	rKeys map[keybase1.UID][]CryptPublicKey, ePubKey TLFEphemeralPublicKey,
	ePrivKey TLFEphemeralPrivateKey, tlfCryptKey TLFCryptKey) (
	serverKeyMap, error) {
	tkb.TLFEphemeralPublicKeys =
		append(tkb.TLFEphemeralPublicKeys, ePubKey)
	newIndex := len(tkb.TLFEphemeralPublicKeys) - 1

	// now fill in the secret keys as needed
	newServerKeys := serverKeyMap{}
	err := fillInDevicesAndServerMap(crypto, newIndex, wKeys, tkb.WKeys,
		ePubKey, ePrivKey, tlfCryptKey, newServerKeys)
	if err != nil {
		return nil, err
	}
	err = fillInDevicesAndServerMap(crypto, newIndex, rKeys, tkb.RKeys,
		ePubKey, ePrivKey, tlfCryptKey, newServerKeys)
	if err != nil {
		return nil, err
	}
	return newServerKeys, nil
}

// GetTLFCryptKeyInfo returns the TLFCryptKeyInfo entry for the given user
// and device.
func (tkb TLFKeyBundle) GetTLFCryptKeyInfo(user keybase1.UID,
	currentCryptPublicKey CryptPublicKey) (TLFCryptKeyInfo, bool, error) {
	key := currentCryptPublicKey.KID
	if u, ok1 := tkb.WKeys[user]; ok1 {
		info, ok := u[key]
		return info, ok, nil
	} else if u, ok1 = tkb.RKeys[user]; ok1 {
		info, ok := u[key]
		return info, ok, nil
	}
	return TLFCryptKeyInfo{}, false, nil
}

// GetTLFEphemeralPublicKey returns the ephemeral public key used for
// the TLFCryptKeyInfo for the given user and device.
func (tkb TLFKeyBundle) GetTLFEphemeralPublicKey(user keybase1.UID,
	currentCryptPublicKey CryptPublicKey) (TLFEphemeralPublicKey, error) {
	key := currentCryptPublicKey.KID

	info, ok, err := tkb.GetTLFCryptKeyInfo(user, currentCryptPublicKey)
	if err != nil {
		return TLFEphemeralPublicKey{}, err
	}
	if !ok {
		return TLFEphemeralPublicKey{},
			TLFEphemeralPublicKeyNotFoundError{user, key}
	}

	return tkb.TLFEphemeralPublicKeys[info.EPubKeyIndex], nil
}

// GetTLFCryptPublicKeys returns the public crypt keys for the given user.
func (tkb TLFKeyBundle) GetTLFCryptPublicKeys(user keybase1.UID) ([]keybase1.KID, bool) {
	if u, ok1 := tkb.WKeys[user]; ok1 {
		return u.GetKIDs(), true
	} else if u, ok1 = tkb.RKeys[user]; ok1 {
		return u.GetKIDs(), true
	}
	return nil, false
}
