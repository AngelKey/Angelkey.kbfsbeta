package libkbfs

import (
	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
	"golang.org/x/net/context"
)

// KeyManagerStandard implements the KeyManager interface by fetching
// keys from KeyOps and KBPKI, and computing the complete keys
// necessary to run KBFS.
type KeyManagerStandard struct {
	config Config
	log    logger.Logger
}

// NewKeyManagerStandard returns a new KeyManagerStandard
func NewKeyManagerStandard(config Config) *KeyManagerStandard {
	return &KeyManagerStandard{config, config.MakeLogger("")}
}

// GetTLFCryptKeyForEncryption implements the KeyManager interface for
// KeyManagerStandard.
func (km *KeyManagerStandard) GetTLFCryptKeyForEncryption(ctx context.Context,
	md *RootMetadata) (tlfCryptKey TLFCryptKey, err error) {
	return km.getTLFCryptKey(ctx, md, md.LatestKeyGeneration())
}

// GetTLFCryptKeyForMDDecryption implements the KeyManager interface
// for KeyManagerStandard.
func (km *KeyManagerStandard) GetTLFCryptKeyForMDDecryption(
	ctx context.Context, md *RootMetadata) (
	tlfCryptKey TLFCryptKey, err error) {
	return km.getTLFCryptKey(ctx, md, md.LatestKeyGeneration())
}

// GetTLFCryptKeyForBlockDecryption implements the KeyManager interface for
// KeyManagerStandard.
func (km *KeyManagerStandard) GetTLFCryptKeyForBlockDecryption(
	ctx context.Context, md *RootMetadata, blockPtr BlockPointer) (
	tlfCryptKey TLFCryptKey, err error) {
	return km.getTLFCryptKey(ctx, md, blockPtr.KeyGen)
}

func (km *KeyManagerStandard) getTLFCryptKey(ctx context.Context,
	md *RootMetadata, keyGen KeyGen) (tlfCryptKey TLFCryptKey, err error) {
	if md.ID.IsPublic() {
		tlfCryptKey = PublicTLFCryptKey
		return
	}

	if keyGen < FirstValidKeyGen {
		err = InvalidKeyGenerationError{md.GetTlfHandle(), keyGen}
		return
	}
	// Is this some key we don't know yet?  Shouldn't really ever happen,
	// since we must have seen the MD that led us to this block, which
	// should include all the latest keys.  Consider this a failsafe.
	if keyGen > md.LatestKeyGeneration() {
		err = NewKeyGenerationError{md.GetTlfHandle(), keyGen}
		return
	}

	// look in the cache first
	kcache := km.config.KeyCache()
	if tlfCryptKey, err = kcache.GetTLFCryptKey(md.ID, keyGen); err == nil {
		return
	}

	// Get the encrypted version of this secret key for this device
	kbpki := km.config.KBPKI()
	uid, err := kbpki.GetCurrentUID(ctx)
	if err != nil {
		return
	}

	currentCryptPublicKey, err := kbpki.GetCurrentCryptPublicKey(ctx)
	if err != nil {
		return
	}

	info, ok, err := md.GetTLFCryptKeyInfo(keyGen, uid, currentCryptPublicKey)
	if err != nil {
		return
	}
	if !ok {
		err = NewReadAccessError(ctx, km.config, md.GetTlfHandle(), uid)
		return
	}

	ePublicKey, err := md.GetTLFEphemeralPublicKey(keyGen, uid,
		currentCryptPublicKey)
	if err != nil {
		return
	}

	crypto := km.config.Crypto()
	clientHalf, err :=
		crypto.DecryptTLFCryptKeyClientHalf(ctx, ePublicKey, info.ClientHalf)
	if err != nil {
		return
	}

	// now get the server-side key-half, do the unmasking, cache the result, return
	// TODO: can parallelize the get() with decryption
	kops := km.config.KeyOps()
	serverHalf, err := kops.GetTLFCryptKeyServerHalf(ctx, info.ServerHalfID)
	if err != nil {
		return
	}

	if tlfCryptKey, err = crypto.UnmaskTLFCryptKey(serverHalf, clientHalf); err != nil {
		return
	}

	if err = kcache.PutTLFCryptKey(md.ID, keyGen, tlfCryptKey); err != nil {
		tlfCryptKey = TLFCryptKey{}
		return
	}

	return
}

func (km *KeyManagerStandard) updateKeyBundle(ctx context.Context,
	md *RootMetadata, keyGen KeyGen, wKeys map[keybase1.UID][]CryptPublicKey,
	rKeys map[keybase1.UID][]CryptPublicKey, ePubKey TLFEphemeralPublicKey,
	ePrivKey TLFEphemeralPrivateKey, tlfCryptKey TLFCryptKey) error {
	tkb, err := md.getTLFKeyBundle(keyGen)
	if err != nil {
		return err
	}

	newServerKeys, err := tkb.fillInDevices(km.config.Crypto(), wKeys, rKeys,
		ePubKey, ePrivKey, tlfCryptKey)
	if err != nil {
		return err
	}

	// Push new keys to the key server.
	if err = km.config.KeyOps().
		PutTLFCryptKeyServerHalves(ctx, newServerKeys); err != nil {
		return err
	}

	return nil
}

