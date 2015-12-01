package libkbfs

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
	"golang.org/x/net/context"
)

// reqType indicates whether an operation makes MD modifications or not
type reqType int

const (
	read  reqType = iota // A read request
	write                // A write request
)

type branchType int

const (
	standard       branchType = iota // an online, read-write branch
	archive                          // an online, read-only branch
	offline                          // an offline, read-write branch
	archiveOffline                   // an offline, read-only branch
)

type state int

const (
	// cleanState: no outstanding local writes.
	cleanState state = iota
	// dirtyState: there are outstanding local writes that haven't yet been
	// synced.
	dirtyState
)

type syncInfo struct {
	oldInfo BlockInfo
	op      *syncOp
	unrefs  []BlockInfo
}

// Constants used in this file.  TODO: Make these configurable?
const (
	maxParallelBlockPuts = 10
	// Max response size for a single DynamoDB query is 1MB.
	maxMDsAtATime = 10
	// Time between checks for dirty files to flush, in case Sync is
	// never called.
	secondsBetweenBackgroundFlushes = 10
)

// FolderBranchOps implements the KBFSOps interface for a specific
// branch of a specific folder.  It is go-routine safe for operations
// within the folder.
//
// We use locks to protect against multiple goroutines accessing the
// same folder-branch.  The goal with our locking strategy is maximize
// concurrent access whenever possible.  See design/state_machine.md
// for more details.  There are three important locks:
//
// 1) writerLock: Any "remote-sync" operation (one which modifies the
//    folder's metadata) must take this lock during the entirety of
//    its operation, to avoid forking the MD.
//
// 2) headLock: This is a read/write mutex.  It must be taken for
//    reading before accessing any part of the current head MD.  It
//    should be taken for the shortest time possible -- that means in
//    general that it should be taken, and the MD copied to a
//    goroutine-local variable, and then it can be released.
//    Remote-sync operations should take it for writing after pushing
//    all of the blocks and MD to the KBFS servers (i.e., all network
//    accesses), and then hold it until after all notifications have
//    been fired, to ensure that no concurrent "local" operations ever
//    see inconsistent state locally.
//
// 3) blockLock: This too is a read/write mutex.  It must be taken for
//    reading before accessing any blocks in the block cache that
//    belong to this folder/branch.  This includes checking their
//    dirty status.  It should be taken for the shortest time possible
//    -- that means in general it should be taken, and then the blocks
//    that will be modified should be copied to local variables in the
//    goroutine, and then it should be released.  The blocks should
//    then be modified locally, and then readied and pushed out
//    remotely.  Only after the blocks have been pushed to the server
//    should a remote-sync operation take the lock again (this time
//    for writing) and put/finalize the blocks.  Write and Truncate
//    should take blockLock for their entire lifetime, since they
//    don't involve writes over the network.  Furthermore, if a block
//    is not in the cache and needs to be fetched, we should release
//    the mutex before doing the network operation, and lock it again
//    before writing the block back to the cache.
//
// We want to allow writes and truncates to a file that's currently
// being sync'd, like any good networked file system.  The tricky part
// is making sure the changes can both: a) be read while the sync is
// happening, and b) be applied to the new file path after the sync is
// done.
//
// For now, we just do the dumb, brute force thing for now: if a block
// is currently being sync'd, it copies the block and puts it back
// into the cache as modified.  Then, when the sync finishes, it
// throws away the modified blocks and re-applies the change to the
// new file path (which might have a completely different set of
// blocks, so we can't just reuse the blocks that were modified during
// the sync.)
type FolderBranchOps struct {
	config           Config
	folderBranch     FolderBranch
	bid              BranchID // protected by writerLock
	bType            branchType
	head             *RootMetadata
	observers        []Observer
	blockWriteLocked bool // blockLock is locked for writing tracks
	// Which blocks are currently being synced, so that writes and
	// truncates can do copy-on-write to avoid messing up the ongoing
	// sync.  The bool value is true if the block needs to be
	// copied before written to.
	copyFileBlocks map[BlockPointer]bool
	// Writes and truncates for blocks that were being sync'd, and
	// need to be replayed after the sync finishes on top of the new
	// versions of the blocks.
	deferredWrites []func(context.Context, *RootMetadata, path) error
	// set to true if this write or truncate should be deferred
	doDeferWrite bool
	// For writes and truncates, track the unsynced to-be-unref'd
	// block infos, per-path.  Uses a stripped BlockPointer in case
	// the Writer has changed during the operation.
	unrefCache map[BlockPointer]*syncInfo
	// For writes and truncates, track the modified (but not yet
	// committed) directory entries.  The outer map maps the parent
	// BlockPointer to the inner map, which maps the entry
	// BlockPointer to a modified entry.  Uses stripped BlockPointers
	// in case the Writer changed during the operation.
	deCache map[BlockPointer]map[BlockPointer]DirEntry

	// these locks, when locked concurrently by the same goroutine,
	// should only be taken in the following order to avoid deadlock:
	writerLock sync.Locker  // taken by any method making MD modifications
	headLock   sync.RWMutex // protects access to the MD

	// protects access to blocks in this folder and to
	// copyFileBlocks/deferredWrites
	blockLock sync.RWMutex

	obsLock   sync.RWMutex // protects access to observers
	cacheLock sync.Mutex   // protects unrefCache and deCache

	nodeCache NodeCache

	// Set to true when we have staged, unmerged commits for this
	// device.  This means the device has forked from the main branch
	// seen by other devices.  Protected by writerLock.
	staged bool

	// The current state of this folder-branch.
	state     state
	stateLock sync.Mutex

	// The current status summary for this folder
	status *folderBranchStatusKeeper

	// How to log
	log logger.Logger

	// Closed on shutdown
	shutdownChan chan struct{}

	// Can be used to turn off notifications for a while (e.g., for testing)
	updatePauseChan chan (<-chan struct{})

	// How to resolve conflicts
	cr *ConflictResolver
}

var _ KBFSOps = (*FolderBranchOps)(nil)

// NewFolderBranchOps constructs a new FolderBranchOps object.
func NewFolderBranchOps(config Config, fb FolderBranch,
	bType branchType) *FolderBranchOps {
	nodeCache := newNodeCacheStandard(fb)

	// make logger
	branchSuffix := ""
	if fb.Branch != MasterBranch {
		branchSuffix = " " + string(fb.Branch)
	}
	tlfStringFull := fb.Tlf.String()
	// Shorten the TLF ID for the module name.  8 characters should be
	// unique enough for a local node.
	log := config.MakeLogger(fmt.Sprintf("FBO %s%s", tlfStringFull[:8],
		branchSuffix))
	// But print it out once in full, just in case.
	log.CInfof(nil, "Created new folder-branch for %s", tlfStringFull)

	fbo := &FolderBranchOps{
		config:         config,
		folderBranch:   fb,
		bid:            BranchID{},
		bType:          bType,
		observers:      make([]Observer, 0),
		copyFileBlocks: make(map[BlockPointer]bool),
		deferredWrites: make(
			[]func(context.Context, *RootMetadata, path) error, 0),
		unrefCache:      make(map[BlockPointer]*syncInfo),
		deCache:         make(map[BlockPointer]map[BlockPointer]DirEntry),
		status:          newFolderBranchStatusKeeper(config, nodeCache),
		writerLock:      &sync.Mutex{},
		nodeCache:       nodeCache,
		state:           cleanState,
		log:             log,
		shutdownChan:    make(chan struct{}),
		updatePauseChan: make(chan (<-chan struct{})),
	}
	fbo.cr = NewConflictResolver(config, fbo)
	if config.DoBackgroundFlushes() {
		go fbo.backgroundFlusher(secondsBetweenBackgroundFlushes * time.Second)
	}
	return fbo
}

// Shutdown safely shuts down any background goroutines that may have
// been launched by FolderBranchOps.
func (fbo *FolderBranchOps) Shutdown() {
	close(fbo.shutdownChan)
	fbo.cr.Shutdown()
}

func (fbo *FolderBranchOps) id() TlfID {
	return fbo.folderBranch.Tlf
}

func (fbo *FolderBranchOps) branch() BranchName {
	return fbo.folderBranch.Branch
}

// GetFavorites implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) GetFavorites(ctx context.Context) ([]*Favorite, error) {
	return nil, errors.New("GetFavorites is not supported by FolderBranchOps")
}

func (fbo *FolderBranchOps) getState() state {
	fbo.stateLock.Lock()
	defer fbo.stateLock.Unlock()
	return fbo.state
}

// getStaged should not be called if writerLock is already taken.
func (fbo *FolderBranchOps) getStaged() bool {
	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()
	return fbo.staged
}

func (fbo *FolderBranchOps) transitionState(newState state) {
	fbo.stateLock.Lock()
	defer fbo.stateLock.Unlock()
	switch newState {
	case cleanState:
		if len(fbo.deCache) > 0 {
			// if we still have writes outstanding, don't allow the
			// transition into the clean state
			return
		}
	default:
		// no specific checks needed
	}
	fbo.state = newState
}

// The caller must hold writerLock.
func (fbo *FolderBranchOps) setStagedLocked(staged bool, bid BranchID) {
	fbo.staged = staged
	fbo.bid = bid
	if !staged {
		fbo.status.setCRChains(nil, nil)
	}
}

func (fbo *FolderBranchOps) checkDataVersion(p path, ptr BlockPointer) error {
	if ptr.DataVer < FirstValidDataVer {
		return InvalidDataVersionError{ptr.DataVer}
	}
	if ptr.DataVer > fbo.config.DataVersion() {
		return NewDataVersionError{p, ptr.DataVer}
	}
	return nil
}

// headLock must be taken by caller
func (fbo *FolderBranchOps) setHeadLocked(ctx context.Context,
	md *RootMetadata) error {
	isFirstHead := fbo.head == nil
	if !isFirstHead {
		mdID, err := md.MetadataID(fbo.config)
		if err != nil {
			return err
		}

		headID, err := fbo.head.MetadataID(fbo.config)
		if err != nil {
			return err
		}

		if headID == mdID {
			// only save this new MD if the MDID has changed
			return nil
		}
	}

	fbo.log.CDebugf(ctx, "Setting head revision to %d", md.Revision)
	err := fbo.config.MDCache().Put(md)
	if err != nil {
		return err
	}

	// If this is the first time the MD is being set, and we are
	// operating on unmerged data, initialize the state properly and
	// kick off conflict resolution.
	if isFirstHead && md.MergedStatus() == Unmerged {
		// no need to take the writer lock here since is the first
		// time the folder is being used
		fbo.setStagedLocked(true, md.BID)
		// Use uninitialized for the merged branch; the unmerged
		// revision is enough to trigger conflict resolution.
		fbo.cr.Resolve(md.Revision, MetadataRevisionUninitialized)
	}

	fbo.head = md
	fbo.status.setRootMetadata(md)
	if isFirstHead {
		// Start registering for updates right away, using this MD
		// as a starting point. For now only the master branch can
		// get updates
		if fbo.branch() == MasterBranch {
			go fbo.registerForUpdates()
		}
	}
	return nil
}

// if rtype == write, then writerLock must be taken
func (fbo *FolderBranchOps) getMDLocked(ctx context.Context, rtype reqType) (
	*RootMetadata, error) {
	fbo.headLock.RLock()
	if fbo.head != nil {
		fbo.headLock.RUnlock()
		return fbo.head, nil
	}
	fbo.headLock.RUnlock()

	// if we're in read mode, we can't safely fetch the new MD without
	// causing races, so bail
	if rtype == read {
		return nil, WriteNeededInReadRequest{}
	}

	// Not in cache, fetch from server and add to cache.  First, see
	// if this device has any unmerged commits -- take the latest one.
	mdops := fbo.config.MDOps()

	// get the head of the unmerged branch for this device (if any)
	md, err := mdops.GetUnmergedForTLF(ctx, fbo.id(), NullBranchID)
	if err != nil {
		return nil, err
	}
	if md == nil {
		// no unmerged MDs for this device, so just get the current head
		md, err = mdops.GetForTLF(ctx, fbo.id())
		if err != nil {
			return nil, err
		}
	}

	if md.data.Dir.Type != Dir {
		err = fbo.initMDLocked(ctx, md)
		if err != nil {
			return nil, err
		}
	} else {
		fbo.headLock.Lock()
		defer fbo.headLock.Unlock()
		err = fbo.setHeadLocked(ctx, md)
		if err != nil {
			return nil, err
		}
	}

	return md, err
}

// if rtype == write, then writerLock must be taken
func (fbo *FolderBranchOps) getMDForReadLocked(
	ctx context.Context, rtype reqType) (*RootMetadata, error) {
	md, err := fbo.getMDLocked(ctx, rtype)
	if err != nil {
		return nil, err
	}

	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return nil, err
	}
	if !md.GetTlfHandle().IsReader(uid) {
		return nil, NewReadAccessError(ctx, fbo.config, md.GetTlfHandle(), uid)
	}
	return md, nil
}

// writerLock must be taken by the caller.
func (fbo *FolderBranchOps) getMDForWriteLocked(ctx context.Context) (
	*RootMetadata, error) {
	md, err := fbo.getMDLocked(ctx, write)
	if err != nil {
		return nil, err
	}

	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return nil, err
	}
	if !md.GetTlfHandle().IsWriter(uid) {
		return nil,
			NewWriteAccessError(ctx, fbo.config, md.GetTlfHandle(), uid)
	}

	// Make a new successor of the current MD to hold the coming
	// writes.  The caller must pass this into syncBlockAndCheckEmbed
	// or the changes will be lost.
	newMd, err := md.MakeSuccessor(fbo.config)
	if err != nil {
		return nil, err
	}
	return &newMd, nil
}

func (fbo *FolderBranchOps) nowUnixNano() int64 {
	return fbo.config.Clock().Now().UnixNano()
}

// writerLock must be taken
func (fbo *FolderBranchOps) initMDLocked(
	ctx context.Context, md *RootMetadata) error {
	// create a dblock since one doesn't exist yet
	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return err
	}

	handle := md.GetTlfHandle()

	if !handle.IsWriter(uid) {
		return NewWriteAccessError(ctx, fbo.config, handle, uid)
	}

	newDblock := &DirBlock{
		Children: make(map[string]DirEntry),
	}

	var expectedKeyGen KeyGen
	if md.ID.IsPublic() {
		md.Writers = make([]keybase1.UID, len(handle.Writers))
		copy(md.Writers, handle.Writers)
		expectedKeyGen = PublicKeyGen
	} else {
		// create a new set of keys for this metadata
		if _, err := fbo.config.KeyManager().Rekey(ctx, md); err != nil {
			return err
		}
		expectedKeyGen = FirstValidKeyGen
	}
	keyGen := md.LatestKeyGeneration()
	if keyGen != expectedKeyGen {
		return InvalidKeyGenerationError{handle, keyGen}
	}
	info, plainSize, readyBlockData, err :=
		fbo.readyBlock(ctx, md, newDblock, uid)
	if err != nil {
		return err
	}

	now := fbo.nowUnixNano()
	md.data.Dir = DirEntry{
		BlockInfo: info,
		Type:      Dir,
		Size:      uint64(plainSize),
		Mtime:     now,
		Ctime:     now,
	}
	md.AddOp(newCreateOp("", BlockPointer{}, Dir))
	md.AddRefBlock(md.data.Dir.BlockInfo)
	md.UnrefBytes = 0

	// make sure we're a writer before putting any blocks
	if !handle.IsWriter(uid) {
		return NewWriteAccessError(ctx, fbo.config, handle, uid)
	}

	if err = fbo.config.BlockOps().Put(ctx, md, info.BlockPointer,
		readyBlockData); err != nil {
		return err
	}
	if err = fbo.config.BlockCache().Put(
		info.BlockPointer, fbo.id(), newDblock); err != nil {
		return err
	}

	// finally, write out the new metadata
	md.data.LastWriter = uid
	if err = fbo.config.MDOps().Put(ctx, md); err != nil {
		return err
	}

	fbo.headLock.Lock()
	defer fbo.headLock.Unlock()
	if fbo.head != nil {
		headID, _ := fbo.head.MetadataID(fbo.config)
		return fmt.Errorf(
			"%v: Unexpected MD ID during new MD initialization: %v",
			md.ID, headID)
	}
	err = fbo.setHeadLocked(ctx, md)
	if err != nil {
		return err
	}
	return nil
}

