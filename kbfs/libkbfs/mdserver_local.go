package libkbfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	keybase1 "github.com/keybase/client/go/protocol"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/storage"
	"github.com/syndtr/goleveldb/leveldb/util"
	"golang.org/x/net/context"
)

// MDServerLocal just stores blocks in local leveldb instances.
type MDServerLocal struct {
	config   Config
	handleDb *leveldb.DB // folder handle                   -> folderId
	mdDb     *leveldb.DB // MD ID                           -> root metadata (signed)
	revDb    *leveldb.DB // folderId+[deviceKID]+[revision] -> MD ID

	// mutex protects observers and sessionHeads
	mutex *sync.Mutex
	// Multiple instances of MDServerLocal could share a reference to
	// this map and sessionHead, and we use that to ensure that all
	// observers are fired correctly no matter which MDServerLocal
	// instance gets the Put() call.
	observers    map[TlfID]map[*MDServerLocal]chan<- error
	sessionHeads map[TlfID]*MDServerLocal

	shutdown     *bool
	shutdownLock *sync.RWMutex
}

func newMDServerLocalWithStorage(config Config, handleStorage, mdStorage,
	revStorage storage.Storage) (*MDServerLocal, error) {
	handleDb, err := leveldb.Open(handleStorage, leveldbOptions)
	if err != nil {
		return nil, err
	}
	mdDb, err := leveldb.Open(mdStorage, leveldbOptions)
	if err != nil {
		return nil, err
	}
	revDb, err := leveldb.Open(revStorage, leveldbOptions)
	if err != nil {
		return nil, err
	}
	mdserv := &MDServerLocal{config, handleDb, mdDb, revDb, &sync.Mutex{},
		make(map[TlfID]map[*MDServerLocal]chan<- error),
		make(map[TlfID]*MDServerLocal), new(bool), &sync.RWMutex{}}
	return mdserv, nil
}

// NewMDServerLocal constructs a new MDServerLocal object that stores
// data in the directories specified as parameters to this function.
func NewMDServerLocal(config Config, handleDbfile string, mdDbfile string,
	revDbfile string) (*MDServerLocal, error) {

	handleStorage, err := storage.OpenFile(handleDbfile)
	if err != nil {
		return nil, err
	}

	mdStorage, err := storage.OpenFile(mdDbfile)
	if err != nil {
		return nil, err
	}

	revStorage, err := storage.OpenFile(revDbfile)
	if err != nil {
		return nil, err
	}

	return newMDServerLocalWithStorage(config, handleStorage, mdStorage,
		revStorage)
}

// NewMDServerMemory constructs a new MDServerLocal object that stores
// all data in-memory.
func NewMDServerMemory(config Config) (*MDServerLocal, error) {
	return newMDServerLocalWithStorage(config,
		storage.NewMemStorage(), storage.NewMemStorage(),
		storage.NewMemStorage())
}

// Helper to aid in enforcement that only specified public keys can access TLF metdata.
func (md *MDServerLocal) checkPerms(ctx context.Context, id TlfID, checkWrite bool) (bool, error) {
	mdID, err := md.getHeadForTLF(ctx, id, Merged)
	if err != nil {
		return false, err
	}
	rmds, err := md.get(ctx, mdID)
	if err != nil {
		return false, err
	}
	if rmds == nil {
		// TODO: the real mdserver will actually reverse lookup the folder handle
		// and check that the UID is listed.
		return true, nil
	}
	device, err := md.getCurrentDeviceKID(ctx)
	if err != nil {
		return false, err
	}
	user, err := md.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return false, err
	}
	isWriter := rmds.MD.IsWriter(user, device)
	if checkWrite {
		return isWriter, nil
	}
	return isWriter || rmds.MD.IsReader(user, device), nil
}

// Helper to aid in enforcement that only specified public keys can access TLF metdata.
func (md *MDServerLocal) isReader(ctx context.Context, id TlfID) (bool, error) {
	return md.checkPerms(ctx, id, false)
}

// Helper to aid in enforcement that only specified public keys can access TLF metdata.
func (md *MDServerLocal) isWriter(ctx context.Context, id TlfID) (bool, error) {
	return md.checkPerms(ctx, id, true)
}