func (km *KeyManagerStandard) checkForNewDevice(ctx context.Context,
	md *RootMetadata, keyInfoMap UserDeviceKeyInfoMap,
	expectedKeys map[keybase1.UID][]CryptPublicKey) bool {
	for u, keys := range expectedKeys {
		kids, ok := keyInfoMap[u]
		if !ok {
			// Currently there probably shouldn't be any new users
			// in the handle, but don't error just in case we ever
			// want to support that in the future.
			km.log.CInfof(ctx, "Rekey %s: adding new user %s", md.ID, u)
			return true
		}
		for _, k := range keys {
			km.log.CDebugf(ctx, "Checking key %v", k.kid)
			if _, ok := kids[k.kid]; !ok {
				km.log.CInfof(ctx, "Rekey %s: adding new device %s for user %s",
					md.ID, k.kid, u)
				return true
			}
		}
	}
	return false
}

func (km *KeyManagerStandard) checkForRemovedDevice(ctx context.Context,
	md *RootMetadata, keyInfoMap UserDeviceKeyInfoMap,
	expectedKeys map[keybase1.UID][]CryptPublicKey) bool {
	for u, kids := range keyInfoMap {
		keys, ok := expectedKeys[u]
		if !ok {
			// Currently there probably shouldn't be any users removed
			// from the handle, but don't error just in case we ever
			// want to support that in the future.
			km.log.CInfof(ctx, "Rekey %s: removing user %s", md.ID, u)
			return true
		}
		keyLookup := make(map[keybase1.KID]bool)
		for _, key := range keys {
			keyLookup[key.kid] = true
		}
		for kid := range kids {
			// Make sure every kid has an expected key
			if !keyLookup[kid] {
				km.log.CInfof(ctx,
					"Rekey %s: removing device %s for user %s", md.ID, kid, u)
				return true
			}
		}
	}
	return false
}

func (km *KeyManagerStandard) deleteKeysForRemovedDevices(ctx context.Context,
	md *RootMetadata, info UserDeviceKeyInfoMap,
	expectedKeys map[keybase1.UID][]CryptPublicKey) error {
	kops := km.config.KeyOps()
	var usersToDelete []keybase1.UID
	for u, kids := range info {
		keys, ok := expectedKeys[u]
		if !ok {
			// The user was completely removed from the handle, which
			// shouldn't happen but might as well make it work just in
			// case.
			km.log.CInfof(ctx, "Rekey %s: removing all server key halves "+
				"for user %s", md.ID, u)
			usersToDelete = append(usersToDelete, u)
			for kid, keyInfo := range kids {
				err := kops.DeleteTLFCryptKeyServerHalf(ctx, u, kid,
					keyInfo.ServerHalfID)
				if err != nil {
					return err
				}
			}
			continue
		}
		keyLookup := make(map[keybase1.KID]bool)
		for _, key := range keys {
			keyLookup[key.KID()] = true
		}
		var toRemove []keybase1.KID
		for kid, keyInfo := range kids {
			// Remove any keys that no longer belong.
			if !keyLookup[kid] {
				toRemove = append(toRemove, kid)
				km.log.CInfof(ctx, "Rekey %s: removing server key halves "+
					" for device %s of user %s", md.ID, kid, u)
				err := kops.DeleteTLFCryptKeyServerHalf(ctx, u, kid,
					keyInfo.ServerHalfID)
				if err != nil {
					return err
				}
			}
		}
		for _, kid := range toRemove {
			delete(info[u], kid)
		}
	}

	for _, u := range usersToDelete {
		delete(info, u)
	}

	return nil
}