// GetOrCreateRootNodeForHandle implements the KBFSOps interface for
// FolderBranchOps
func (fbo *FolderBranchOps) GetOrCreateRootNodeForHandle(
	ctx context.Context, handle *TlfHandle, branch BranchName) (
	node Node, de DirEntry, err error) {
	err = errors.New("GetOrCreateRootNodeForHandle is not supported by " +
		"FolderBranchOps")
	return
}

func (fbo *FolderBranchOps) checkNode(node Node) error {
	fb := node.GetFolderBranch()
	if fb != fbo.folderBranch {
		return WrongOpsError{fbo.folderBranch, fb}
	}
	return nil
}

// CheckForNewMDAndInit sees whether the given MD object has been
// initialized yet; if not, it does so.
func (fbo *FolderBranchOps) CheckForNewMDAndInit(
	ctx context.Context, md *RootMetadata) (err error) {
	fbo.log.CDebugf(ctx, "CheckForNewMDAndInit, revision=%d (%s)",
		md.Revision, md.MergedStatus())
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	fb := FolderBranch{md.ID, MasterBranch}
	if fb != fbo.folderBranch {
		return WrongOpsError{fbo.folderBranch, fb}
	}

	if md.data.Dir.Type == Dir {
		// this MD is already initialized
		fbo.headLock.Lock()
		defer fbo.headLock.Unlock()
		// Only update the head the first time; later it will be
		// updated either directly via writes or through the
		// background update processor.
		if fbo.head == nil {
			err := fbo.setHeadLocked(ctx, md)
			if err != nil {
				return err
			}
		}
		return nil
	}

	// otherwise, intialize
	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()
	return fbo.initMDLocked(ctx, md)
}

// execReadThenWrite first tries to execute the passed-in method in
// read mode.  If it fails with a WriteNeededInReadRequest error, it
// re-executes the method as in write mode.  The passed-in method
// must note whether or not this is a write call.
func (fbo *FolderBranchOps) execReadThenWrite(f func(reqType) error) error {
	err := f(read)

	// Redo as a write request if needed
	if _, ok := err.(WriteNeededInReadRequest); ok {
		fbo.writerLock.Lock()
		defer fbo.writerLock.Unlock()
		err = f(write)
	}
	return err
}

// GetRootNode implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) GetRootNode(ctx context.Context,
	folderBranch FolderBranch) (
	node Node, de DirEntry, handle *TlfHandle, err error) {
	fbo.log.CDebugf(ctx, "GetRootNode")
	defer func() {
		if err != nil {
			fbo.log.CDebugf(ctx, "Error: %v", err)
		} else {
			fbo.log.CDebugf(ctx, "Done: %p", node.GetID())
		}
	}()

	if folderBranch != fbo.folderBranch {
		err = WrongOpsError{fbo.folderBranch, folderBranch}
		return
	}

	// don't check read permissions here -- anyone should be able to read
	// the MD to determine whether there's a public subdir or not
	var md *RootMetadata
	err = fbo.execReadThenWrite(func(rtype reqType) error {
		md, err = fbo.getMDLocked(ctx, rtype)
		return err
	})
	if err != nil {
		return
	}

	handle = md.GetTlfHandle()
	node, err = fbo.nodeCache.GetOrCreate(md.data.Dir.BlockPointer,
		handle.ToString(ctx, fbo.config), nil)
	if err != nil {
		node = nil
		return
	}
	de = md.Data().Dir
	return
}

type makeNewBlock func() Block

// blockLock should be taken for reading by the caller. dir must be
// valid.
func (fbo *FolderBranchOps) getBlockLocked(ctx context.Context,
	md *RootMetadata, dir path, newBlock makeNewBlock, rtype reqType) (
	Block, error) {
	if !dir.isValid() {
		return nil, InvalidPathError{}
	}
	bcache := fbo.config.BlockCache()
	if block, err := bcache.Get(dir.tailPointer(), dir.Branch); err == nil {
		return block, nil
	}

	// Unlock the blockLock while we wait for the network, only if
	// it's locked for reading.  If it's locked for writing, that
	// indicates we are performing an atomic write operation, and we
	// need to ensure that nothing else comes in and modifies the
	// blocks, so don't unlock.
	doLock := true
	if !fbo.blockWriteLocked {
		fbo.blockLock.RUnlock()
		defer func() {
			if doLock {
				fbo.blockLock.RLock()
			}
		}()
	}
	// TODO: add an optimization here that will avoid fetching the
	// same block twice from over the network

	// fetch the block, and add to cache
	block := newBlock()

	// if this is a file block, then send a read notification
	if _, ok := block.(*FileBlock); ok {
		fbo.config.Reporter().Notify(ctx, readNotification(dir, false))
		defer fbo.config.Reporter().Notify(ctx, readNotification(dir, true))
	}

	bops := fbo.config.BlockOps()
	if err := bops.Get(ctx, md, dir.tailPointer(), block); err != nil {
		return nil, err
	}

	// relock before accessing the cache
	doLock = false
	if !fbo.blockWriteLocked {
		fbo.blockLock.RLock()
	}
	if err := bcache.Put(dir.tailPointer(), fbo.id(), block); err != nil {
		return nil, err
	}
	return block, nil
}

func (fbo *FolderBranchOps) getBlockForReading(ctx context.Context,
	md *RootMetadata, dir path, newBlock makeNewBlock) (
	Block, error) {
	fbo.blockLock.RLock()
	defer fbo.blockLock.RUnlock()
	return fbo.getBlockLocked(ctx, md, dir, newBlock, read)
}

// getDirLocked returns the directory block at the given path.
// When rType == write and the cached version of the block is
// currently clean, this method makes a copy of the directory block
// and returns it.  If this method might be called again for the same
// block within a single operation, it is the caller's responsibility
// to write that block copy back to the cache as dirty.
//
// blockLock should be taken for reading by the caller, and writerLock
// too if rtype == write.
func (fbo *FolderBranchOps) getDirLocked(ctx context.Context, md *RootMetadata,
	dir path, rtype reqType) (*DirBlock, error) {
	// get the directory for the last element in the path
	block, err := fbo.getBlockLocked(ctx, md, dir, NewDirBlock, rtype)
	if err != nil {
		return nil, err
	}
	dblock, ok := block.(*DirBlock)
	if !ok {
		return nil, NotDirError{dir}
	}
	if rtype == write && !fbo.config.BlockCache().IsDirty(
		dir.tailPointer(), dir.Branch) {
		// copy the block if it's for writing
		dblock = dblock.DeepCopy()
	}
	return dblock, nil
}

// getFileLocked returns the file block at the given path.  When
// rType == write and the cached version of the block is currently
// clean, this method makes a copy of the file block and returns it.
// If this method might be called again for the same block within a
// single operation, it is the caller's responsibility to write that
// block back to the cache as dirty.
//
// blockLock should be taken for reading by the caller, and writerLock
// too if rtype == write.
func (fbo *FolderBranchOps) getFileLocked(ctx context.Context,
	md *RootMetadata, file path, rtype reqType) (*FileBlock, error) {
	// get the file for the last element in the path
	block, err := fbo.getBlockLocked(ctx, md, file, NewFileBlock, rtype)
	if err != nil {
		return nil, err
	}
	fblock, ok := block.(*FileBlock)
	if !ok {
		return nil, &NotFileError{file}
	}
	ptr := file.tailPointer()
	if rtype == write {
		// copy the block if it's for writing, and either the block is
		// not yet dirty or the block is currently being sync'd and
		// needs a copy even though it's already dirty
		if !fbo.config.BlockCache().IsDirty(ptr, file.Branch) ||
			fbo.copyFileBlocks[ptr] {
			fblock = fblock.DeepCopy()
		}
	}
	return fblock, nil
}

// stripBP removes the Writer from the BlockPointer, in case it
// changes as part of a write/truncate operation before the blocks are
// sync'd.
func stripBP(ptr BlockPointer) BlockPointer {
	return BlockPointer{
		ID:       ptr.ID,
		RefNonce: ptr.RefNonce,
		KeyGen:   ptr.KeyGen,
		DataVer:  ptr.DataVer,
		Creator:  ptr.Creator,
	}
}

func (fbo *FolderBranchOps) updateDirBlock(ctx context.Context,
	dir path, block *DirBlock) *DirBlock {
	// see if this directory has any outstanding writes/truncates that
	// require an updated DirEntry
	fbo.cacheLock.Lock()
	defer fbo.cacheLock.Unlock()
	deMap, ok := fbo.deCache[stripBP(dir.tailPointer())]
	if ok {
		// do a deep copy, replacing direntries as we go
		dblockCopy := NewDirBlock().(*DirBlock)
		*dblockCopy = *block
		dblockCopy.Children = make(map[string]DirEntry)
		for k, v := range block.Children {
			if de, ok := deMap[stripBP(v.BlockPointer)]; ok {
				// We have a local copy update to the block, so set
				// ourselves to be writer, if possible.  If there's an
				// error, just log it and keep going because having
				// the correct Writer is not important enough to fail
				// the whole lookup.
				uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
				if err != nil {
					fbo.log.CDebugf(ctx, "Ignoring error while getting "+
						"logged-in user during directory entry lookup: %v", err)
				} else {
					de.SetWriter(uid)
				}

				dblockCopy.Children[k] = de
			} else {
				dblockCopy.Children[k] = v
			}
		}
		return dblockCopy
	}
	return block
}

// GetDirChildren implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) GetDirChildren(ctx context.Context, dir Node) (
	children map[string]EntryType, err error) {
	fbo.log.CDebugf(ctx, "GetDirChildren %p", dir.GetID())
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(dir)
	if err != nil {
		return
	}

	md, err := fbo.getMDForReadLocked(ctx, read)
	if err != nil {
		return nil, err
	}

	fbo.blockLock.RLock()
	defer fbo.blockLock.RUnlock()
	dirPath := fbo.nodeCache.PathFromNode(dir)
	var block *DirBlock
	fbo.execReadThenWrite(func(rtype reqType) error {
		block, err = fbo.getDirLocked(ctx, md, dirPath, rtype)
		return err
	})
	if err != nil {
		return
	}

	children = make(map[string]EntryType)
	for k, de := range block.Children {
		children[k] = de.Type
	}
	return
}

// blockLocked must be taken for reading by the caller. file must have
// a valid parent.
func (fbo *FolderBranchOps) getEntryLocked(ctx context.Context,
	md *RootMetadata, file path) (*DirBlock, DirEntry, error) {
	if !file.hasValidParent() {
		return nil, DirEntry{}, InvalidPathError{}
	}

	parentPath := file.parentPath()
	dblock, err := fbo.getDirLocked(ctx, md, *parentPath, write)
	if err != nil {
		return nil, DirEntry{}, err
	}

	dblock = fbo.updateDirBlock(ctx, *parentPath, dblock)

	// make sure it exists
	name := file.tailName()
	de, ok := dblock.Children[name]
	if !ok {
		return nil, DirEntry{}, NoSuchNameError{name}
	}

	return dblock, de, err
}

func (fbo *FolderBranchOps) getEntry(ctx context.Context, md *RootMetadata,
	file path) (*DirBlock, DirEntry, error) {
	fbo.blockLock.RLock()
	defer fbo.blockLock.RUnlock()
	return fbo.getEntryLocked(ctx, md, file)
}

// Lookup implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) Lookup(ctx context.Context, dir Node, name string) (
	node Node, de DirEntry, err error) {
	fbo.log.CDebugf(ctx, "Lookup %p %s", dir.GetID(), name)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(dir)
	if err != nil {
		return
	}

	md, err := fbo.getMDForReadLocked(ctx, read)
	if err != nil {
		return
	}

	fbo.blockLock.RLock()
	defer fbo.blockLock.RUnlock()
	dirPath := fbo.nodeCache.PathFromNode(dir)
	childPath := *dirPath.ChildPathNoPtr(name)
	_, de, err = fbo.getEntryLocked(ctx, md, childPath)
	if err != nil {
		return
	}

	if de.Type == Sym {
		node = nil
	} else {
		err = fbo.checkDataVersion(childPath, de.BlockPointer)
		if err != nil {
			return
		}

		node, err = fbo.nodeCache.GetOrCreate(de.BlockPointer, name, dir)
	}
	return
}

// Stat implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) Stat(ctx context.Context, node Node) (
	de DirEntry, err error) {
	fbo.log.CDebugf(ctx, "Stat %p", node.GetID())
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(node)
	if err != nil {
		return
	}

	md, err := fbo.getMDForReadLocked(ctx, read)
	if err != nil {
		return
	}

	fbo.blockLock.RLock()
	defer fbo.blockLock.RUnlock()
	nodePath := fbo.nodeCache.PathFromNode(node)
	if !nodePath.isValid() {
		err = InvalidPathError{}
		return
	}

	if nodePath.hasValidParent() {
		_, de, err = fbo.getEntryLocked(ctx, md, nodePath)
	} else {
		// nodePath is just the root.
		de = md.data.Dir
	}
	return
}

var zeroPtr BlockPointer

type blockState struct {
	blockPtr       BlockPointer
	block          Block
	readyBlockData ReadyBlockData
}

// blockPutState is an internal structure to track data when putting blocks
type blockPutState struct {
	blockStates []blockState
}

func newBlockPutState(length int) *blockPutState {
	bps := &blockPutState{}
	bps.blockStates = make([]blockState, 0, length)
	return bps
}

func (bps *blockPutState) addNewBlock(blockPtr BlockPointer, block Block,
	readyBlockData ReadyBlockData) {
	bps.blockStates = append(bps.blockStates,
		blockState{blockPtr, block, readyBlockData})
}

func (bps *blockPutState) mergeOtherBps(other *blockPutState) {
	bps.blockStates = append(bps.blockStates, other.blockStates...)
}

func (fbo *FolderBranchOps) readyBlock(ctx context.Context, md *RootMetadata,
	block Block, uid keybase1.UID) (
	info BlockInfo, plainSize int, readyBlockData ReadyBlockData, err error) {
	var ptr BlockPointer
	if fBlock, ok := block.(*FileBlock); ok && !fBlock.IsInd {
		// first see if we are duplicating any known blocks in this folder
		ptr, err = fbo.config.BlockCache().CheckForKnownPtr(fbo.id(), fBlock)
		if err != nil {
			return
		}
	}

	// Ready the block, even in the case where we can reuse an
	// existing block, just so that we know what the size of the
	// encrypted data will be.
	id, plainSize, readyBlockData, err :=
		fbo.config.BlockOps().Ready(ctx, md, block)
	if err != nil {
		return
	}

	if ptr.IsInitialized() {
		ptr.RefNonce, err = fbo.config.Crypto().MakeBlockRefNonce()
		if err != nil {
			return
		}
		ptr.SetWriter(uid)
	} else {
		ptr = BlockPointer{
			ID:       id,
			KeyGen:   md.LatestKeyGeneration(),
			DataVer:  fbo.config.DataVersion(),
			Creator:  uid,
			RefNonce: zeroBlockRefNonce,
		}
	}

	info = BlockInfo{
		BlockPointer: ptr,
		EncodedSize:  uint32(readyBlockData.GetEncodedSize()),
	}
	return
}

