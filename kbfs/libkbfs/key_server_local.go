package libkbfs

import (
	"errors"
	"sync"

	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/storage"
	"golang.org/x/net/context"
)

// KeyServerLocal puts/gets key server halves in/from a local leveldb instance.
type KeyServerLocal struct {
	config Config
	db     *leveldb.DB // TLFCryptKeyServerHalfID -> TLFCryptKeyServerHalf
	log    logger.Logger

	shutdown     *bool
	shutdownLock *sync.RWMutex
}

// Test that KeyServerLocal fully implements the KeyServer interface.
var _ KeyServer = (*KeyServerLocal)(nil)

func newKeyServerLocalWithStorage(config Config, storage storage.Storage) (
	*KeyServerLocal, error) {
	db, err := leveldb.Open(storage, leveldbOptions)
	if err != nil {
		return nil, err
	}
	kops := &KeyServerLocal{config, db, config.MakeLogger(""), new(bool),
		&sync.RWMutex{}}
	return kops, nil
}

// NewKeyServerLocal returns a KeyServerLocal with a leveldb instance at the
// given file.
func NewKeyServerLocal(config Config, dbfile string) (*KeyServerLocal, error) {
	storage, err := storage.OpenFile(dbfile)
	if err != nil {
		return nil, err
	}
	return newKeyServerLocalWithStorage(config, storage)
}

// NewKeyServerMemory returns a KeyServerLocal with an in-memory leveldb
// instance.
func NewKeyServerMemory(config Config) (*KeyServerLocal, error) {
	return newKeyServerLocalWithStorage(config, storage.NewMemStorage())
}

// GetTLFCryptKeyServerHalf implements the KeyServer interface for
// KeyServerLocal.
func (ks *KeyServerLocal) GetTLFCryptKeyServerHalf(ctx context.Context,
	serverHalfID TLFCryptKeyServerHalfID, key CryptPublicKey) (serverHalf TLFCryptKeyServerHalf, err error) {
	ks.shutdownLock.RLock()
	defer ks.shutdownLock.RUnlock()
	if *ks.shutdown {
		err = errors.New("Key server already shut down")
	}

	buf, err := ks.db.Get(serverHalfID.ID.Bytes(), nil)
	if err != nil {
		return
	}

	err = ks.config.Codec().Decode(buf, &serverHalf)
	if err != nil {
		return TLFCryptKeyServerHalf{}, err
	}

	_, uid, err := ks.config.KBPKI().GetCurrentUserInfo(ctx)
	if err != nil {
		return TLFCryptKeyServerHalf{}, err
	}

	err = ks.config.Crypto().VerifyTLFCryptKeyServerHalfID(
		serverHalfID, uid, key.kid, serverHalf)
	if err != nil {
		ks.log.CDebugf(ctx, "error verifying server half ID: %s", err)
		return TLFCryptKeyServerHalf{}, MDServerErrorUnauthorized{}
	}
	return serverHalf, nil
}

// PutTLFCryptKeyServerHalves implements the KeyOps interface for KeyServerLocal.
func (ks *KeyServerLocal) PutTLFCryptKeyServerHalves(ctx context.Context,
	serverKeyHalves map[keybase1.UID]map[keybase1.KID]TLFCryptKeyServerHalf) error {
	ks.shutdownLock.RLock()
	defer ks.shutdownLock.RUnlock()
	if *ks.shutdown {
		return errors.New("Key server already shut down")
	}

	// batch up the writes such that they're atomic.
	batch := &leveldb.Batch{}
	crypto := ks.config.Crypto()
	for uid, deviceMap := range serverKeyHalves {
		for deviceKID, serverHalf := range deviceMap {
			buf, err := ks.config.Codec().Encode(serverHalf)
			if err != nil {
				return err
			}
			id, err := crypto.GetTLFCryptKeyServerHalfID(uid, deviceKID, serverHalf)
			if err != nil {
				return err
			}
			batch.Put(id.ID.Bytes(), buf)
		}
	}
	return ks.db.Write(batch, nil)
}

// DeleteTLFCryptKeyServerHalf implements the KeyOps interface for
// KeyServerLocal.
func (ks *KeyServerLocal) DeleteTLFCryptKeyServerHalf(ctx context.Context,
	_ keybase1.UID, _ keybase1.KID,
	serverHalfID TLFCryptKeyServerHalfID) error {
	ks.shutdownLock.RLock()
	defer ks.shutdownLock.RUnlock()
	if *ks.shutdown {
		return errors.New("Key server already shut down")
	}

	// TODO: verify that the kid is really valid for the given uid

	if err := ks.db.Delete(serverHalfID.ID.Bytes(), nil); err != nil {
		return err
	}
	return nil
}

// Copies a key server but swaps the config.
func (ks *KeyServerLocal) copy(config Config) *KeyServerLocal {
	return &KeyServerLocal{config, ks.db, config.MakeLogger(""), ks.shutdown,
		ks.shutdownLock}
}

// Shutdown implements the KeyServer interface for KeyServerLocal.
func (ks *KeyServerLocal) Shutdown() {
	ks.shutdownLock.Lock()
	defer ks.shutdownLock.Unlock()
	if *ks.shutdown {
		return
	}
	*ks.shutdown = true

	if ks.db != nil {
		ks.db.Close()
	}
}