// Rekey implements the KeyManager interface for KeyManagerStandard.
func (km *KeyManagerStandard) Rekey(ctx context.Context, md *RootMetadata) (
	rekeyDone bool, err error) {
	km.log.CDebugf(ctx, "Rekey %s", md.ID)
	defer func() { km.log.CDebugf(ctx, "Rekey %s done: %v", md.ID, err) }()

	currKeyGen := md.LatestKeyGeneration()
	if md.ID.IsPublic() || currKeyGen == PublicKeyGen {
		return false, InvalidPublicTLFOperation{md.ID, "rekey"}
	}

	handle := md.GetTlfHandle()

	// Decide whether we have a new device and/or a revoked device, or neither.
	// Look up all the device public keys for all writers and readers first.
	wKeys := make(map[keybase1.UID][]CryptPublicKey)
	rKeys := make(map[keybase1.UID][]CryptPublicKey)

	// TODO: parallelize
	for _, w := range handle.Writers {
		// HACK: clear cache
		if kdm, ok := km.config.KeybaseDaemon().(KeybaseDaemonMeasured); ok {
			if kdr, ok := kdm.delegate.(*KeybaseDaemonRPC); ok {
				kdr.setCachedUserInfo(w, UserInfo{})
			}
		}
		publicKeys, err := km.config.KBPKI().GetCryptPublicKeys(ctx, w)
		if err != nil {
			return false, err
		}
		wKeys[w] = publicKeys
	}
	for _, r := range handle.Readers {
		// HACK: clear cache
		if kdm, ok := km.config.KeybaseDaemon().(KeybaseDaemonMeasured); ok {
			if kdr, ok := kdm.delegate.(*KeybaseDaemonRPC); ok {
				kdr.setCachedUserInfo(r, UserInfo{})
			}
		}
		publicKeys, err := km.config.KBPKI().GetCryptPublicKeys(ctx, r)
		if err != nil {
			return false, err
		}
		rKeys[r] = publicKeys
	}

	// If there's at least one revoked device, add a new key generation
	addNewDevice := false
	incKeyGen := false
	if currKeyGen < FirstValidKeyGen {
		incKeyGen = true
	} else {
		// See if there is at least one new device in relation to the
		// current key bundle
		tkb, err := md.getTLFKeyBundle(currKeyGen)
		if err != nil {
			return false, err
		}

		addNewDevice = km.checkForNewDevice(ctx, md, tkb.WKeys, wKeys) ||
			km.checkForNewDevice(ctx, md, tkb.RKeys, rKeys)

		incKeyGen = km.checkForRemovedDevice(ctx, md, tkb.WKeys, wKeys) ||
			km.checkForRemovedDevice(ctx, md, tkb.RKeys, rKeys)
	}

	if !addNewDevice && !incKeyGen {
		km.log.CDebugf(ctx, "Skipping rekeying %s: no new or removed devices",
			md.ID)
		return false, nil
	}

	// send rekey start notification
	km.config.Reporter().Notify(ctx, rekeyNotification(ctx, km.config, handle, false))

	// For addNewDevice, we only use the ephemeral keys; incKeyGen
	// needs all of them.  ePrivKey will be discarded at the end of the
	// function in either case.
	//
	// TODO: split MakeRandomTLFKeys into two separate methods.
	pubKey, privKey, ePubKey, ePrivKey, tlfCryptKey, err :=
		km.config.Crypto().MakeRandomTLFKeys()
	if err != nil {
		return false, err
	}

	// If there's at least one new device, add that device to every key bundle.
	if addNewDevice {
		for keyGen := KeyGen(FirstValidKeyGen); keyGen <= currKeyGen; keyGen++ {
			currTlfCryptKey, err := km.getTLFCryptKey(ctx, md, keyGen)
			if err != nil {
				return false, err
			}

			err = km.updateKeyBundle(ctx, md, keyGen, wKeys, rKeys,
				ePubKey, ePrivKey, currTlfCryptKey)
			if err != nil {
				return false, err
			}
		}
	}

	if !incKeyGen {
		// we're done!
		return true, nil
	}

	newClientKeys := TLFKeyBundle{
		TLFWriterKeyBundle: &TLFWriterKeyBundle{
			WKeys:        make(UserDeviceKeyInfoMap),
			TLFPublicKey: pubKey,
			// TLFEphemeralPublicKeys will be filled in by updateKeyBundle
		},
		TLFReaderKeyBundle: &TLFReaderKeyBundle{
			RKeys: make(UserDeviceKeyInfoMap),
		},
	}
	err = md.AddNewKeys(newClientKeys)
	if err != nil {
		return false, err
	}
	currKeyGen = md.LatestKeyGeneration()
	err = km.updateKeyBundle(ctx, md, currKeyGen, wKeys, rKeys, ePubKey,
		ePrivKey, tlfCryptKey)
	if err != nil {
		return false, err
	}
	md.data.TLFPrivateKey = privKey

	// Delete server-side key halves for any revoked devices.
	for keygen := KeyGen(FirstValidKeyGen); keygen <= currKeyGen; keygen++ {
		tkb, err := md.getTLFKeyBundle(keygen)
		if err != nil {
			return false, err
		}

		err = km.deleteKeysForRemovedDevices(ctx, md, tkb.WKeys, wKeys)
		if err != nil {
			return false, err
		}
		err = km.deleteKeysForRemovedDevices(ctx, md, tkb.RKeys, rKeys)
		if err != nil {
			return false, err
		}
	}

	// Might as well cache the TLFCryptKey while we're at it.
	err = km.config.KeyCache().PutTLFCryptKey(md.ID, currKeyGen, tlfCryptKey)
	if err != nil {
		return false, err
	}
	return true, nil
}