func (fbo *FolderBranchOps) readyBlockMultiple(ctx context.Context,
	md *RootMetadata, currBlock Block, uid keybase1.UID, bps *blockPutState) (
	info BlockInfo, plainSize int, err error) {
	info, plainSize, readyBlockData, err :=
		fbo.readyBlock(ctx, md, currBlock, uid)
	if err != nil {
		return
	}

	bps.addNewBlock(info.BlockPointer, currBlock, readyBlockData)
	return
}

func (fbo *FolderBranchOps) unembedBlockChanges(
	ctx context.Context, bps *blockPutState, md *RootMetadata,
	changes *BlockChanges, uid keybase1.UID) (err error) {
	buf, err := fbo.config.Codec().Encode(changes)
	if err != nil {
		return
	}
	block := NewFileBlock().(*FileBlock)
	block.Contents = buf
	info, _, err := fbo.readyBlockMultiple(ctx, md, block, uid, bps)
	if err != nil {
		return
	}
	md.data.cachedChanges = *changes
	changes.Pointer = info.BlockPointer
	changes.Ops = nil
	md.RefBytes += uint64(info.EncodedSize)
	md.DiskUsage += uint64(info.EncodedSize)
	return
}

// cacheBlockIfNotYetDirtyLocked puts a block into the cache, but only
// does so if the block isn't already marked as dirty in the cache.
// This is useful when operating on a dirty copy of a block that may
// already be in the cache.
//
// blockLock should be taken by the caller for writing.
func (fbo *FolderBranchOps) cacheBlockIfNotYetDirtyLocked(
	ptr BlockPointer, branch BranchName, block Block) error {
	if !fbo.config.BlockCache().IsDirty(ptr, branch) {
		return fbo.config.BlockCache().PutDirty(ptr, branch, block)
	} else if fbo.copyFileBlocks[ptr] {
		fbo.copyFileBlocks[ptr] = false
		fbo.doDeferWrite = true
		// Overwrite the dirty block if this is a copy-on-write during
		// a sync.  Don't worry, the old dirty block is safe in the
		// sync goroutine (and also probably saved to the cache under
		// its new ID already.
		return fbo.config.BlockCache().PutDirty(ptr, branch, block)
	}
	return nil
}

type localBcache map[BlockPointer]*DirBlock

// syncBlock updates, and readies, the blocks along the path for the
// given write, up to the root of the tree or stopAt (if specified).
// When it updates the root of the tree, it also modifies the given
// head object with a new revision number and root block ID.  It first
// checks the provided lbc for blocks that may have been modified by
// previous syncBlock calls or the FS calls themselves.  It returns
// the updated path to the changed directory, the new or updated
// directory entry created as part of the call, and a summary of all
// the blocks that now must be put to the block server.
//
// entryType must not be Sym.
//
// TODO: deal with multiple nodes for indirect blocks
func (fbo *FolderBranchOps) syncBlock(ctx context.Context, uid keybase1.UID,
	md *RootMetadata, newBlock Block, dir path, name string,
	entryType EntryType, mtime bool, ctime bool, stopAt BlockPointer,
	lbc localBcache) (
	path, DirEntry, *blockPutState, error) {
	// now ready each dblock and write the DirEntry for the next one
	// in the path
	currBlock := newBlock
	currName := name
	newPath := path{
		FolderBranch: dir.FolderBranch,
		path:         make([]pathNode, 0, len(dir.path)),
	}
	bps := newBlockPutState(len(dir.path))
	refPath := *dir.ChildPathNoPtr(name)
	var newDe DirEntry
	doSetTime := true
	now := fbo.nowUnixNano()
	for len(newPath.path) < len(dir.path)+1 {
		info, plainSize, err :=
			fbo.readyBlockMultiple(ctx, md, currBlock, uid, bps)
		if err != nil {
			return path{}, DirEntry{}, nil, err
		}

		// prepend to path and setup next one
		newPath.path = append([]pathNode{{info.BlockPointer, currName}},
			newPath.path...)

		// get the parent block
		prevIdx := len(dir.path) - len(newPath.path)
		var prevDblock *DirBlock
		var de DirEntry
		var nextName string
		nextDoSetTime := false
		if prevIdx < 0 {
			// root dir, update the MD instead
			de = md.data.Dir
		} else {
			prevDir := path{
				FolderBranch: dir.FolderBranch,
				path:         dir.path[:prevIdx+1],
			}

			// First, check the localBcache, which could contain
			// blocks that were modified across multiple calls to
			// syncBlock.
			var ok bool
			prevDblock, ok = lbc[prevDir.tailPointer()]
			if !ok {
				prevDblock, err = func() (*DirBlock, error) {
					// If the block isn't in the local bcache, we have to
					// fetch it, possibly from the network.  Take
					// blockLock to make this safe, but we don't need to
					// hold it throughout the entire syncBlock execution
					// because we are only fetching directory blocks.
					// Directory blocks are only ever modified while
					// holding writerLock, so it's safe to release the
					// blockLock in between fetches.
					fbo.blockLock.RLock()
					defer fbo.blockLock.RUnlock()
					return fbo.getDirLocked(ctx, md, prevDir, write)
				}()
				if err != nil {
					return path{}, DirEntry{}, nil, err
				}
			}

			// modify the direntry for currName; make one
			// if it doesn't exist (which should only
			// happen the first time around).
			//
			// TODO: Pull the creation out of here and
			// into createEntryLocked().
			if de, ok = prevDblock.Children[currName]; !ok {
				// If this isn't the first time
				// around, we have an error.
				if len(newPath.path) > 1 {
					return path{}, DirEntry{}, nil, NoSuchNameError{currName}
				}

				// If this is a file, the size should be 0. (TODO:
				// Ensure this.) If this is a directory, the size will
				// be filled in below.  The times will be filled in
				// below as well, since we should only be creating a
				// new directory entry when doSetTime is true.
				de = DirEntry{
					Type: entryType,
					Size: 0,
				}
				// If we're creating a new directory entry, the
				// parent's times must be set as well.
				nextDoSetTime = true
			}

			currBlock = prevDblock
			nextName = prevDir.tailName()
		}

		if de.Type == Dir {
			// TODO: When we use indirect dir blocks,
			// we'll have to calculate the size some other
			// way.
			de.Size = uint64(plainSize)
		}

		if prevIdx < 0 {
			md.AddUpdate(md.data.Dir.BlockInfo, info)
		} else if prevDe, ok := prevDblock.Children[currName]; ok {
			md.AddUpdate(prevDe.BlockInfo, info)
		} else {
			// this is a new block
			md.AddRefBlock(info)
		}

		if len(refPath.path) > 1 {
			refPath = *refPath.parentPath()
		}
		de.BlockInfo = info

		if doSetTime {
			if mtime {
				de.Mtime = now
			}
			if ctime {
				de.Ctime = now
			}
		}
		if !newDe.IsInitialized() {
			newDe = de
		}

		if prevIdx < 0 {
			md.data.Dir = de
		} else {
			prevDblock.Children[currName] = de
		}
		currName = nextName

		// Stop before we get to the common ancestor; it will be taken care of
		// on the next sync call
		if prevIdx >= 0 && dir.path[prevIdx].BlockPointer == stopAt {
			// Put this back into the cache as dirty -- the next
			// syncBlock call will ready it.
			dblock, ok := currBlock.(*DirBlock)
			if !ok {
				return path{}, DirEntry{}, nil, BadDataError{stopAt.ID}
			}
			lbc[stopAt] = dblock
			break
		}
		doSetTime = nextDoSetTime
	}

	return newPath, newDe, bps, nil
}

// entryType must not be Sym.
func (fbo *FolderBranchOps) syncBlockAndCheckEmbed(ctx context.Context,
	md *RootMetadata, newBlock Block, dir path, name string,
	entryType EntryType, mtime bool, ctime bool, stopAt BlockPointer,
	lbc localBcache) (path, DirEntry, *blockPutState, error) {
	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return path{}, DirEntry{}, nil, err
	}

	newPath, newDe, bps, err := fbo.syncBlock(ctx, uid, md, newBlock,
		dir, name, entryType, mtime, ctime, stopAt, lbc)
	if err != nil {
		return path{}, DirEntry{}, nil, err
	}

	// do the block changes need their own blocks?
	bsplit := fbo.config.BlockSplitter()
	if !bsplit.ShouldEmbedBlockChanges(&md.data.Changes) {
		err = fbo.unembedBlockChanges(ctx, bps, md, &md.data.Changes,
			uid)
		if err != nil {
			return path{}, DirEntry{}, nil, err
		}
	}

	return newPath, newDe, bps, nil
}

func (fbo *FolderBranchOps) doOneBlockPut(ctx context.Context,
	md *RootMetadata, blockState blockState,
	errChan chan error) {
	err := fbo.config.BlockOps().
		Put(ctx, md, blockState.blockPtr, blockState.readyBlockData)
	if err != nil {
		// one error causes everything else to cancel
		select {
		case errChan <- err:
		default:
			return
		}
	}
}

// doBlockPuts writes all the pending block puts to the cache and
// server.
func (fbo *FolderBranchOps) doBlockPuts(ctx context.Context,
	md *RootMetadata, bps blockPutState) error {
	errChan := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	blocks := make(chan blockState, len(bps.blockStates))
	var wg sync.WaitGroup

	numWorkers := len(bps.blockStates)
	if numWorkers > maxParallelBlockPuts {
		numWorkers = maxParallelBlockPuts
	}
	wg.Add(numWorkers)

	worker := func() {
		defer wg.Done()
		for blockState := range blocks {
			fbo.doOneBlockPut(ctx, md, blockState, errChan)
			select {
			// return early if the context has been canceled
			case <-ctx.Done():
				return
			default:
			}
		}
	}
	for i := 0; i < numWorkers; i++ {
		go worker()
	}

	for _, blockState := range bps.blockStates {
		blocks <- blockState
	}
	close(blocks)

	go func() {
		wg.Wait()
		close(errChan)
	}()
	return <-errChan
}

// both writerLock and blockLocked should be taken by the caller
func (fbo *FolderBranchOps) finalizeBlocksLocked(bps *blockPutState) error {
	bcache := fbo.config.BlockCache()
	for _, blockState := range bps.blockStates {
		newPtr := blockState.blockPtr
		// only cache this block if we made a brand new block, not if
		// we just incref'd some other block.
		if !newPtr.IsFirstRef() {
			continue
		}
		if err := bcache.Put(newPtr, fbo.id(), blockState.block); err != nil {
			return err
		}
	}
	return nil
}

// Returns true if the passed error indicates a reviion conflict.
func (fbo *FolderBranchOps) isRevisionConflict(err error) bool {
	if err == nil {
		return false
	}
	_, isConflictRevision := err.(MDServerErrorConflictRevision)
	_, isConflictPrevRoot := err.(MDServerErrorConflictPrevRoot)
	_, isConflictDiskUsage := err.(MDServerErrorConflictDiskUsage)
	_, isConditionFailed := err.(MDServerErrorConditionFailed)
	return isConflictRevision || isConflictPrevRoot ||
		isConflictDiskUsage || isConditionFailed
}

// writerLock must be taken by the caller.
func (fbo *FolderBranchOps) finalizeWriteLocked(ctx context.Context,
	md *RootMetadata, bps *blockPutState) error {
	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return err
	}

	// finally, write out the new metadata
	md.data.LastWriter = uid
	mdops := fbo.config.MDOps()

	doUnmergedPut, wasStaged := true, fbo.staged
	mergedRev := MetadataRevisionUninitialized
	if !fbo.staged {
		// only do a normal Put if we're not already staged.
		err = mdops.Put(ctx, md)
		doUnmergedPut = fbo.isRevisionConflict(err)
		if err != nil && !doUnmergedPut {
			return err
		}
		// The first time we transition, our last known MD revision is
		// the same (at least) as what we thought our new revision
		// should be.  Otherwise, just leave it at uninitialized and
		// let the resolver sort it out.
		if doUnmergedPut {
			mergedRev = md.Revision
		}
	}

	if doUnmergedPut {
		// We're out of date, so put it as an unmerged MD.
		var bid BranchID
		if !wasStaged {
			// new branch ID
			crypto := fbo.config.Crypto()
			if bid, err = crypto.MakeRandomBranchID(); err != nil {
				return err
			}
		} else {
			bid = fbo.bid
		}
		err := mdops.PutUnmerged(ctx, md, bid)
		if err != nil {
			return nil
		}
		fbo.setStagedLocked(true, bid)
		fbo.cr.Resolve(md.Revision, mergedRev)
	} else {
		if fbo.staged {
			// If we were staged, prune all unmerged history now
			err = fbo.config.MDServer().PruneBranch(ctx, fbo.id(), fbo.bid)
			if err != nil {
				return err
			}
		}

		fbo.setStagedLocked(false, NullBranchID)
	}
	fbo.transitionState(cleanState)

	fbo.headLock.Lock()
	defer fbo.headLock.Unlock()

	// now take the blockLock, since we are potentially finalizing and
	// messing with old blocks
	fbo.blockLock.Lock()
	err = fbo.finalizeBlocksLocked(bps)
	fbo.blockLock.Unlock()
	if err != nil {
		return err
	}

	err = fbo.setHeadLocked(ctx, md)
	if err != nil {
		// XXX: if we return with an error here, should we somehow
		// roll back the nodeCache BlockPointer updates that happened
		// in finalizeBlocksLocked()?
		return err
	}

	fbo.notifyBatch(ctx, md)
	return nil
}

// writerLock must be taken by the caller, but not blockLock
func (fbo *FolderBranchOps) syncBlockAndFinalizeLocked(ctx context.Context,
	md *RootMetadata, newBlock Block, dir path, name string,
	entryType EntryType, mtime bool, ctime bool, stopAt BlockPointer) (
	DirEntry, error) {
	_, de, bps, err := fbo.syncBlockAndCheckEmbed(ctx, md, newBlock, dir,
		name, entryType, mtime, ctime, zeroPtr, nil)
	if err != nil {
		return DirEntry{}, err
	}
	err = fbo.doBlockPuts(ctx, md, *bps)
	if err != nil {
		// TODO: in theory we could recover from a
		// IncrementMissingBlockError.  We would have to delete the
		// offending block from our cache and re-doing ALL of the
		// block ready calls.
		return DirEntry{}, err
	}
	err = fbo.finalizeWriteLocked(ctx, md, bps)
	if err != nil {
		return DirEntry{}, err
	}
	return de, nil
}