// GetForHandle implements the MDServer interface for MDServerLocal.
func (md *MDServerLocal) GetForHandle(ctx context.Context, handle *TlfHandle,
	mStatus MergeStatus) (TlfID, *RootMetadataSigned, error) {
	id := NullTlfID
	md.shutdownLock.RLock()
	defer md.shutdownLock.RUnlock()
	if *md.shutdown {
		return id, nil, errors.New("MD server already shut down")
	}

	handleBytes := handle.ToBytes(md.config)
	buf, err := md.handleDb.Get(handleBytes, nil)
	if err != nil && err != leveldb.ErrNotFound {
		return id, nil, MDServerError{err}
	}
	if err == nil {
		var id TlfID
		err := id.UnmarshalBinary(buf)
		if err != nil {
			return NullTlfID, nil, err
		}
		rmds, err := md.GetForTLF(ctx, id, mStatus)
		return id, rmds, err
	}

	// Allocate a new random ID.
	id, err = md.config.Crypto().MakeRandomTlfID(handle.IsPublic())
	if err != nil {
		return id, nil, MDServerError{err}
	}

	err = md.handleDb.Put(handleBytes, id.Bytes(), nil)
	if err != nil {
		return id, nil, MDServerError{err}
	}
	return id, nil, nil
}

// GetForTLF implements the MDServer interface for MDServerLocal.
func (md *MDServerLocal) GetForTLF(ctx context.Context, id TlfID,
	mStatus MergeStatus) (*RootMetadataSigned, error) {
	md.shutdownLock.RLock()
	defer md.shutdownLock.RUnlock()
	if *md.shutdown {
		return nil, errors.New("MD server already shut down")
	}

	// Check permissions
	ok, err := md.isReader(ctx, id)
	if err != nil {
		return nil, MDServerError{err}
	}
	if !ok {
		return nil, MDServerErrorUnauthorized{}
	}

	mdID, err := md.getHeadForTLF(ctx, id, mStatus)
	if err != nil {
		return nil, MDServerError{err}
	}
	if mdID == (MdID{}) {
		return nil, nil
	}
	rmds, err := md.get(ctx, mdID)
	if err != nil {
		return nil, MDServerError{err}
	}
	return rmds, nil
}

func (md *MDServerLocal) getHeadForTLF(ctx context.Context, id TlfID,
	mStatus MergeStatus) (mdID MdID, err error) {
	key, err := md.getMDKey(ctx, id, 0, mStatus)
	if err != nil {
		return
	}
	buf, err := md.revDb.Get(key[:], nil)
	if err != nil {
		if err == leveldb.ErrNotFound {
			mdID, err = MdID{}, nil
			return
		}
		return
	}
	return MdIDFromBytes(buf)
}

func (md *MDServerLocal) getMDKey(ctx context.Context, id TlfID,
	revision MetadataRevision, mStatus MergeStatus) ([]byte, error) {
	// short-cut
	if revision == MetadataRevisionUninitialized && mStatus == Merged {
		return id.Bytes(), nil
	}
	buf := &bytes.Buffer{}

	// add folder id
	_, err := buf.Write(id.Bytes())
	if err != nil {
		return []byte{}, err
	}

	// this order is significant. this way we can iterate by prefix
	// when pruning unmerged history per device.
	if mStatus == Unmerged {
		// add device KID
		deviceKID, err := md.getCurrentDeviceKID(ctx)
		if err != nil {
			return []byte{}, err
		}
		_, err = buf.Write(deviceKID.ToBytes())
		if err != nil {
			return []byte{}, err
		}
	}

	if revision >= MetadataRevisionInitial {
		// add revision
		err = binary.Write(buf, binary.BigEndian, revision.Number())
		if err != nil {
			return []byte{}, err
		}
	}
	return buf.Bytes(), nil
}