// entryType must not by Sym.  writerLock must be taken by caller.
func (fbo *FolderBranchOps) createEntryLocked(
	ctx context.Context, dir Node, name string, entryType EntryType) (
	Node, DirEntry, error) {
	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx)
	if err != nil {
		return nil, DirEntry{}, err
	}

	fbo.blockLock.RLock()
	dirPath := fbo.nodeCache.PathFromNode(dir)
	dblock, err := fbo.getDirLocked(ctx, md, dirPath, write)
	if err != nil {
		fbo.blockLock.RUnlock()
		return nil, DirEntry{}, err
	}
	fbo.blockLock.RUnlock()

	// does name already exist?
	if _, ok := dblock.Children[name]; ok {
		return nil, DirEntry{}, NameExistsError{name}
	}

	md.AddOp(newCreateOp(name, dirPath.tailPointer(), entryType))
	// create new data block
	var newBlock Block
	// XXX: for now, put a unique ID in every new block, to make sure it
	// has a unique block ID. This may not be needed once we have encryption.
	if entryType == Dir {
		newBlock = &DirBlock{
			Children: make(map[string]DirEntry),
		}
	} else {
		newBlock = &FileBlock{}
	}

	de, err := fbo.syncBlockAndFinalizeLocked(ctx, md, newBlock, dirPath, name,
		entryType, true, true, zeroPtr)
	if err != nil {
		return nil, DirEntry{}, err
	}
	node, err := fbo.nodeCache.GetOrCreate(de.BlockPointer, name, dir)
	if err != nil {
		return nil, DirEntry{}, err
	}
	return node, de, nil
}

// CreateDir implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) CreateDir(
	ctx context.Context, dir Node, path string) (
	n Node, de DirEntry, err error) {
	fbo.log.CDebugf(ctx, "CreateDir %p %s", dir.GetID(), path)
	defer func() {
		if err != nil {
			fbo.log.CDebugf(ctx, "Error: %v", err)
		} else {
			fbo.log.CDebugf(ctx, "Done: %p", n.GetID())
		}
	}()

	err = fbo.checkNode(dir)
	if err != nil {
		return nil, DirEntry{}, err
	}

	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()
	return fbo.createEntryLocked(ctx, dir, path, Dir)
}

// CreateFile implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) CreateFile(
	ctx context.Context, dir Node, path string, isExec bool) (
	n Node, de DirEntry, err error) {
	fbo.log.CDebugf(ctx, "CreateFile %p %s", dir.GetID(), path)
	defer func() {
		if err != nil {
			fbo.log.CDebugf(ctx, "Error: %v", err)
		} else {
			fbo.log.CDebugf(ctx, "Done: %p", n.GetID())
		}
	}()

	err = fbo.checkNode(dir)
	if err != nil {
		return
	}

	var entryType EntryType
	if isExec {
		entryType = Exec
	} else {
		entryType = File
	}

	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()
	return fbo.createEntryLocked(ctx, dir, path, entryType)
}

// writerLock must be taken by caller.
func (fbo *FolderBranchOps) createLinkLocked(
	ctx context.Context, dir Node, fromName string, toPath string) (
	DirEntry, error) {
	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx)
	if err != nil {
		return DirEntry{}, err
	}

	fbo.blockLock.RLock()
	dirPath := fbo.nodeCache.PathFromNode(dir)
	dblock, err := fbo.getDirLocked(ctx, md, dirPath, write)
	if err != nil {
		fbo.blockLock.RUnlock()
		return DirEntry{}, err
	}
	fbo.blockLock.RUnlock()

	// TODO: validate inputs

	// does name already exist?
	if _, ok := dblock.Children[fromName]; ok {
		return DirEntry{}, NameExistsError{fromName}
	}

	md.AddOp(newCreateOp(fromName, dirPath.tailPointer(), Sym))

	// Create a direntry for the link, and then sync
	now := fbo.nowUnixNano()
	dblock.Children[fromName] = DirEntry{
		Type:    Sym,
		Size:    uint64(len(toPath)),
		SymPath: toPath,
		Mtime:   now,
		Ctime:   now,
	}

	_, err = fbo.syncBlockAndFinalizeLocked(
		ctx, md, dblock, *dirPath.parentPath(), dirPath.tailName(), Dir,
		true, true, zeroPtr)
	if err != nil {
		return DirEntry{}, err
	}
	return dblock.Children[fromName], nil
}

// CreateLink implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) CreateLink(
	ctx context.Context, dir Node, fromName string, toPath string) (
	de DirEntry, err error) {
	fbo.log.CDebugf(ctx, "CreateLink %p %s -> %s",
		dir.GetID(), fromName, toPath)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(dir)
	if err != nil {
		return DirEntry{}, err
	}

	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()
	return fbo.createLinkLocked(ctx, dir, fromName, toPath)
}

// unrefEntry modifies md to unreference all relevant blocks for the
// given entry.
func (fbo *FolderBranchOps) unrefEntry(ctx context.Context,
	md *RootMetadata, dir path, de DirEntry, name string) error {
	md.AddUnrefBlock(de.BlockInfo)
	// construct a path for the child so we can unlink with it.
	childPath := *dir.ChildPathNoPtr(name)
	childPath.path[len(childPath.path)-1].BlockPointer = de.BlockPointer

	// If this is an indirect block, we need to delete all of its
	// children as well. (TODO: handle multiple levels of
	// indirection.)  NOTE: non-empty directories can't be removed, so
	// no need to check for indirect directory blocks here.
	if de.Type == File || de.Type == Exec {
		block, err := func() (Block, error) {
			fbo.blockLock.RLock()
			defer fbo.blockLock.RUnlock()
			return fbo.getBlockLocked(ctx, md, childPath, NewFileBlock, write)
		}()
		if err != nil {
			return NoSuchBlockError{de.ID}
		}
		fBlock, ok := block.(*FileBlock)
		if !ok {
			return &NotFileError{dir}
		}
		if fBlock.IsInd {
			for _, ptr := range fBlock.IPtrs {
				md.AddUnrefBlock(ptr.BlockInfo)
			}
		}
	}
	return nil
}

// writerLock must be taken by caller.
func (fbo *FolderBranchOps) removeEntryLocked(ctx context.Context,
	md *RootMetadata, dir path, name string) error {
	pblock, err := func() (*DirBlock, error) {
		fbo.blockLock.RLock()
		defer fbo.blockLock.RUnlock()
		return fbo.getDirLocked(ctx, md, dir, write)
	}()
	if err != nil {
		return err
	}

	// make sure the entry exists
	de, ok := pblock.Children[name]
	if !ok {
		return NoSuchNameError{name}
	}

	md.AddOp(newRmOp(name, dir.tailPointer()))
	err = fbo.unrefEntry(ctx, md, dir, de, name)
	if err != nil {
		return err
	}

	// the actual unlink
	delete(pblock.Children, name)

	// sync the parent directory
	_, err = fbo.syncBlockAndFinalizeLocked(
		ctx, md, pblock, *dir.parentPath(), dir.tailName(),
		Dir, true, true, zeroPtr)
	if err != nil {
		return err
	}
	return nil
}

// RemoveDir implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) RemoveDir(
	ctx context.Context, dir Node, dirName string) (err error) {
	fbo.log.CDebugf(ctx, "RemoveDir %p %s", dir.GetID(), dirName)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(dir)
	if err != nil {
		return
	}

	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()

	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx)
	if err != nil {
		return err
	}

	dirPath := fbo.nodeCache.PathFromNode(dir)
	err = func() error {
		fbo.blockLock.RLock()
		defer fbo.blockLock.RUnlock()
		pblock, err := fbo.getDirLocked(ctx, md, dirPath, read)
		de, ok := pblock.Children[dirName]
		if !ok {
			return NoSuchNameError{dirName}
		}

		// construct a path for the child so we can check for an empty dir
		childPath := *dirPath.ChildPathNoPtr(dirName)
		childPath.path[len(childPath.path)-1].BlockPointer = de.BlockPointer

		childBlock, err := fbo.getDirLocked(ctx, md, childPath, read)
		if err != nil {
			return err
		}

		if len(childBlock.Children) > 0 {
			return DirNotEmptyError{dirName}
		}
		return nil
	}()
	if err != nil {
		return err
	}

	return fbo.removeEntryLocked(ctx, md, dirPath, dirName)
}

// RemoveEntry implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) RemoveEntry(ctx context.Context, dir Node,
	name string) (err error) {
	fbo.log.CDebugf(ctx, "RemoveEntry %p %s", dir.GetID(), name)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(dir)
	if err != nil {
		return err
	}

	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()

	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx)
	if err != nil {
		return err
	}

	dirPath := fbo.nodeCache.PathFromNode(dir)
	return fbo.removeEntryLocked(ctx, md, dirPath, name)
}

// writerLock must be taken by caller.
func (fbo *FolderBranchOps) renameLocked(
	ctx context.Context, oldParent path, oldName string, newParent path,
	newName string, newParentNode Node) error {
	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx)
	if err != nil {
		return err
	}

	doUnlock := true
	fbo.blockLock.RLock()
	defer func() {
		if doUnlock {
			fbo.blockLock.RUnlock()
		}
	}()

	// look up in the old path
	oldPBlock, err := fbo.getDirLocked(ctx, md, oldParent, write)
	if err != nil {
		return err
	}
	newDe, ok := oldPBlock.Children[oldName]
	// does the name exist?
	if !ok {
		return NoSuchNameError{oldName}
	}

	md.AddOp(newRenameOp(oldName, oldParent.tailPointer(), newName,
		newParent.tailPointer(), newDe.BlockPointer, newDe.Type))

	lbc := make(localBcache)
	// look up in the old path
	var newPBlock *DirBlock
	// TODO: Write a SameBlock() function that can deal properly with
	// dedup'd blocks that share an ID but can be updated separately.
	if oldParent.tailPointer().ID == newParent.tailPointer().ID {
		newPBlock = oldPBlock
	} else {
		newPBlock, err = fbo.getDirLocked(ctx, md, newParent, write)
		if err != nil {
			return err
		}
		now := fbo.nowUnixNano()

		oldGrandparent := *oldParent.parentPath()
		if len(oldGrandparent.path) > 0 {
			// Update the old parent's mtime/ctime, unless the
			// oldGrandparent is the same as newParent (in which case, the
			// syncBlockAndCheckEmbed call will take care of it).
			if oldGrandparent.tailPointer().ID != newParent.tailPointer().ID {
				b, err := fbo.getDirLocked(ctx, md, oldGrandparent, write)
				if err != nil {
					return err
				}
				if de, ok := b.Children[oldParent.tailName()]; ok {
					de.Ctime = now
					de.Mtime = now
					b.Children[oldParent.tailName()] = de
					// Put this block back into the local cache as dirty
					lbc[oldGrandparent.tailPointer()] = b
				}
			}
		} else {
			md.data.Dir.Ctime = now
			md.data.Dir.Mtime = now
		}
	}
	doUnlock = false
	fbo.blockLock.RUnlock()

	// does name exist?
	if de, ok := newPBlock.Children[newName]; ok {
		if de.Type == Dir {
			fbo.log.CWarningf(ctx, "Renaming over a directory (%s/%s) is not "+
				"allowed.", newParent, newName)
			return NotFileError{*newParent.ChildPathNoPtr(newName)}
		}

		// Delete the old block pointed to by this direntry.
		err := fbo.unrefEntry(ctx, md, newParent, de, newName)
		if err != nil {
			return err
		}
	}

	// only the ctime changes
	newDe.Ctime = fbo.nowUnixNano()
	newPBlock.Children[newName] = newDe
	delete(oldPBlock.Children, oldName)

	// find the common ancestor
	var i int
	found := false
	// the root block will always be the same, so start at number 1
	for i = 1; i < len(oldParent.path) && i < len(newParent.path); i++ {
		if oldParent.path[i].ID != newParent.path[i].ID {
			found = true
			i--
			break
		}
	}
	if !found {
		// if we couldn't find one, then the common ancestor is the
		// last node in the shorter path
		if len(oldParent.path) < len(newParent.path) {
			i = len(oldParent.path) - 1
		} else {
			i = len(newParent.path) - 1
		}
	}
	commonAncestor := oldParent.path[i].BlockPointer
	oldIsCommon := oldParent.tailPointer() == commonAncestor
	newIsCommon := newParent.tailPointer() == commonAncestor

	newOldPath := path{FolderBranch: oldParent.FolderBranch}
	var oldBps *blockPutState
	if oldIsCommon {
		if newIsCommon {
			// if old and new are both the common ancestor, there is
			// nothing to do (syncBlock will take care of everything)
		} else {
			// If the old one is common and the new one is not, then
			// the last syncBlockAndCheckEmbed call will need to access
			// the old one.
			lbc[oldParent.tailPointer()] = oldPBlock
		}
	} else {
		if newIsCommon {
			// If the new one is common, then the first
			// syncBlockAndCheckEmbed call will need to access it.
			lbc[newParent.tailPointer()] = newPBlock
		}

		// The old one is not the common ancestor, so we need to sync it.
		// TODO: optimize by pushing blocks from both paths in parallel
		newOldPath, _, oldBps, err = fbo.syncBlockAndCheckEmbed(
			ctx, md, oldPBlock, *oldParent.parentPath(), oldParent.tailName(),
			Dir, true, true, commonAncestor, lbc)
		if err != nil {
			return err
		}
	}

	newNewPath, _, newBps, err := fbo.syncBlockAndCheckEmbed(
		ctx, md, newPBlock, *newParent.parentPath(), newParent.tailName(),
		Dir, true, true, zeroPtr, lbc)
	if err != nil {
		return err
	}

	// newOldPath is really just a prefix now.  A copy is necessary as an
	// append could cause the new path to contain nodes from the old path.
	newOldPath.path = append(make([]pathNode, i+1, i+1), newOldPath.path...)
	copy(newOldPath.path[:i+1], newNewPath.path[:i+1])

	// merge and finalize the blockPutStates
	if oldBps != nil {
		newBps.mergeOtherBps(oldBps)
	}

	err = fbo.doBlockPuts(ctx, md, *newBps)
	if err != nil {
		return err
	}

	return fbo.finalizeWriteLocked(ctx, md, newBps)
}

// Rename implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) Rename(
	ctx context.Context, oldParent Node, oldName string, newParent Node,
	newName string) (err error) {
	fbo.log.CDebugf(ctx, "Rename %p/%s -> %p/%s", oldParent.GetID(),
		oldName, newParent.GetID(), newName)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(newParent)
	if err != nil {
		return err
	}

	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()

	oldParentPath := fbo.nodeCache.PathFromNode(oldParent)
	newParentPath := fbo.nodeCache.PathFromNode(newParent)

	// only works for paths within the same topdir
	if oldParentPath.FolderBranch != newParentPath.FolderBranch {
		return RenameAcrossDirsError{}
	}

	return fbo.renameLocked(ctx, oldParentPath, oldName, newParentPath,
		newName, newParent)
}

// blockLock must be taken for reading by caller.
func (fbo *FolderBranchOps) getFileBlockAtOffsetLocked(ctx context.Context,
	md *RootMetadata, file path, topBlock *FileBlock, off int64,
	rtype reqType) (ptr BlockPointer, parentBlock *FileBlock, indexInParent int,
	block *FileBlock, more bool, startOff int64, err error) {
	// find the block matching the offset, if it exists
	ptr = file.tailPointer()
	block = topBlock
	more = false
	startOff = 0
	// search until it's not an indirect block
	for block.IsInd {
		nextIndex := len(block.IPtrs) - 1
		for i, ptr := range block.IPtrs {
			if ptr.Off == off {
				// small optimization to avoid iterating past the right ptr
				nextIndex = i
				break
			} else if ptr.Off > off {
				// i can never be 0, because the first ptr always has
				// an offset at the beginning of the range
				nextIndex = i - 1
				break
			}
		}
		nextPtr := block.IPtrs[nextIndex]
		parentBlock = block
		indexInParent = nextIndex
		startOff = nextPtr.Off
		newPath := file
		// there is more to read if we ever took a path through a
		// ptr that wasn't the final ptr in its respective list
		more = more || (nextIndex != len(block.IPtrs)-1)
		ptr = nextPtr.BlockPointer
		newPath.path = append(newPath.path, pathNode{
			nextPtr.BlockPointer, file.tailName(),
		})
		if block, err = fbo.getFileLocked(ctx, md, newPath, rtype); err != nil {
			return
		}
	}

	return
}

// blockLock must be taken for reading by the caller
func (fbo *FolderBranchOps) readLocked(
	ctx context.Context, file path, dest []byte, off int64) (int64, error) {
	// verify we have permission to read
	md, err := fbo.getMDForReadLocked(ctx, read)
	if err != nil {
		return 0, err
	}

	// getFileLocked already checks read permissions
	fblock, err := fbo.getFileLocked(ctx, md, file, read)
	if err != nil {
		return 0, err
	}

	nRead := int64(0)
	n := int64(len(dest))

	for nRead < n {
		nextByte := nRead + off
		toRead := n - nRead
		_, _, _, block, _, startOff, err := fbo.getFileBlockAtOffsetLocked(
			ctx, md, file, fblock, nextByte, read)
		if err != nil {
			return 0, err
		}
		blockLen := int64(len(block.Contents))
		lastByteInBlock := startOff + blockLen

		if nextByte >= lastByteInBlock {
			return nRead, nil
		} else if toRead > lastByteInBlock-nextByte {
			toRead = lastByteInBlock - nextByte
		}

		firstByteToRead := nextByte - startOff
		copy(dest[nRead:nRead+toRead],
			block.Contents[firstByteToRead:toRead+firstByteToRead])
		nRead += toRead
	}

	return n, nil
}

// Read implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) Read(
	ctx context.Context, file Node, dest []byte, off int64) (
	n int64, err error) {
	fbo.log.CDebugf(ctx, "Read %p %d %d", file.GetID(), len(dest), off)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(file)
	if err != nil {
		return 0, err
	}

	fbo.blockLock.RLock()
	defer fbo.blockLock.RUnlock()
	filePath := fbo.nodeCache.PathFromNode(file)
	return fbo.readLocked(ctx, filePath, dest, off)
}

// blockLock must be taken by the caller.
func (fbo *FolderBranchOps) newRightBlockLocked(
	ctx context.Context, ptr BlockPointer, branch BranchName, pblock *FileBlock,
	off int64, md *RootMetadata) error {
	newRID, err := fbo.config.Crypto().MakeTemporaryBlockID()
	if err != nil {
		return err
	}
	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return err
	}
	rblock := &FileBlock{}

	pblock.IPtrs = append(pblock.IPtrs, IndirectFilePtr{
		BlockInfo: BlockInfo{
			BlockPointer: BlockPointer{
				ID:       newRID,
				KeyGen:   md.LatestKeyGeneration(),
				DataVer:  fbo.config.DataVersion(),
				Creator:  uid,
				RefNonce: zeroBlockRefNonce,
			},
			EncodedSize: 0,
		},
		Off: off,
	})

	if err := fbo.config.BlockCache().PutDirty(
		pblock.IPtrs[len(pblock.IPtrs)-1].BlockPointer,
		branch, rblock); err != nil {
		return err
	}

	if err = fbo.cacheBlockIfNotYetDirtyLocked(
		ptr, branch, pblock); err != nil {
		return err
	}
	return nil
}

// cacheLock must be taken by the caller
func (fbo *FolderBranchOps) getOrCreateSyncInfoLocked(de DirEntry) *syncInfo {
	ptr := stripBP(de.BlockPointer)
	si, ok := fbo.unrefCache[ptr]
	if !ok {
		si = &syncInfo{
			oldInfo: de.BlockInfo,
			op:      newSyncOp(de.BlockPointer),
		}
		fbo.unrefCache[ptr] = si
	}
	return si
}

// blockLock must be taken for writing by the caller.
func (fbo *FolderBranchOps) writeDataLocked(
	ctx context.Context, md *RootMetadata, file path, data []byte,
	off int64, doNotify bool) error {
	// check writer status explicitly
	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return err
	}
	if !md.GetTlfHandle().IsWriter(uid) {
		return NewWriteAccessError(ctx, fbo.config, md.GetTlfHandle(), uid)
	}

	fblock, err := fbo.getFileLocked(ctx, md, file, write)
	if err != nil {
		return err
	}

	bcache := fbo.config.BlockCache()
	bsplit := fbo.config.BlockSplitter()
	n := int64(len(data))
	nCopied := int64(0)

	_, de, err := fbo.getEntryLocked(ctx, md, file)
	if err != nil {
		return err
	}

	fbo.cacheLock.Lock()
	defer fbo.cacheLock.Unlock()
	si := fbo.getOrCreateSyncInfoLocked(de)
	for nCopied < n {
		ptr, parentBlock, indexInParent, block, more, startOff, err :=
			fbo.getFileBlockAtOffsetLocked(ctx, md, file, fblock,
				off+nCopied, write)
		if err != nil {
			return err
		}

		oldLen := len(block.Contents)
		nCopied += bsplit.CopyUntilSplit(block, !more, data[nCopied:],
			off+nCopied-startOff)

		// the block splitter could only have copied to the end of the
		// existing block (or appended to the end of the final block), so
		// we shouldn't ever hit this case:
		if more && oldLen < len(block.Contents) {
			return BadSplitError{}
		}

		// TODO: support multiple levels of indirection.  Right now the
		// code only does one but it should be straightforward to
		// generalize, just annoying

		// if we need another block but there are no more, then make one
		if nCopied < n && !more {
			// If the block doesn't already have a parent block, make one.
			if ptr == file.tailPointer() {
				// pick a new id for this block, and use this block's ID for
				// the parent
				newID, err := fbo.config.Crypto().MakeTemporaryBlockID()
				if err != nil {
					return err
				}
				fblock = &FileBlock{
					CommonBlock: CommonBlock{
						IsInd: true,
					},
					IPtrs: []IndirectFilePtr{
						{
							BlockInfo: BlockInfo{
								BlockPointer: BlockPointer{
									ID:       newID,
									KeyGen:   md.LatestKeyGeneration(),
									DataVer:  fbo.config.DataVersion(),
									Creator:  uid,
									RefNonce: zeroBlockRefNonce,
								},
								EncodedSize: 0,
							},
							Off: 0,
						},
					},
				}
				if err := bcache.PutDirty(
					file.tailPointer(), file.Branch, fblock); err != nil {
					return err
				}
				ptr = fblock.IPtrs[0].BlockPointer
			}

			// Make a new right block and update the parent's
			// indirect block list
			if err := fbo.newRightBlockLocked(ctx, file.tailPointer(),
				file.Branch, fblock,
				startOff+int64(len(block.Contents)), md); err != nil {
				return err
			}
		}

		if oldLen != len(block.Contents) || de.Writer != uid {
			de.EncodedSize = 0
			// update the file info
			de.Size += uint64(len(block.Contents) - oldLen)
			parentPtr := stripBP(file.parentPath().tailPointer())
			if _, ok := fbo.deCache[parentPtr]; !ok {
				fbo.deCache[parentPtr] = make(map[BlockPointer]DirEntry)
			}
			fbo.deCache[parentPtr][stripBP(file.tailPointer())] = de
		}

		if parentBlock != nil {
			// remember how many bytes it was
			si.unrefs = append(si.unrefs,
				parentBlock.IPtrs[indexInParent].BlockInfo)
			parentBlock.IPtrs[indexInParent].EncodedSize = 0
		}
		// keep the old block ID while it's dirty
		if err = fbo.cacheBlockIfNotYetDirtyLocked(ptr, file.Branch,
			block); err != nil {
			return err
		}
	}

	if fblock.IsInd {
		// Always make the top block dirty, so we will sync its
		// indirect blocks.  This has the added benefit of ensuring
		// that any write to a file while it's being sync'd will be
		// deferred, even if it's to a block that's not currently
		// being sync'd, since this top-most block will always be in
		// the copyFileBlocks set.
		if err = fbo.cacheBlockIfNotYetDirtyLocked(
			file.tailPointer(), file.Branch, fblock); err != nil {
			return err
		}
	}
	si.op.addWrite(uint64(off), uint64(len(data)))

	if doNotify {
		fbo.notifyLocal(ctx, file, si.op)
	}
	fbo.transitionState(dirtyState)
	return nil
}

// Write implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) Write(
	ctx context.Context, file Node, data []byte, off int64) (err error) {
	fbo.log.CDebugf(ctx, "Write %p %d %d", file.GetID(), len(data), off)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(file)
	if err != nil {
		return err
	}

	// Get the MD for reading.  We won't modify it; we'll track the
	// unref changes on the side, and put them into the MD during the
	// sync.
	md, err := fbo.getMDLocked(ctx, read)
	if err != nil {
		return err
	}

	fbo.blockLock.Lock()
	defer fbo.blockLock.Unlock()
	filePath := fbo.nodeCache.PathFromNode(file)
	fbo.blockWriteLocked = true
	defer func() {
		fbo.blockWriteLocked = false
		fbo.doDeferWrite = false
	}()

	err = fbo.writeDataLocked(ctx, md, filePath, data, off, true)
	if err != nil {
		return err
	}

	if fbo.doDeferWrite {
		// There's an ongoing sync, and this write altered dirty
		// blocks that are in the process of syncing.  So, we have to
		// redo this write once the sync is complete, using the new
		// file path.
		//
		// There is probably a less terrible of doing this that
		// doesn't involve so much copying and rewriting, but this is
		// the most obviously correct way.
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)
		fbo.deferredWrites = append(fbo.deferredWrites,
			func(ctx context.Context, rmd *RootMetadata, f path) error {
				return fbo.writeDataLocked(
					ctx, md, f, dataCopy, off, false)
			})
	}

	fbo.status.addDirtyNode(file)
	return nil
}

// blockLocked must be held for writing by the caller
func (fbo *FolderBranchOps) truncateLocked(
	ctx context.Context, md *RootMetadata, file path, size uint64,
	doNotify bool) error {
	// check writer status explicitly
	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return err
	}
	if !md.GetTlfHandle().IsWriter(uid) {
		return NewWriteAccessError(ctx, fbo.config, md.GetTlfHandle(), uid)
	}

	fblock, err := fbo.getFileLocked(ctx, md, file, write)
	if err != nil {
		return err
	}

	// find the block where the file should now end
	iSize := int64(size) // TODO: deal with overflow
	ptr, parentBlock, indexInParent, block, more, startOff, err :=
		fbo.getFileBlockAtOffsetLocked(ctx, md, file, fblock, iSize, write)

	currLen := int64(startOff) + int64(len(block.Contents))
	if currLen < iSize {
		// if we need to extend the file, let's just do a write
		moreNeeded := iSize - currLen
		return fbo.writeDataLocked(ctx, md, file, make([]byte, moreNeeded,
			moreNeeded), currLen, doNotify)
	} else if currLen == iSize {
		// same size!
		return nil
	}

	// update the local entry size
	_, de, err := fbo.getEntryLocked(ctx, md, file)
	if err != nil {
		return err
	}

	// otherwise, we need to delete some data (and possibly entire blocks)
	block.Contents = append([]byte(nil), block.Contents[:iSize-startOff]...)
	fbo.cacheLock.Lock()
	doCacheUnlock := true
	defer func() {
		if doCacheUnlock {
			fbo.cacheLock.Unlock()
		}
	}()

	si := fbo.getOrCreateSyncInfoLocked(de)
	if more {
		// TODO: if indexInParent == 0, we can remove the level of indirection
		for _, ptr := range parentBlock.IPtrs[indexInParent+1:] {
			si.unrefs = append(si.unrefs, ptr.BlockInfo)
		}
		parentBlock.IPtrs = parentBlock.IPtrs[:indexInParent+1]
		// always make the parent block dirty, so we will sync it
		if err = fbo.cacheBlockIfNotYetDirtyLocked(
			file.tailPointer(), file.Branch, parentBlock); err != nil {
			return err
		}
	}

	if fblock.IsInd {
		// Always make the top block dirty, so we will sync its
		// indirect blocks.  This has the added benefit of ensuring
		// that any truncate to a file while it's being sync'd will be
		// deferred, even if it's to a block that's not currently
		// being sync'd, since this top-most block will always be in
		// the copyFileBlocks set.
		if err = fbo.cacheBlockIfNotYetDirtyLocked(
			file.tailPointer(), file.Branch, fblock); err != nil {
			return err
		}
	}

	if parentBlock != nil {
		// TODO: When we implement more than one level of indirection,
		// make sure that the pointer to parentBlock in the grandparent block
		// has EncodedSize 0.
		si.unrefs = append(si.unrefs,
			parentBlock.IPtrs[indexInParent].BlockInfo)
		parentBlock.IPtrs[indexInParent].EncodedSize = 0
	}

	doCacheUnlock = false
	si.op.addTruncate(size)
	fbo.cacheLock.Unlock()

	de.EncodedSize = 0
	de.Size = size
	parentPtr := stripBP(file.parentPath().tailPointer())
	if _, ok := fbo.deCache[parentPtr]; !ok {
		fbo.deCache[parentPtr] = make(map[BlockPointer]DirEntry)
	}
	fbo.deCache[parentPtr][stripBP(file.tailPointer())] = de

	// Keep the old block ID while it's dirty.
	if err = fbo.cacheBlockIfNotYetDirtyLocked(
		ptr, file.Branch, block); err != nil {
		return err
	}

	if doNotify {
		fbo.notifyLocal(ctx, file, si.op)
	}
	fbo.transitionState(dirtyState)
	return nil
}

// Truncate implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) Truncate(
	ctx context.Context, file Node, size uint64) (err error) {
	fbo.log.CDebugf(ctx, "Truncate %p %d", file.GetID(), size)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(file)
	if err != nil {
		return err
	}

	// Get the MD for reading.  We won't modify it; we'll track the
	// unref changes on the side, and put them into the MD during the
	// sync.
	md, err := fbo.getMDLocked(ctx, read)
	if err != nil {
		return err
	}

	fbo.blockLock.Lock()
	defer fbo.blockLock.Unlock()
	filePath := fbo.nodeCache.PathFromNode(file)
	fbo.blockWriteLocked = true
	defer func() {
		fbo.blockWriteLocked = false
		fbo.doDeferWrite = false
	}()

	err = fbo.truncateLocked(ctx, md, filePath, size, true)
	if err != nil {
		return err
	}

	if fbo.doDeferWrite {
		// There's an ongoing sync, and this truncate altered
		// dirty blocks that are in the process of syncing.  So,
		// we have to redo this truncate once the sync is complete,
		// using the new file path.
		fbo.deferredWrites = append(fbo.deferredWrites,
			func(ctx context.Context, rmd *RootMetadata, f path) error {
				return fbo.truncateLocked(ctx, md, f, size, false)
			})
	}

	fbo.status.addDirtyNode(file)
	return nil
}