func (md *MDServerLocal) get(ctx context.Context, mdID MdID) (
	*RootMetadataSigned, error) {
	buf, err := md.mdDb.Get(mdID.Bytes(), nil)
	if err != nil {
		if err == leveldb.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	var rmds RootMetadataSigned
	err = md.config.Codec().Decode(buf, &rmds)
	return &rmds, err
}

func (md *MDServerLocal) getCurrentDeviceKID(ctx context.Context) (keybase1.KID, error) {
	key, err := md.config.KBPKI().GetCurrentCryptPublicKey(ctx)
	if err != nil {
		return keybase1.KID(""), err
	}
	return key.KID, nil
}

// GetRange implements the MDServer interface for MDServerLocal.
func (md *MDServerLocal) GetRange(ctx context.Context, id TlfID,
	mStatus MergeStatus, start, stop MetadataRevision) (
	[]*RootMetadataSigned, error) {
	md.shutdownLock.RLock()
	defer md.shutdownLock.RUnlock()
	if *md.shutdown {
		return nil, errors.New("MD server already shut down")
	}

	// Check permissions
	ok, err := md.isReader(ctx, id)
	if err != nil {
		return nil, MDServerError{err}
	}
	if !ok {
		return nil, MDServerErrorUnauthorized{}
	}

	var rmdses []*RootMetadataSigned
	startKey, err := md.getMDKey(ctx, id, start, mStatus)
	if err != nil {
		return rmdses, MDServerError{err}
	}
	stopKey, err := md.getMDKey(ctx, id, stop+1, mStatus)
	if err != nil {
		return rmdses, MDServerError{err}
	}

	iter := md.revDb.NewIterator(&util.Range{Start: startKey, Limit: stopKey}, nil)
	defer iter.Release()
	for iter.Next() {
		// get MD block from MD ID
		buf := iter.Value()
		mdID, err := MdIDFromBytes(buf)
		if err != nil {
			return rmdses, MDServerError{err}
		}
		rmds, err := md.get(ctx, mdID)
		if err != nil {
			return rmdses, MDServerError{err}
		}
		rmdses = append(rmdses, rmds)
	}
	if err := iter.Error(); err != nil {
		return rmdses, MDServerError{err}
	}

	return rmdses, nil
}

// Put implements the MDServer interface for MDServerLocal.
func (md *MDServerLocal) Put(ctx context.Context, rmds *RootMetadataSigned) error {
	md.shutdownLock.RLock()
	defer md.shutdownLock.RUnlock()
	if *md.shutdown {
		return errors.New("MD server already shut down")
	}

	// Consistency checks and the actual write need to be synchronized.
	md.mutex.Lock()
	defer md.mutex.Unlock()

	id := rmds.MD.ID

	// Check permissions
	ok, err := md.isWriter(ctx, id)
	if err != nil {
		return MDServerError{err}
	}
	if !ok {
		return MDServerErrorUnauthorized{}
	}

	mStatus := rmds.MD.MergedStatus()
	currHead, err := md.getHeadForTLF(ctx, id, mStatus)
	if err != nil {
		return MDServerError{err}
	}
	if mStatus == Unmerged && currHead == (MdID{}) {
		// currHead for unmerged history might be on the main branch
		currHead = rmds.MD.PrevRoot
	}

	// Consistency checks
	var head *RootMetadataSigned
	if currHead != (MdID{}) {
		head, err = md.get(ctx, currHead)
		if err != nil {
			return MDServerError{err}
		}
		if head == nil {
			return MDServerError{fmt.Errorf("head MD not found %v", currHead)}
		}
		if head.MD.Revision+1 != rmds.MD.Revision {
			return MDServerErrorConflictRevision{
				Expected: head.MD.Revision + 1,
				Actual:   rmds.MD.Revision,
			}
		}
	}
	if rmds.MD.PrevRoot != currHead {
		return MDServerErrorConflictPrevRoot{
			Expected: currHead,
			Actual:   rmds.MD.PrevRoot,
		}
	}
	// down here because this order is consistent with mdserver
	if head != nil {
		expected := head.MD.DiskUsage + rmds.MD.RefBytes - rmds.MD.UnrefBytes
		if rmds.MD.DiskUsage != expected {
			return MDServerErrorConflictDiskUsage{
				Expected: expected,
				Actual:   rmds.MD.DiskUsage,
			}
		}
	}

	mdID, err := rmds.MD.MetadataID(md.config)

	buf, err := md.config.Codec().Encode(rmds)
	if err != nil {
		return MDServerError{err}
	}

	// The folder ID points to the current MD block ID, and the
	// MD ID points to the buffer
	err = md.mdDb.Put(mdID.Bytes(), buf, nil)
	if err != nil {
		return MDServerError{err}
	}

	// Wrap changes to the revision DB in a batch.
	batch := new(leveldb.Batch)

	// Add an entry with the revision key.
	revKey, err := md.getMDKey(ctx, id, rmds.MD.Revision, mStatus)
	if err != nil {
		return MDServerError{err}
	}
	batch.Put(revKey, mdID.Bytes())

	// Add an entry with the head key.
	headKey, err := md.getMDKey(ctx, id, MetadataRevisionUninitialized,
		mStatus)
	if err != nil {
		return MDServerError{err}
	}
	batch.Put(headKey, mdID.Bytes())

	// Write the batch.
	err = md.revDb.Write(batch, nil)
	if err != nil {
		return MDServerError{err}
	}

	if mStatus == Merged {
		md.sessionHeads[id] = md

		// now fire all the observers that aren't from this session
		for k, v := range md.observers[id] {
			if k != md {
				v <- nil
				close(v)
				delete(md.observers[id], k)
			}
		}
		if len(md.observers[id]) == 0 {
			delete(md.observers, id)
		}
	}

	return nil
}

// PruneUnmerged implements the MDServer interface for MDServerLocal.
func (md *MDServerLocal) PruneUnmerged(ctx context.Context, id TlfID) error {
	md.shutdownLock.RLock()
	defer md.shutdownLock.RUnlock()
	if *md.shutdown {
		return errors.New("MD server already shut down")
	}

	// No revision and unmerged history.
	headKey, err := md.getMDKey(ctx, id, 0, Unmerged)

	// Do these deletes in atomic batches.
	revBatch, mdBatch := new(leveldb.Batch), new(leveldb.Batch)

	// Iterate and delete.
	iter := md.revDb.NewIterator(util.BytesPrefix(headKey), nil)
	defer iter.Release()
	for iter.Next() {
		mdID := iter.Value()
		// Queue these up for deletion.
		mdBatch.Delete(mdID)
		// Delete the reference from the revision DB.
		revBatch.Delete(iter.Key())
	}
	if err = iter.Error(); err != nil {
		return MDServerError{err}
	}

	// Write the batches of deletes.
	if err := md.revDb.Write(revBatch, nil); err != nil {
		return MDServerError{err}
	}
	if err := md.mdDb.Write(mdBatch, nil); err != nil {
		return MDServerError{err}
	}

	return nil
}

// RegisterForUpdate implements the MDServer interface for MDServerLocal.
func (md *MDServerLocal) RegisterForUpdate(ctx context.Context, id TlfID,
	currHead MetadataRevision) (<-chan error, error) {
	md.shutdownLock.RLock()
	defer md.shutdownLock.RUnlock()
	if *md.shutdown {
		return nil, errors.New("MD server already shut down")
	}

	md.mutex.Lock()
	defer md.mutex.Unlock()

	// are we already past this revision?  If so, fire observer
	// immediately
	currMergedHead, err := md.getHeadForTLF(ctx, id, Merged)
	if err != nil {
		return nil, err
	}
	var currMergedHeadRev MetadataRevision
	if currMergedHead != (MdID{}) {
		head, err := md.get(ctx, currMergedHead)
		if err != nil {
			return nil, MDServerError{err}
		}
		if head == nil {
			return nil,
				MDServerError{fmt.Errorf("head MD not found %v", currHead)}
		}
		currMergedHeadRev = head.MD.Revision
	}

	c := make(chan error, 1)
	if currMergedHeadRev > currHead && md != md.sessionHeads[id] {
		c <- nil
		close(c)
		return c, nil
	}

	if _, ok := md.observers[id]; !ok {
		md.observers[id] = make(map[*MDServerLocal]chan<- error)
	}

	// Otherwise, this is a legit observer.  This assumes that each
	// client will be using a unique instance of MDServerLocal.
	if _, ok := md.observers[id][md]; ok {
		// If the local node registers something twice, it indicates a
		// fatal bug.  Note that in the real MDServer implementation,
		// we should allow this, in order to make the RPC properly
		// idempotent.
		panic(fmt.Sprintf("Attempted double-registration for MDServerLocal %p",
			md))
	}
	md.observers[id][md] = c
	return c, nil
}

// Shutdown implements the MDServer interface for MDServerLocal.
func (md *MDServerLocal) Shutdown() {
	md.shutdownLock.Lock()
	defer md.shutdownLock.Unlock()
	if *md.shutdown {
		return
	}
	*md.shutdown = true

	if md.handleDb != nil {
		md.handleDb.Close()
	}
	if md.mdDb != nil {
		md.mdDb.Close()
	}
	if md.revDb != nil {
		md.revDb.Close()
	}
}

// This should only be used for testing with an in-memory server.
func (md *MDServerLocal) copy(config Config) *MDServerLocal {
	// NOTE: observers and sessionHeads are copied shallowly on
	// purpose, so that the MD server that gets a Put will notify all
	// observers correctly no matter where they got on the list.
	return &MDServerLocal{config, md.handleDb, md.mdDb, md.revDb, md.mutex,
		md.observers, md.sessionHeads, md.shutdown, md.shutdownLock}
}