// writerLock must be taken by caller.
func (fbo *FolderBranchOps) setExLocked(
	ctx context.Context, file path, ex bool) (err error) {
	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx)
	if err != nil {
		return
	}

	fbo.blockLock.RLock()
	dblock, de, err := fbo.getEntryLocked(ctx, md, file)
	if err != nil {
		fbo.blockLock.RUnlock()
		return
	}
	fbo.blockLock.RUnlock()

	// If the file is a symlink, do nothing (to match ext4
	// behavior).
	if de.Type == Sym {
		return
	}

	if ex && (de.Type == File) {
		de.Type = Exec
	} else if !ex && (de.Type == Exec) {
		de.Type = File
	}

	parentPath := file.parentPath()
	md.AddOp(newSetAttrOp(file.tailName(), parentPath.tailPointer(), exAttr,
		file.tailPointer()))

	// If the type isn't File or Exec, there's nothing to do, but
	// change the ctime anyway (to match ext4 behavior).
	de.Ctime = fbo.nowUnixNano()
	dblock.Children[file.tailName()] = de
	_, err = fbo.syncBlockAndFinalizeLocked(
		ctx, md, dblock, *parentPath.parentPath(), parentPath.tailName(),
		Dir, false, false, zeroPtr)
	return err
}

// SetEx implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) SetEx(
	ctx context.Context, file Node, ex bool) (err error) {
	fbo.log.CDebugf(ctx, "SetEx %p %t", file.GetID(), ex)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(file)
	if err != nil {
		return
	}

	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()
	filePath := fbo.nodeCache.PathFromNode(file)
	return fbo.setExLocked(ctx, filePath, ex)
}

// writerLock must be taken by caller.
func (fbo *FolderBranchOps) setMtimeLocked(
	ctx context.Context, file path, mtime *time.Time) error {
	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx)
	if err != nil {
		return err
	}

	fbo.blockLock.RLock()
	dblock, de, err := fbo.getEntryLocked(ctx, md, file)
	if err != nil {
		fbo.blockLock.RUnlock()
		return err
	}
	fbo.blockLock.RUnlock()

	parentPath := file.parentPath()
	md.AddOp(newSetAttrOp(file.tailName(), parentPath.tailPointer(), mtimeAttr,
		file.tailPointer()))

	de.Mtime = mtime.UnixNano()
	// setting the mtime counts as changing the file MD, so must set ctime too
	de.Ctime = fbo.nowUnixNano()
	dblock.Children[file.tailName()] = de
	_, err = fbo.syncBlockAndFinalizeLocked(
		ctx, md, dblock, *parentPath.parentPath(), parentPath.tailName(),
		Dir, false, false, zeroPtr)
	return err
}

// SetMtime implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) SetMtime(
	ctx context.Context, file Node, mtime *time.Time) (err error) {
	fbo.log.CDebugf(ctx, "SetMtime %p %v", file.GetID(), mtime)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	if mtime == nil {
		// Can happen on some OSes (e.g. OSX) when trying to set the atime only
		return nil
	}

	err = fbo.checkNode(file)
	if err != nil {
		return
	}

	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()
	filePath := fbo.nodeCache.PathFromNode(file)
	return fbo.setMtimeLocked(ctx, filePath, mtime)
}

// cacheLock should be taken by the caller
func (fbo *FolderBranchOps) mergeUnrefCacheLocked(file path, md *RootMetadata) {
	filePtr := stripBP(file.tailPointer())
	for _, info := range fbo.unrefCache[filePtr].unrefs {
		// it's ok if we push the same ptr.ID/RefNonce multiple times,
		// because the subsequent ones should have a QuotaSize of 0.
		md.AddUnrefBlock(info)
	}
}

// writerLock must be taken by the caller.
func (fbo *FolderBranchOps) syncLocked(ctx context.Context, file path) (
	stillDirty bool, err error) {
	// if the cache for this file isn't dirty, we're done
	fbo.blockLock.RLock()
	bcache := fbo.config.BlockCache()
	if !bcache.IsDirty(file.tailPointer(), file.Branch) {
		fbo.blockLock.RUnlock()
		return false, nil
	}
	fbo.blockLock.RUnlock()

	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx)
	if err != nil {
		return true, err
	}

	// If the MD doesn't match the MD expected by the path, that
	// implies we are using a cached path, which implies the node has
	// been unlinked.  In that case, we can safely ignore this sync.
	if md.data.Dir.BlockPointer != file.path[0].BlockPointer {
		return true, nil
	}

	doUnlock := true
	fbo.blockLock.RLock()
	defer func() {
		if doUnlock {
			fbo.blockLock.RUnlock()
		}
	}()

	// notify the daemon that a write is being performed
	fbo.config.Reporter().Notify(ctx, writeNotification(file, false))
	defer fbo.config.Reporter().Notify(ctx, writeNotification(file, true))

	// update the parent directories, and write all the new blocks out
	// to disk
	fblock, err := fbo.getFileLocked(ctx, md, file, write)
	if err != nil {
		return true, err
	}

	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return true, err
	}

	bps := newBlockPutState(1)
	filePtr := stripBP(file.tailPointer())
	si, ok := func() (*syncInfo, bool) {
		fbo.cacheLock.Lock()
		defer fbo.cacheLock.Unlock()
		si, ok := fbo.unrefCache[filePtr]
		return si, ok
	}()
	if !ok {
		return true, fmt.Errorf("No syncOp found for file pointer %v", filePtr)
	}
	md.AddOp(si.op)
	// Note: below we add possibly updated file blocks as "unref" and
	// "ref" blocks.  This is fine, since conflict resolution or
	// notifications will never happen within a file.

	// if this is an indirect block:
	//   1) check if each dirty block is split at the right place.
	//   2) if it needs fewer bytes, prepend the extra bytes to the next
	//      block (making a new one if it doesn't exist), and the next block
	//      gets marked dirty
	//   3) if it needs more bytes, then use copyUntilSplit() to fetch bytes
	//      from the next block (if there is one), remove the copied bytes
	//      from the next block and mark it dirty
	//   4) Then go through once more, and ready and finalize each
	//      dirty block, updating its ID in the indirect pointer list
	bsplit := fbo.config.BlockSplitter()
	var deferredDirtyDeletes []func() error
	if fblock.IsInd {
		for i := 0; i < len(fblock.IPtrs); i++ {
			ptr := fblock.IPtrs[i]
			isDirty := bcache.IsDirty(ptr.BlockPointer, file.Branch)
			if (ptr.EncodedSize > 0) && isDirty {
				return true, InconsistentEncodedSizeError{ptr.BlockInfo}
			}
			if isDirty {
				_, _, _, block, more, _, err :=
					fbo.getFileBlockAtOffsetLocked(ctx, md, file, fblock,
						ptr.Off, write)
				if err != nil {
					return true, err
				}

				splitAt := bsplit.CheckSplit(block)
				switch {
				case splitAt == 0:
					continue
				case splitAt > 0:
					endOfBlock := ptr.Off + int64(len(block.Contents))
					extraBytes := block.Contents[splitAt:]
					block.Contents = block.Contents[:splitAt]
					// put the extra bytes in front of the next block
					if !more {
						// need to make a new block
						if err := fbo.newRightBlockLocked(
							ctx, file.tailPointer(), file.Branch, fblock,
							endOfBlock, md); err != nil {
							return true, err
						}
					}
					rPtr, _, _, rblock, _, _, err :=
						fbo.getFileBlockAtOffsetLocked(ctx, md, file, fblock,
							endOfBlock, write)
					if err != nil {
						return true, err
					}
					rblock.Contents = append(extraBytes, rblock.Contents...)
					if err = fbo.cacheBlockIfNotYetDirtyLocked(
						rPtr, file.Branch, rblock); err != nil {
						return true, err
					}
					fblock.IPtrs[i+1].Off = ptr.Off + int64(len(block.Contents))
					md.AddUnrefBlock(fblock.IPtrs[i+1].BlockInfo)
					fblock.IPtrs[i+1].EncodedSize = 0
				case splitAt < 0:
					if !more {
						// end of the line
						continue
					}

					endOfBlock := ptr.Off + int64(len(block.Contents))
					rPtr, _, _, rblock, _, _, err :=
						fbo.getFileBlockAtOffsetLocked(ctx, md, file, fblock,
							endOfBlock, write)
					if err != nil {
						return true, err
					}
					// copy some of that block's data into this block
					nCopied := bsplit.CopyUntilSplit(block, false,
						rblock.Contents, int64(len(block.Contents)))
					rblock.Contents = rblock.Contents[nCopied:]
					if len(rblock.Contents) > 0 {
						if err = fbo.cacheBlockIfNotYetDirtyLocked(
							rPtr, file.Branch, rblock); err != nil {
							return true, err
						}
						fblock.IPtrs[i+1].Off =
							ptr.Off + int64(len(block.Contents))
						md.AddUnrefBlock(fblock.IPtrs[i+1].BlockInfo)
						fblock.IPtrs[i+1].EncodedSize = 0
					} else {
						// TODO: delete the block, and if we're down
						// to just one indirect block, remove the
						// layer of indirection
						//
						// TODO: When we implement more than one level
						// of indirection, make sure that the pointer
						// to the parent block in the grandparent
						// block has EncodedSize 0.
						md.AddUnrefBlock(fblock.IPtrs[i+1].BlockInfo)
						fblock.IPtrs =
							append(fblock.IPtrs[:i+1], fblock.IPtrs[i+2:]...)
					}
				}
			}
		}

		for i, ptr := range fblock.IPtrs {
			isDirty := bcache.IsDirty(ptr.BlockPointer, file.Branch)
			if (ptr.EncodedSize > 0) && isDirty {
				return true, &InconsistentEncodedSizeError{ptr.BlockInfo}
			}
			if isDirty {
				_, _, _, block, _, _, err := fbo.getFileBlockAtOffsetLocked(
					ctx, md, file, fblock, ptr.Off, write)
				if err != nil {
					return true, err
				}

				newInfo, _, readyBlockData, err :=
					fbo.readyBlock(ctx, md, block, uid)
				if err != nil {
					return true, err
				}

				// Defer the DeleteDirty until after the new path is
				// ready, in case anyone tries to read the dirty file
				// in the meantime.
				localPtr := ptr.BlockPointer
				deferredDirtyDeletes =
					append(deferredDirtyDeletes, func() error {
						return bcache.DeleteDirty(localPtr, file.Branch)
					})

				fblock.IPtrs[i].BlockInfo = newInfo
				md.AddRefBlock(newInfo)
				bps.addNewBlock(newInfo.BlockPointer, block, readyBlockData)
				fbo.copyFileBlocks[localPtr] = true
			}
		}
	}

	fbo.copyFileBlocks[file.tailPointer()] = true

	parentPath := file.parentPath()
	dblock, err := fbo.getDirLocked(ctx, md, *parentPath, write)
	if err != nil {
		return true, err
	}
	lbc := make(localBcache)

	// add in the cached unref pieces and fixup the dir entry
	fbo.cacheLock.Lock()
	fbo.mergeUnrefCacheLocked(file, md)

	// update the file's directory entry to the cached copy
	parentPtr := stripBP(parentPath.tailPointer())
	doDeleteDe := false
	if deMap, ok := fbo.deCache[parentPtr]; ok {
		if de, ok := deMap[filePtr]; ok {
			// remember the old info
			de.EncodedSize = si.oldInfo.EncodedSize
			dblock.Children[file.tailName()] = de
			lbc[parentPath.tailPointer()] = dblock
			doDeleteDe = true
			delete(deMap, filePtr)
			if len(deMap) == 0 {
				delete(fbo.deCache, parentPtr)
			} else {
				fbo.deCache[parentPtr] = deMap
			}
		}
	}
	fbo.cacheLock.Unlock()

	doUnlock = false
	fbo.blockLock.RUnlock()

	newPath, _, newBps, err :=
		fbo.syncBlockAndCheckEmbed(ctx, md, fblock, *parentPath,
			file.tailName(), File, true, true, zeroPtr, lbc)
	if err != nil {
		return true, err
	}
	newBps.mergeOtherBps(bps)

	err = fbo.doBlockPuts(ctx, md, *newBps)
	if err != nil {
		return true, err
	}

	deferredDirtyDeletes = append(deferredDirtyDeletes, func() error {
		return bcache.DeleteDirty(file.tailPointer(), file.Branch)
	})

	err = fbo.finalizeWriteLocked(ctx, md, newBps)
	if err != nil {
		return true, err
	}

	fbo.blockLock.Lock()
	defer fbo.blockLock.Unlock()
	err = func() error {
		fbo.cacheLock.Lock()
		defer fbo.cacheLock.Unlock()
		for _, f := range deferredDirtyDeletes {
			// This will also clear any dirty blocks that resulted from a
			// write/truncate happening during the sync.  But that's ok,
			// because we will redo them below.
			err = f()
			if err != nil {
				return err
			}
		}

		// Clear the updated de from the cache.  We are guaranteed that
		// any concurrent write to this file was deferred, even if it was
		// to a block that wasn't currently being sync'd, since the
		// top-most block is always in copyFileBlocks and is always
		// dirtied during a write/truncate.
		if doDeleteDe {
			deMap := fbo.deCache[parentPtr]
			delete(deMap, filePtr)
			if len(deMap) == 0 {
				delete(fbo.deCache, parentPtr)
			} else {
				fbo.deCache[parentPtr] = deMap
			}
		}

		// we can get rid of all the sync state that might have
		// happened during the sync, since we will replay the writes
		// below anyway.
		delete(fbo.unrefCache, filePtr)
		return nil
	}()
	if err != nil {
		return true, err
	}

	fbo.copyFileBlocks = make(map[BlockPointer]bool)
	// Redo any writes or truncates that happened to our file while
	// the sync was happening.
	writes := fbo.deferredWrites
	stillDirty = len(fbo.deferredWrites) != 0
	fbo.deferredWrites = nil
	for _, f := range writes {
		// we can safely read head here because we hold writerLock
		err = f(ctx, fbo.head, newPath)
		if err != nil {
			// It's a little weird to return an error from a deferred
			// write here. Hopefully that will never happen.
			return true, err
		}
	}

	return stillDirty, nil
}

// Sync implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) Sync(ctx context.Context, file Node) (err error) {
	fbo.log.CDebugf(ctx, "Sync %p", file.GetID())
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(file)
	if err != nil {
		return
	}

	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()
	filePath := fbo.nodeCache.PathFromNode(file)
	stillDirty, err := fbo.syncLocked(ctx, filePath)
	if err != nil {
		return err
	}

	if !stillDirty {
		fbo.status.rmDirtyNode(file)
	}
	return nil
}

// Status implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) Status(
	ctx context.Context, folderBranch FolderBranch) (
	fbs FolderBranchStatus, updateChan <-chan StatusUpdate, err error) {
	fbo.log.CDebugf(ctx, "Status")
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	if folderBranch != fbo.folderBranch {
		return FolderBranchStatus{}, nil,
			WrongOpsError{fbo.folderBranch, folderBranch}
	}

	// Wait for conflict resolution to settle down, if necessary.
	fbo.cr.Wait(ctx)

	return fbo.status.getStatus(ctx)
}

// RegisterForChanges registers a single Observer to receive
// notifications about this folder/branch.
func (fbo *FolderBranchOps) RegisterForChanges(obs Observer) error {
	fbo.obsLock.Lock()
	defer fbo.obsLock.Unlock()
	// It's the caller's responsibility to make sure
	// RegisterForChanges isn't called twice for the same Observer
	fbo.observers = append(fbo.observers, obs)
	return nil
}

// UnregisterFromChanges stops an Observer from getting notifications
// about the folder/branch.
func (fbo *FolderBranchOps) UnregisterFromChanges(obs Observer) error {
	fbo.obsLock.Lock()
	defer fbo.obsLock.Unlock()
	for i, oldObs := range fbo.observers {
		if oldObs == obs {
			fbo.observers = append(fbo.observers[:i], fbo.observers[i+1:]...)
			break
		}
	}
	return nil
}

func (fbo *FolderBranchOps) notifyLocal(ctx context.Context,
	file path, so *syncOp) {
	node := fbo.nodeCache.Get(file.tailPointer())
	if node == nil {
		return
	}
	// notify about the most recent write op
	write := so.Writes[len(so.Writes)-1]

	fbo.obsLock.RLock()
	defer fbo.obsLock.RUnlock()
	for _, obs := range fbo.observers {
		obs.LocalChange(ctx, node, write)
	}
}

// notifyBatch sends out a notification for the most recent op in md
func (fbo *FolderBranchOps) notifyBatch(ctx context.Context, md *RootMetadata) {
	var lastOp op
	if md.data.Changes.Ops != nil {
		lastOp = md.data.Changes.Ops[len(md.data.Changes.Ops)-1]
	} else {
		// Uh-oh, the block changes have been kicked out into a block.
		// Use a cached copy instead, and clear it when done.
		lastOp = md.data.cachedChanges.Ops[len(md.data.cachedChanges.Ops)-1]
		md.data.cachedChanges.Ops = nil
	}

	fbo.notifyOneOp(ctx, lastOp, md)
}

// searchForNodesInDirLocked recursively tries to find a path, and
// ultimately a node, to ptr, given the set of pointers that were
// updated in a particular operation.  The keys in nodeMap make up the
// set of BlockPointers that are being searched for, and nodeMap is
// updated in place to include the corresponding discovered nodes.
//
// Returns the number of nodes found by this invocation.
//
// blockLock must be taken for reading
func (fbo *FolderBranchOps) searchForNodesInDirLocked(ctx context.Context,
	cache NodeCache, newPtrs map[BlockPointer]bool, md *RootMetadata,
	currDir path, nodeMap map[BlockPointer]Node, numNodesFoundSoFar int) (
	int, error) {
	dirBlock, err := fbo.getDirLocked(ctx, md, currDir, read)
	if err != nil {
		return 0, err
	}

	if numNodesFoundSoFar >= len(nodeMap) {
		return 0, nil
	}

	numNodesFound := 0
	for name, de := range dirBlock.Children {
		if _, ok := nodeMap[de.BlockPointer]; ok {
			childPath := currDir.ChildPathNoPtr(name)
			childPath.path[len(childPath.path)-1].BlockPointer = de.BlockPointer
			// make a node for every pathnode
			var n Node
			for _, pn := range childPath.path {
				n, err = cache.GetOrCreate(pn.BlockPointer, pn.Name, n)
				if err != nil {
					return 0, err
				}
			}
			nodeMap[de.BlockPointer] = n
			numNodesFound++
			if numNodesFoundSoFar+numNodesFound >= len(nodeMap) {
				return numNodesFound, nil
			}
		}

		// otherwise, recurse if this represents an updated block
		if _, ok := newPtrs[de.BlockPointer]; de.Type == Dir && ok {
			childPath := *currDir.ChildPathNoPtr(name)
			childPath.path[len(childPath.path)-1].BlockPointer = de.BlockPointer
			n, err := fbo.searchForNodesInDirLocked(ctx, cache, newPtrs, md,
				childPath, nodeMap, numNodesFoundSoFar+numNodesFound)
			if err != nil {
				return 0, err
			}
			numNodesFound += n
			if numNodesFoundSoFar+numNodesFound >= len(nodeMap) {
				return numNodesFound, nil
			}
		}
	}

	return numNodesFound, nil
}

// searchForNodes tries to resolve all the given pointers to a Node
// object, using only the updated pointers specified in newPtrs.  Does
// an error if any subset of the pointer paths do not exist; it is the
// caller's responsibility to decide to error on particular unresolved
// nodes.
func (fbo *FolderBranchOps) searchForNodes(ctx context.Context,
	cache NodeCache, ptrs []BlockPointer, newPtrs map[BlockPointer]bool,
	md *RootMetadata) (map[BlockPointer]Node, error) {
	fbo.blockLock.RLock()
	defer fbo.blockLock.RUnlock()

	nodeMap := make(map[BlockPointer]Node)
	for _, ptr := range ptrs {
		nodeMap[ptr] = nil
	}

	if len(ptrs) == 0 {
		return nodeMap, nil
	}

	// Start with the root node
	rootPtr := md.data.Dir.BlockPointer
	node := cache.Get(rootPtr)
	if node == nil {
		return nil, fmt.Errorf("Cannot find root node corresponding to %v",
			rootPtr)
	}

	// are they looking for the root directory?
	numNodesFound := 0
	if _, ok := nodeMap[rootPtr]; ok {
		nodeMap[rootPtr] = node
		numNodesFound++
		if numNodesFound >= len(nodeMap) {
			return nodeMap, nil
		}
	}

	rootPath := cache.PathFromNode(node)
	if len(rootPath.path) != 1 {
		return nil, fmt.Errorf("Invalid root path for %v: %s",
			md.data.Dir.BlockPointer, rootPath)
	}

	_, err := fbo.searchForNodesInDirLocked(ctx, cache, newPtrs, md, rootPath,
		nodeMap, numNodesFound)
	if err != nil {
		return nil, err
	}

	// Return the whole map even if some nodes weren't found.
	return nodeMap, nil
}

// searchForNode tries to figure out the path to the given
// blockPointer, using only the block updates that happened as part of
// a given MD update operation.
func (fbo *FolderBranchOps) searchForNode(ctx context.Context,
	ptr BlockPointer, op op, md *RootMetadata) (Node, error) {
	// Record which pointers are new to this update, and thus worth
	// searching.
	newPtrs := make(map[BlockPointer]bool)
	for _, update := range op.AllUpdates() {
		newPtrs[update.Ref] = true
	}

	nodeMap, err := fbo.searchForNodes(ctx, fbo.nodeCache, []BlockPointer{ptr},
		newPtrs, md)
	if err != nil {
		return nil, err
	}

	n, ok := nodeMap[ptr]
	if !ok {
		return nil, NodeNotFoundError{ptr}
	}

	return n, nil
}

func (fbo *FolderBranchOps) unlinkFromCache(op op, oldDir BlockPointer,
	node Node, name string) {
	// The entry could be under any one of the unref'd blocks, and
	// it's safe to perform this when the pointer isn't real, so just
	// try them all to avoid the overhead of looking up the right
	// pointer in the old version of the block.
	childPath := fbo.nodeCache.PathFromNode(node).ChildPathNoPtr(name)
	// revert the parent pointer
	childPath.path[len(childPath.path)-2].BlockPointer = oldDir
	for _, ptr := range op.Unrefs() {
		childPath.path[len(childPath.path)-1].BlockPointer = ptr
		fbo.nodeCache.Unlink(ptr, *childPath)
	}
}

// cacheLock must be taken by the caller.
func (fbo *FolderBranchOps) moveDeCacheEntryLocked(oldParent BlockPointer,
	newParent BlockPointer, moved BlockPointer) {
	if newParent == zeroPtr {
		// A rename within the same directory, so no need to move anything.
		return
	}

	oldPtr := stripBP(oldParent)
	if deMap, ok := fbo.deCache[oldPtr]; ok {
		dePtr := stripBP(moved)
		if de, ok := deMap[dePtr]; ok {
			newPtr := stripBP(newParent)
			if _, ok = fbo.deCache[newPtr]; !ok {
				fbo.deCache[newPtr] = make(map[BlockPointer]DirEntry)
			}
			fbo.deCache[newPtr][dePtr] = de
			delete(deMap, dePtr)
			if len(deMap) == 0 {
				delete(fbo.deCache, oldPtr)
			} else {
				fbo.deCache[oldPtr] = deMap
			}
		}
	}
}

func (fbo *FolderBranchOps) updatePointers(op op) {
	fbo.cacheLock.Lock()
	defer fbo.cacheLock.Unlock()
	for _, update := range op.AllUpdates() {
		fbo.nodeCache.UpdatePointer(update.Unref, update.Ref)
		// move the deCache for this directory
		oldPtrStripped := stripBP(update.Unref)
		if deMap, ok := fbo.deCache[oldPtrStripped]; ok {
			fbo.deCache[stripBP(update.Ref)] = deMap
			delete(fbo.deCache, oldPtrStripped)
		}
	}

	// For renames, we need to update any outstanding writes as well.
	rop, ok := op.(*renameOp)
	if !ok {
		return
	}
	fbo.moveDeCacheEntryLocked(rop.OldDir.Ref, rop.NewDir.Ref, rop.Renamed)
}

func (fbo *FolderBranchOps) notifyOneOp(ctx context.Context, op op,
	md *RootMetadata) {
	fbo.updatePointers(op)

	var changes []NodeChange
	switch realOp := op.(type) {
	default:
		return
	case *createOp:
		node := fbo.nodeCache.Get(realOp.Dir.Ref)
		if node == nil {
			return
		}
		fbo.log.CDebugf(ctx, "notifyOneOp: create %s in node %p",
			realOp.NewName, node.GetID())
		changes = append(changes, NodeChange{
			Node:       node,
			DirUpdated: []string{realOp.NewName},
		})
	case *rmOp:
		node := fbo.nodeCache.Get(realOp.Dir.Ref)
		if node == nil {
			return
		}
		fbo.log.CDebugf(ctx, "notifyOneOp: remove %s in node %p",
			realOp.OldName, node.GetID())
		changes = append(changes, NodeChange{
			Node:       node,
			DirUpdated: []string{realOp.OldName},
		})

		// If this node exists, then the child node might exist too,
		// and we need to unlink it in the node cache.
		fbo.unlinkFromCache(op, realOp.Dir.Unref, node, realOp.OldName)
	case *renameOp:
		oldNode := fbo.nodeCache.Get(realOp.OldDir.Ref)
		if oldNode != nil {
			changes = append(changes, NodeChange{
				Node:       oldNode,
				DirUpdated: []string{realOp.OldName},
			})
		}
		var newNode Node
		if realOp.NewDir.Ref != zeroPtr {
			newNode = fbo.nodeCache.Get(realOp.NewDir.Ref)
			if newNode != nil {
				changes = append(changes, NodeChange{
					Node:       newNode,
					DirUpdated: []string{realOp.NewName},
				})
			}
		} else {
			newNode = oldNode
			if oldNode != nil {
				// Add another name to the existing NodeChange.
				changes[len(changes)-1].DirUpdated =
					append(changes[len(changes)-1].DirUpdated, realOp.NewName)
			}
		}

		if oldNode != nil {
			var newNodeID NodeID
			if newNode != nil {
				newNodeID = newNode.GetID()
			}
			fbo.log.CDebugf(ctx, "notifyOneOp: rename %v from %s/%p to %s/%p",
				realOp.Renamed, realOp.OldName, oldNode.GetID(), realOp.NewName,
				newNodeID)

			if newNode == nil {
				if childNode :=
					fbo.nodeCache.Get(realOp.Renamed); childNode != nil {
					// if the childNode exists, we still have to update
					// its path to go through the new node.  That means
					// creating nodes for all the intervening paths.
					// Unfortunately we don't have enough information to
					// know what the newPath is; we have to guess it from
					// the updates.
					var err error
					newNode, err =
						fbo.searchForNode(ctx, realOp.NewDir.Ref, realOp, md)
					if newNode == nil {
						fbo.log.CErrorf(ctx, "Couldn't find the new node: %v",
							err)
					}
				}
			}

			if newNode != nil {
				// If new node exists as well, unlink any previously
				// existing entry and move the node.
				var unrefPtr BlockPointer
				if oldNode != newNode {
					unrefPtr = realOp.NewDir.Unref
				} else {
					unrefPtr = realOp.OldDir.Unref
				}
				fbo.unlinkFromCache(op, unrefPtr, newNode, realOp.NewName)
				err :=
					fbo.nodeCache.Move(realOp.Renamed, newNode, realOp.NewName)
				if err != nil {
					return
				}
			}
		}
	case *syncOp:
		node := fbo.nodeCache.Get(realOp.File.Ref)
		if node == nil {
			return
		}
		fbo.log.CDebugf(ctx, "notifyOneOp: sync %d writes in node %p",
			len(realOp.Writes), node.GetID())

		changes = append(changes, NodeChange{
			Node:        node,
			FileUpdated: realOp.Writes,
		})
	case *setAttrOp:
		node := fbo.nodeCache.Get(realOp.Dir.Ref)
		if node == nil {
			return
		}
		fbo.log.CDebugf(ctx, "notifyOneOp: setAttr %s for file %s in node %p",
			realOp.Attr, realOp.Name, node.GetID())

		childPath :=
			*fbo.nodeCache.PathFromNode(node).ChildPathNoPtr(realOp.Name)

		// find the node for the actual change; requires looking up
		// the child entry to get the BlockPointer, unfortunately.
		_, de, err := fbo.getEntry(ctx, md, childPath)
		if err != nil {
			return
		}

		childNode := fbo.nodeCache.Get(de.BlockPointer)
		if childNode == nil {
			return
		}

		changes = append(changes, NodeChange{
			Node: childNode,
		})
	}

	fbo.obsLock.RLock()
	defer fbo.obsLock.RUnlock()
	for _, obs := range fbo.observers {
		obs.BatchChanges(ctx, changes)
	}
}

// headLock must be taken for reading, at least
func (fbo *FolderBranchOps) getCurrMDRevisionLocked() MetadataRevision {
	if fbo.head != nil {
		return fbo.head.Revision
	}
	return MetadataRevisionUninitialized
}

func (fbo *FolderBranchOps) getCurrMDRevision() MetadataRevision {
	fbo.headLock.RLock()
	defer fbo.headLock.RUnlock()
	return fbo.getCurrMDRevisionLocked()
}

func (fbo *FolderBranchOps) reembedBlockChanges(ctx context.Context,
	rmds []*RootMetadata) error {
	// if any of the operations have unembedded block ops, fetch those
	// now and fix them up.  TODO: parallelize me.
	for _, rmd := range rmds {
		if rmd.data.Changes.Pointer == zeroPtr {
			continue
		}

		block, err := func() (*FileBlock, error) {
			fbo.blockLock.RLock()
			defer fbo.blockLock.RUnlock()
			// make a fake path so getFileLocked is happy
			p := path{
				FolderBranch: fbo.folderBranch,
				path: []pathNode{
					{BlockPointer: rmd.data.Changes.Pointer}},
			}
			return fbo.getFileLocked(ctx, rmd, p, read)
		}()
		if err != nil {
			return err
		}

		err = fbo.config.Codec().Decode(block.Contents, &rmd.data.Changes)
		if err != nil {
			return err
		}
	}
	return nil
}

type applyMDUpdatesFunc func(context.Context, []*RootMetadata) error

// writerLock must be held by the caller
func (fbo *FolderBranchOps) applyMDUpdatesLocked(ctx context.Context,
	rmds []*RootMetadata) error {
	fbo.headLock.Lock()
	defer fbo.headLock.Unlock()

	// if we have staged changes, ignore all updates until conflict
	// resolution kicks in.  TODO: cache these for future use.
	if fbo.staged {
		if len(rmds) > 0 {
			unmergedRev := MetadataRevisionUninitialized
			if fbo.head != nil {
				unmergedRev = fbo.head.Revision
			}
			fbo.cr.Resolve(unmergedRev, rmds[len(rmds)-1].Revision)
		}
		return errors.New("Ignoring MD updates while local updates are staged")
	}

	// Don't allow updates while we're in the dirty state; the next
	// sync will put us into an unmerged state anyway and we'll
	// require conflict resolution.
	if fbo.getState() != cleanState {
		return errors.New("Ignoring MD updates while writes are dirty")
	}

	fbo.reembedBlockChanges(ctx, rmds)

	for _, rmd := range rmds {
		// check that we're applying the expected MD revision
		if rmd.Revision <= fbo.getCurrMDRevisionLocked() {
			// Already caught up!
			continue
		}
		if rmd.Revision != fbo.getCurrMDRevisionLocked()+1 {
			return MDUpdateApplyError{rmd.Revision,
				fbo.getCurrMDRevisionLocked()}
		}

		err := fbo.setHeadLocked(ctx, rmd)
		if err != nil {
			return err
		}

		for _, op := range rmd.data.Changes.Ops {
			fbo.notifyOneOp(ctx, op, rmd)
		}
	}
	return nil
}

// writerLock must be held by the caller
func (fbo *FolderBranchOps) undoMDUpdatesLocked(ctx context.Context,
	rmds []*RootMetadata) error {
	fbo.headLock.Lock()
	defer fbo.headLock.Unlock()

	// Don't allow updates while we're in the dirty state; the next
	// sync will put us into an unmerged state anyway and we'll
	// require conflict resolution.
	if fbo.getState() != cleanState {
		return NotPermittedWhileDirtyError{}
	}

	fbo.reembedBlockChanges(ctx, rmds)

	// go backwards through the updates
	for i := len(rmds) - 1; i >= 0; i-- {
		rmd := rmds[i]
		// on undo, it's ok to re-apply the current revision since you
		// need to invert all of its ops.
		if rmd.Revision != fbo.getCurrMDRevisionLocked() &&
			rmd.Revision != fbo.getCurrMDRevisionLocked()-1 {
			return MDUpdateInvertError{rmd.Revision,
				fbo.getCurrMDRevisionLocked()}
		}

		err := fbo.setHeadLocked(ctx, rmd)
		if err != nil {
			return err
		}

		// iterate the ops in reverse and invert each one
		ops := rmd.data.Changes.Ops
		for j := len(ops) - 1; j >= 0; j-- {
			fbo.notifyOneOp(ctx, invertOpForLocalNotifications(ops[j]), rmd)
		}
	}
	return nil
}

func (fbo *FolderBranchOps) applyMDUpdates(ctx context.Context,
	rmds []*RootMetadata) error {
	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()
	return fbo.applyMDUpdatesLocked(ctx, rmds)
}

// Assumes all necessary locking is either already done by caller, or
// is done by applyFunc.
func (fbo *FolderBranchOps) getAndApplyMDUpdates(ctx context.Context,
	applyFunc applyMDUpdatesFunc) error {
	// first look up all MD revisions newer than my current head
	start := fbo.getCurrMDRevision() + 1
	rmds, err := getMergedMDUpdates(ctx, fbo.config, fbo.id(), start)
	if err != nil {
		return err
	}

	err = applyFunc(ctx, rmds)
	if err != nil {
		return err
	}
	return nil
}

func (fbo *FolderBranchOps) getUnmergedMDUpdates(ctx context.Context) (
	MetadataRevision, []*RootMetadata, error) {
	// acquire writerLock to read the current branch ID.
	bid := func() BranchID {
		fbo.writerLock.Lock()
		defer fbo.writerLock.Unlock()
		return fbo.bid
	}()
	return getUnmergedMDUpdates(ctx, fbo.config, fbo.id(),
		bid, fbo.getCurrMDRevision())
}

// writerLock should be held by caller.
func (fbo *FolderBranchOps) getUnmergedMDUpdatesLocked(ctx context.Context) (
	MetadataRevision, []*RootMetadata, error) {
	return getUnmergedMDUpdates(ctx, fbo.config, fbo.id(),
		fbo.bid, fbo.getCurrMDRevision())
}

// writerLock should be held by caller.
func (fbo *FolderBranchOps) undoUnmergedMDUpdatesLocked(
	ctx context.Context) error {
	currHead, unmergedRmds, err := fbo.getUnmergedMDUpdatesLocked(ctx)
	if err != nil {
		return err
	}

	err = fbo.undoMDUpdatesLocked(ctx, unmergedRmds)
	if err != nil {
		return err
	}

	// We have arrived at the branch point.  The new root is
	// the previous revision from the current head.  Find it
	// and apply.  TODO: somehow fake the current head into
	// being currHead-1, so that future calls to
	// applyMDUpdates will fetch this along with the rest of
	// the updates.
	fbo.setStagedLocked(false, NullBranchID)

	rmds, err :=
		getMDRange(ctx, fbo.config, fbo.id(), NullBranchID, currHead, currHead, Merged)
	if err != nil {
		return err
	}
	if len(rmds) == 0 {
		return fmt.Errorf("Couldn't find the branch point %d", currHead)
	}
	err = fbo.setHeadLocked(ctx, rmds[0])
	if err != nil {
		return err
	}

	// Now that we're back on the merged branch, forget about all the
	// unmerged updates
	mdcache := fbo.config.MDCache()
	for _, rmd := range unmergedRmds {
		mdcache.Delete(rmd)
	}
	return nil
}

// UnstageForTesting implements the KBFSOps interface for FolderBranchOps
// TODO: remove once we have automatic conflict resolution
func (fbo *FolderBranchOps) UnstageForTesting(
	ctx context.Context, folderBranch FolderBranch) (err error) {
	fbo.log.CDebugf(ctx, "UnstageForTesting")
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	if folderBranch != fbo.folderBranch {
		return WrongOpsError{fbo.folderBranch, folderBranch}
	}

	if !fbo.getStaged() {
		// no-op
		return nil
	}

	if fbo.getState() != cleanState {
		return NotPermittedWhileDirtyError{}
	}

	// launch unstaging in a new goroutine, because we don't want to
	// use the provided context because upper layers might ignore our
	// notifications if we do.  But we still want to wait for the
	// context to cancel.
	c := make(chan error, 1)
	logTags := make(logger.CtxLogTags)
	logTags[CtxFBOIDKey] = CtxFBOOpID
	ctxWithTags := logger.NewContextWithLogTags(context.Background(), logTags)
	id, err := MakeRandomRequestID()
	if err != nil {
		fbo.log.Warning("Couldn't generate a random request ID: %v", err)
	} else {
		ctxWithTags = context.WithValue(ctxWithTags, CtxFBOIDKey, id)
	}
	freshCtx, cancel := context.WithCancel(ctxWithTags)
	defer cancel()
	fbo.log.CDebugf(freshCtx, "Launching new context for UnstageForTesting")
	go func() {
		fbo.writerLock.Lock()
		defer fbo.writerLock.Unlock()

		// fetch all of my unstaged updates, and undo them one at a time
		bid, wasStaged := fbo.bid, fbo.staged
		err := fbo.undoUnmergedMDUpdatesLocked(freshCtx)
		if err != nil {
			c <- err
			return
		}

		// let the server know we no longer have need
		if wasStaged {
			err = fbo.config.MDServer().PruneBranch(freshCtx, fbo.id(), bid)
			if err != nil {
				c <- err
				return
			}
		}

		// now go forward in time, if possible
		c <- fbo.getAndApplyMDUpdates(freshCtx, fbo.applyMDUpdatesLocked)
	}()

	select {
	case err := <-c:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RekeyForTesting implements the KBFSOps interface for FolderBranchOps
// TODO: remove once we have automatic rekeying
func (fbo *FolderBranchOps) RekeyForTesting(
	ctx context.Context, folderBranch FolderBranch) (err error) {
	fbo.log.CDebugf(ctx, "RekeyForTesting")
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	if folderBranch != fbo.folderBranch {
		return WrongOpsError{fbo.folderBranch, folderBranch}
	}

	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()

	md, err := fbo.getMDForWriteLocked(ctx)
	if err != nil {
		return err
	}

	rekeyDone, err := fbo.config.KeyManager().Rekey(ctx, md)
	if err != nil {
		return err
	}

	// TODO: implement a "forced" option that rekeys even when the
	// devices haven't changed?
	if !rekeyDone {
		fbo.log.CDebugf(ctx, "No rekey necessary")
		return nil
	}

	// add an empty operation to satisfy assumptions elsewhere
	md.AddOp(newGCOp())

	err = fbo.finalizeWriteLocked(ctx, md, &blockPutState{})
	if err != nil {
		return err
	}

	// send rekey finish notification
	handle := md.GetTlfHandle()
	fbo.config.Reporter().Notify(ctx, rekeyNotification(ctx, fbo.config, handle, true))

	return nil
}

// SyncFromServer implements the KBFSOps interface for FolderBranchOps
func (fbo *FolderBranchOps) SyncFromServer(
	ctx context.Context, folderBranch FolderBranch) (err error) {
	fbo.log.CDebugf(ctx, "SyncFromServer")
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	if folderBranch != fbo.folderBranch {
		return WrongOpsError{fbo.folderBranch, folderBranch}
	}

	if fbo.getStaged() {
		if err := fbo.cr.Wait(ctx); err != nil {
			return err
		}
		// If we are still staged after the wait, then we have a problem.
		if fbo.getStaged() {
			return fmt.Errorf("Conflict resolution didn't take us out of " +
				"staging.")
		}
	}

	if fbo.getState() != cleanState {
		return errors.New("Can't sync from server while dirty.")
	}

	if err := fbo.getAndApplyMDUpdates(ctx, fbo.applyMDUpdates); err != nil {
		if applyErr, ok := err.(MDUpdateApplyError); ok {
			if applyErr.rev == applyErr.curr {
				fbo.log.CDebugf(ctx, "Already up-to-date with server")
				return nil
			}
		}
		return err
	}
	return nil
}

// CtxFBOTagKey is the type used for unique context tags within FolderBranchOps
type CtxFBOTagKey int

const (
	// CtxFBOIDKey is the type of the tag for unique operation IDs
	// within FolderBranchOps.
	CtxFBOIDKey CtxFBOTagKey = iota
)

// CtxFBOOpID is the display name for the unique operation
// FolderBranchOps ID tag.
const CtxFBOOpID = "FBOID"

// Run the passed function with a context that's canceled on shutdown.
func (fbo *FolderBranchOps) runUnlessShutdown(fn func(ctx context.Context) error) error {
	// Tag each request with a unique ID
	logTags := make(logger.CtxLogTags)
	logTags[CtxFBOIDKey] = CtxFBOOpID
	ctx := logger.NewContextWithLogTags(context.Background(), logTags)
	id, err := MakeRandomRequestID()
	if err != nil {
		fbo.log.Warning("Couldn't generate a random request ID: %v", err)
	} else {
		ctx = context.WithValue(ctx, CtxFBOIDKey, id)
	}

	ctx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	errChan := make(chan error, 1)
	go func() {
		errChan <- fn(ctx)
	}()

	select {
	case err := <-errChan:
		return err
	case <-fbo.shutdownChan:
		return errors.New("shutdown received")
	}
}

func (fbo *FolderBranchOps) registerForUpdates() {
	var err error
	var updateChan <-chan error

	err = fbo.runUnlessShutdown(func(ctx context.Context) (err error) {
		currRev := fbo.getCurrMDRevision()
		fbo.log.CDebugf(ctx, "Registering for updates (curr rev = %d)", currRev)
		defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

		// this will retry on connectivity issues. TODO: backoff on explicit
		// throttle errors from the back-end inside MDServer.
		updateChan, err = fbo.config.MDServer().RegisterForUpdate(ctx, fbo.id(),
			currRev)
		return err
	})

	if err != nil {
		// TODO: we should probably display something or put us in some error
		// state obvious to the user.
		return
	}

	// successful registration; now, wait for an update or a shutdown
	go fbo.runUnlessShutdown(func(ctx context.Context) (err error) {
		fbo.log.CDebugf(ctx, "Waiting for updates")
		defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

		for {
			select {
			case err := <-updateChan:
				fbo.log.CDebugf(ctx, "Got an update: %v", err)
				defer fbo.registerForUpdates()
				if err != nil {
					return err
				}
				err = fbo.getAndApplyMDUpdates(ctx, fbo.applyMDUpdates)
				if err != nil {
					fbo.log.CDebugf(ctx, "Got an error while applying "+
						"updates: %v", err)
					if _, ok := err.(NotPermittedWhileDirtyError); ok {
						// If this fails because of outstanding dirty
						// files, delay a bit to avoid wasting RPCs
						// and CPU.
						time.Sleep(1 * time.Second)
					}
					return err
				}
				return nil
			case unpause := <-fbo.updatePauseChan:
				fbo.log.CInfof(ctx, "Updates paused")
				// wait to be unpaused
				<-unpause
				fbo.log.CInfof(ctx, "Updates unpaused")
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
}

func (fbo *FolderBranchOps) getDirtyPointers() []BlockPointer {
	fbo.cacheLock.Lock()
	defer fbo.cacheLock.Unlock()
	var dirtyPtrs []BlockPointer
	for _, entries := range fbo.deCache {
		for ptr := range entries {
			dirtyPtrs = append(dirtyPtrs, ptr)
		}
	}
	return dirtyPtrs
}

func (fbo *FolderBranchOps) backgroundFlusher(betweenFlushes time.Duration) {
	ticker := time.NewTicker(betweenFlushes)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			dirtyPtrs := fbo.getDirtyPointers()
			fbo.runUnlessShutdown(func(ctx context.Context) (err error) {
				for _, ptr := range dirtyPtrs {
					node := fbo.nodeCache.Get(ptr)
					if node == nil {
						continue
					}
					err := fbo.Sync(ctx, node)
					if err != nil {
						// Just log the warning and keep trying to
						// sync the rest of the dirty files.
						fbo.log.CWarningf(ctx, "Couldn't sync dirty file %v",
							ptr)
					}
				}
				return nil
			})
		case <-fbo.shutdownChan:
			return
		}
	}
}

// finalizeResolution caches all the blocks, and writes the new MD to
// the merged branch, failing if there is a conflict.  It also sends
// out the given newOps notifications locally.  This is used for
// completing conflict resolution.
func (fbo *FolderBranchOps) finalizeResolution(ctx context.Context,
	md *RootMetadata, bps *blockPutState, newOps []op) error {
	// Take the writer lock.
	fbo.writerLock.Lock()
	defer fbo.writerLock.Unlock()

	// Put the blocks into the cache so that, even if we fail below,
	// future attempts may reuse the blocks.
	err := func() error {
		fbo.blockLock.Lock()
		defer fbo.blockLock.Unlock()
		return fbo.finalizeBlocksLocked(bps)
	}()
	if err != nil {
		return err
	}

	// Last chance to get pre-empted.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Put the MD.  If there's a conflict, abort the whole process and
	// let CR restart itself.
	err = fbo.config.MDOps().Put(ctx, md)
	doUnmergedPut := fbo.isRevisionConflict(err)
	if doUnmergedPut {
		fbo.log.CDebugf(ctx, "Got a conflict after resolution; aborting CR")
		return err
	}
	if err != nil {
		return err
	}
	err = fbo.config.MDServer().PruneBranch(ctx, fbo.id(), fbo.bid)
	if err != nil {
		return err
	}

	// Set the head to the new MD.
	fbo.headLock.Lock()
	defer fbo.headLock.Unlock()
	err = fbo.setHeadLocked(ctx, md)
	if err != nil {
		fbo.log.CWarningf(ctx, "Couldn't set local MD head after a "+
			"successful put: %v", err)
		return err
	}
	fbo.setStagedLocked(false, NullBranchID)

	// notifyOneOp for every fixed-up merged op.
	for _, op := range newOps {
		fbo.notifyOneOp(ctx, op, md)
	}
	return nil
}
