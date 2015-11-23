package libkbfs

import (
	"bytes"
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/keybase/client/go/libkb"
	keybase1 "github.com/keybase/client/go/protocol"
	"golang.org/x/net/context"
)

// CounterLock keeps track of the number of lock attempts
type CounterLock struct {
	countLock sync.Mutex
	realLock  sync.Mutex
	count     int
}

func (cl *CounterLock) Lock() {
	cl.countLock.Lock()
	cl.count++
	cl.countLock.Unlock()
	cl.realLock.Lock()
}

func (cl *CounterLock) Unlock() {
	cl.realLock.Unlock()
}

func (cl *CounterLock) GetCount() int {
	cl.countLock.Lock()
	defer cl.countLock.Unlock()
	return cl.count
}

func kbfsOpsConcurInit(t *testing.T, users ...libkb.NormalizedUsername) (
	Config, keybase1.UID, context.Context) {
	config := MakeTestConfigOrBust(t, users...)

	currentUID, err := config.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	return config, currentUID, ctx
}

// Test that only one of two concurrent GetRootMD requests can end up
// fetching the MD from the server.  The second one should wait, and
// then get it from the MD cache.
func TestKBFSOpsConcurDoubleMDGet(t *testing.T) {
	config, uid, ctx := kbfsOpsConcurInit(t, "test_user")
	defer config.Shutdown()
	m := NewMDOpsConcurTest(uid)
	config.SetMDOps(m)

	n := 10
	c := make(chan error, n)
	dir := FakeTlfID(0, false)
	cl := &CounterLock{}

	ops := getOps(config, dir)
	ops.writerLock = cl
	for i := 0; i < n; i++ {
		go func() {
			_, _, _, err := config.KBFSOps().
				GetRootNode(ctx, FolderBranch{dir, MasterBranch})
			c <- err
		}()
	}
	// wait until at least the first one started
	m.enter <- struct{}{}
	close(m.enter)
	// make sure that the second goroutine has also started its write
	// call, and thus must be queued behind the first one (since we
	// are guaranteed the first one is currently running, and they
	// both need the same lock).
	for cl.GetCount() < 2 {
		runtime.Gosched()
	}
	// Now let the first one complete.  The second one should find the
	// MD in the cache, and thus never call MDOps.Get().
	m.start <- struct{}{}
	close(m.start)
	for i := 0; i < n; i++ {
		err := <-c
		if err != nil {
			t.Errorf("Got an error doing concurrent MD gets: err=(%s)", err)
		}
	}
	TestStateForTlf(t, ctx, config, dir)
}

// Test that a read can happen concurrently with a sync
func TestKBFSOpsConcurReadDuringSync(t *testing.T) {
	config, uid, ctx := kbfsOpsConcurInit(t, "test_user")
	defer config.Shutdown()

	// create and write to a file
	kbfsOps := config.KBFSOps()
	h := NewTlfHandle()
	uid, err := config.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Errorf("Couldn't get logged in user: %v", err)
	}
	h.Writers = append(h.Writers, uid)
	rootNode, _, err :=
		kbfsOps.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	fileNode, _, err := kbfsOps.CreateFile(ctx, rootNode, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}
	data := []byte{1}
	err = kbfsOps.Write(ctx, fileNode, data, 0)
	if err != nil {
		t.Fatalf("Couldn't write file: %v", err)
	}

	// now make an MDOps that will pause during Put()
	m := NewMDOpsConcurTest(uid)
	config.SetMDOps(m)

	// start the sync
	errChan := make(chan error)
	go func() {
		errChan <- kbfsOps.Sync(ctx, fileNode)
	}()

	// wait until Sync gets stuck at MDOps.Put()
	m.start <- struct{}{}

	// now make sure we can read the file and see the byte we wrote
	buf := make([]byte, 1)
	nr, err := kbfsOps.Read(ctx, fileNode, buf, 0)
	if err != nil {
		t.Errorf("Couldn't read data: %v\n", err)
	}
	if nr != 1 || !bytes.Equal(data, buf) {
		t.Errorf("Got wrong data %v; expected %v", buf, data)
	}

	// now unblock Sync and make sure there was no error
	m.enter <- struct{}{}
	err = <-errChan
	if err != nil {
		t.Errorf("Sync got an error: %v", err)
	}
	TestStateForTlf(t, ctx, config, rootNode.GetFolderBranch().Tlf)
}

// Test that a write can happen concurrently with a sync
func TestKBFSOpsConcurWriteDuringSync(t *testing.T) {
	config, uid, ctx := kbfsOpsConcurInit(t, "test_user")
	defer config.Shutdown()

	// create and write to a file
	kbfsOps := config.KBFSOps()
	h := NewTlfHandle()
	uid, err := config.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Errorf("Couldn't get logged in user: %v", err)
	}
	h.Writers = append(h.Writers, uid)
	rootNode, _, err :=
		kbfsOps.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	fileNode, _, err := kbfsOps.CreateFile(ctx, rootNode, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}
	data := []byte{1}
	err = kbfsOps.Write(ctx, fileNode, data, 0)
	if err != nil {
		t.Errorf("Couldn't write file: %v", err)
	}

	// now make an MDOps that will pause during Put()
	m := NewMDOpsConcurTest(uid)
	config.SetMDOps(m)

	// start the sync
	errChan := make(chan error)
	go func() {
		errChan <- kbfsOps.Sync(ctx, fileNode)
	}()

	// wait until Sync gets stuck at MDOps.Put()
	m.start <- struct{}{}

	// now make sure we can write the file and see the new byte we wrote
	newData := []byte{2}
	err = kbfsOps.Write(ctx, fileNode, newData, 1)
	if err != nil {
		t.Errorf("Couldn't write data: %v\n", err)
	}

	// read the data back
	buf := make([]byte, 2)
	nr, err := kbfsOps.Read(ctx, fileNode, buf, 0)
	if err != nil {
		t.Errorf("Couldn't read data: %v\n", err)
	}
	expectedData := append(data, newData...)
	if nr != 2 || !bytes.Equal(expectedData, buf) {
		t.Errorf("Got wrong data %v; expected %v", buf, expectedData)
	}

	// now unblock Sync and make sure there was no error
	m.enter <- struct{}{}
	err = <-errChan
	if err != nil {
		t.Errorf("Sync got an error: %v", err)
	}

	// finally, make sure we can still read it after the sync too
	// (even though the second write hasn't been sync'd yet)
	buf2 := make([]byte, 2)
	nr, err = kbfsOps.Read(ctx, fileNode, buf2, 0)
	if err != nil {
		t.Errorf("Couldn't read data: %v\n", err)
	}
	if nr != 2 || !bytes.Equal(expectedData, buf2) {
		t.Errorf("2nd read: Got wrong data %v; expected %v", buf2, expectedData)
	}

	// there should be 5 blocks at this point: the original root block
	// + 2 modifications (create + write), the top indirect file block
	// and a modification (write).
	numCleanBlocks := config.BlockCache().(*BlockCacheStandard).blocks.Len()
	if numCleanBlocks != 5 {
		t.Errorf("Unexpected number of cached clean blocks: %d\n",
			numCleanBlocks)
	}
	TestStateForTlf(t, ctx, config, rootNode.GetFolderBranch().Tlf)
}

// Test that a write can survive a folder BlockPointer update
func TestKBFSOpsConcurWriteDuringFolderUpdate(t *testing.T) {
	config, uid, ctx := kbfsOpsConcurInit(t, "test_user")
	defer config.Shutdown()

	// create and write to a file
	kbfsOps := config.KBFSOps()
	h := NewTlfHandle()
	uid, err := config.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Errorf("Couldn't get logged in user: %v", err)
	}
	h.Writers = append(h.Writers, uid)
	rootNode, _, err :=
		kbfsOps.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	fileNode, _, err := kbfsOps.CreateFile(ctx, rootNode, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}
	data := []byte{1}
	err = kbfsOps.Write(ctx, fileNode, data, 0)
	if err != nil {
		t.Errorf("Couldn't write file: %v", err)
	}

	// Now update the folder pointer in some other way
	_, _, err = kbfsOps.CreateFile(ctx, rootNode, "b", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}

	// Now sync the original file and see make sure the write survived
	if err := kbfsOps.Sync(ctx, fileNode); err != nil {
		t.Fatalf("Couldn't sync: %v", err)
	}

	de, err := kbfsOps.Stat(ctx, fileNode)
	if err != nil {
		t.Errorf("Couldn't stat file: %v", err)
	}
	if g, e := de.Size, len(data); g != uint64(e) {
		t.Errorf("Got wrong size %d; expected %d", g, e)
	}
	TestStateForTlf(t, ctx, config, rootNode.GetFolderBranch().Tlf)
}

// Test that a write can happen concurrently with a sync when there
// are multiple blocks in the file.
func TestKBFSOpsConcurWriteDuringSyncMultiBlocks(t *testing.T) {
	config, uid, ctx := kbfsOpsConcurInit(t, "test_user")
	defer config.Shutdown()

	// make blocks small
	config.BlockSplitter().(*BlockSplitterSimple).maxSize = 5

	// create and write to a file
	kbfsOps := config.KBFSOps()
	h := NewTlfHandle()
	uid, err := config.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Errorf("Couldn't get logged in user: %v", err)
	}
	h.Writers = append(h.Writers, uid)
	rootNode, _, err :=
		kbfsOps.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	fileNode, _, err := kbfsOps.CreateFile(ctx, rootNode, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}
	// 2 blocks worth of data
	data := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	err = kbfsOps.Write(ctx, fileNode, data, 0)
	if err != nil {
		t.Errorf("Couldn't write file: %v", err)
	}

	// sync these initial blocks
	err = kbfsOps.Sync(ctx, fileNode)
	if err != nil {
		t.Errorf("Couldn't do the first sync: %v", err)
	}

	// there should be 7 blocks at this point: the original root block
	// + 2 modifications (create + write), the top indirect file block
	// and a modification (write), and its two children blocks.
	numCleanBlocks := config.BlockCache().(*BlockCacheStandard).blocks.Len()
	if numCleanBlocks != 7 {
		t.Errorf("Unexpected number of cached clean blocks: %d\n",
			numCleanBlocks)
	}

	// write to the first block
	b1data := []byte{11, 12}
	err = kbfsOps.Write(ctx, fileNode, b1data, 0)
	if err != nil {
		t.Errorf("Couldn't write 1st block of file: %v", err)
	}

	// now make an MDOps that will pause during Put()
	m := NewMDOpsConcurTest(uid)
	config.SetMDOps(m)

	// start the sync
	errChan := make(chan error)
	go func() {
		errChan <- kbfsOps.Sync(ctx, fileNode)
	}()

	// wait until Sync gets stuck at MDOps.Put()
	m.start <- struct{}{}

	// now make sure we can write the second block of the file and see
	// the new bytes we wrote
	newData := []byte{20}
	err = kbfsOps.Write(ctx, fileNode, newData, 9)
	if err != nil {
		t.Errorf("Couldn't write data: %v\n", err)
	}

	// read the data back
	buf := make([]byte, 10)
	nr, err := kbfsOps.Read(ctx, fileNode, buf, 0)
	if err != nil {
		t.Errorf("Couldn't read data: %v\n", err)
	}
	expectedData := []byte{11, 12, 3, 4, 5, 6, 7, 8, 9, 20}
	if nr != 10 || !bytes.Equal(expectedData, buf) {
		t.Errorf("Got wrong data %v; expected %v", buf, expectedData)
	}

	// now unblock Sync and make sure there was no error
	m.enter <- struct{}{}
	err = <-errChan
	if err != nil {
		t.Errorf("Sync got an error: %v", err)
	}

	// finally, make sure we can still read it after the sync too
	// (even though the second write hasn't been sync'd yet)
	buf2 := make([]byte, 10)
	nr, err = kbfsOps.Read(ctx, fileNode, buf2, 0)
	if err != nil {
		t.Errorf("Couldn't read data: %v\n", err)
	}
	if nr != 10 || !bytes.Equal(expectedData, buf2) {
		t.Errorf("2nd read: Got wrong data %v; expected %v", buf2, expectedData)
	}
	TestStateForTlf(t, ctx, config, rootNode.GetFolderBranch().Tlf)
}

// Test that a write consisting of multiple blocks can be canceled
// before all blocks have been written.
func TestKBFSOpsConcurWriteParallelBlocksCanceled(t *testing.T) {
	if maxParallelBlockPuts <= 1 {
		t.Skip("Skipping because we are not putting blocks in parallel.")
	}
	config, uid, ctx := kbfsOpsConcurInit(t, "test_user")
	defer config.Shutdown()

	// give it a remote block server with a fake client
	fc := NewFakeBServerClient(nil, nil, nil)
	b := newBlockServerRemoteWithClient(ctx, config, cancelableClient{fc})
	config.SetBlockServer(b)

	// make blocks small
	blockSize := int64(5)
	config.BlockSplitter().(*BlockSplitterSimple).maxSize = blockSize

	// create and write to a file
	kbfsOps := config.KBFSOps()
	h := NewTlfHandle()
	uid, err := config.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Errorf("Couldn't get logged in user: %v", err)
	}
	h.Writers = append(h.Writers, uid)
	rootNode, _, err :=
		kbfsOps.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	fileNode, _, err := kbfsOps.CreateFile(ctx, rootNode, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}
	// Two initial blocks, then maxParallelBlockPuts blocks that
	// will be processed but discarded, then three extra blocks
	// that will be ignored.
	initialBlocks := 2
	extraBlocks := 3
	totalFileBlocks := initialBlocks + maxParallelBlockPuts + extraBlocks
	var data []byte
	for i := int64(0); i < blockSize*int64(totalFileBlocks); i++ {
		data = append(data, byte(i))
	}
	err = kbfsOps.Write(ctx, fileNode, data, 0)
	if err != nil {
		t.Errorf("Couldn't write file: %v", err)
	}

	// now set a control channel, let a couple blocks go in, and then
	// cancel the context
	readyChan := make(chan struct{})
	goChan := make(chan struct{})
	finishChan := make(chan struct{})
	fc.readyChan = readyChan
	fc.goChan = goChan
	fc.finishChan = finishChan

	prevNBlocks := fc.numBlocks()
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		// let the first initialBlocks blocks through.
		for i := 0; i < initialBlocks; i++ {
			<-readyChan
		}

		for i := 0; i < initialBlocks; i++ {
			goChan <- struct{}{}
		}

		for i := 0; i < initialBlocks; i++ {
			<-finishChan
		}

		// Let each parallel block worker block on readyChan.
		for i := 0; i < maxParallelBlockPuts; i++ {
			<-readyChan
		}

		// Make sure all the workers are busy.
		select {
		case <-readyChan:
			t.Error("Worker unexpectedly ready")
		default:
		}

		cancel()
	}()

	err = kbfsOps.Sync(ctx, fileNode)
	if err != context.Canceled {
		t.Errorf("Sync did not get canceled error: %v", err)
	}
	nowNBlocks := fc.numBlocks()
	if nowNBlocks != prevNBlocks+2 {
		t.Errorf("Unexpected number of blocks; prev = %d, now = %d",
			prevNBlocks, nowNBlocks)
	}

	// Now clean up by letting the rest of the blocks through.
	for i := 0; i < maxParallelBlockPuts; i++ {
		goChan <- struct{}{}
	}

	for i := 0; i < maxParallelBlockPuts; i++ {
		<-finishChan
	}

	// Make sure there are no more workers, i.e. the extra blocks
	// aren't sent to the server.
	select {
	case <-readyChan:
		t.Error("Worker unexpectedly ready")
	default:
	}
}

// Test that, when writing multiple blocks in parallel, one error will
// cancel the remaining puts.
func TestKBFSOpsConcurWriteParallelBlocksError(t *testing.T) {
	config, uid, ctx := kbfsOpsConcurInit(t, "test_user")
	defer config.Shutdown()

	// give it a mock'd block server
	ctr := NewSafeTestReporter(t)
	mockCtrl := gomock.NewController(ctr)
	defer mockCtrl.Finish()
	defer ctr.CheckForFailures()
	b := NewMockBlockServer(mockCtrl)
	config.SetBlockServer(b)

	// from the folder creation, then 2 for file creation
	c := b.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any()).Times(3).Return(nil)

	// make blocks small
	blockSize := int64(5)
	config.BlockSplitter().(*BlockSplitterSimple).maxSize = blockSize

	// create and write to a file
	kbfsOps := config.KBFSOps()
	h := NewTlfHandle()
	uid, err := config.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Errorf("Couldn't get logged in user: %v", err)
	}
	h.Writers = append(h.Writers, uid)
	rootNode, _, err :=
		kbfsOps.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	fileNode, _, err := kbfsOps.CreateFile(ctx, rootNode, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}
	// 15 blocks
	var data []byte
	fileBlocks := int64(15)
	for i := int64(0); i < blockSize*fileBlocks; i++ {
		data = append(data, byte(i))
	}
	err = kbfsOps.Write(ctx, fileNode, data, 0)
	if err != nil {
		t.Errorf("Couldn't write file: %v", err)
	}

	// let two blocks through and fail the third:
	c = b.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any()).Times(2).After(c).Return(nil)
	putErr := errors.New("This is a forced error on put")
	errPtrChan := make(chan BlockPointer)
	c = b.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any()).
		Do(func(ctx context.Context, id BlockID, tlfID TlfID,
		context BlockContext, buf []byte,
		serverHalf BlockCryptKeyServerHalf) {
		errPtrChan <- context.(BlockPointer)
	}).After(c).Return(putErr)
	// let the rest through
	proceedChan := make(chan struct{})
	b.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any()).AnyTimes().
		Do(func(ctx context.Context, id BlockID, tlfID TlfID,
		context BlockContext, buf []byte,
		serverHalf BlockCryptKeyServerHalf) {
		<-proceedChan
	}).After(c).Return(nil)
	b.EXPECT().Shutdown().AnyTimes()

	var errPtr BlockPointer
	go func() {
		errPtr = <-errPtrChan
		close(proceedChan)
	}()

	err = kbfsOps.Sync(ctx, fileNode)
	if err != putErr {
		t.Errorf("Sync did not get the expected error: %v", err)
	}

	// wait for proceedChan to close, so we know the errPtr has been set
	<-proceedChan

	// make sure the error'd file didn't make it to the cache
	if _, err := config.BlockCache().Get(errPtr, MasterBranch); err == nil {
		t.Errorf("Failed block put for %v left block in cache", errPtr)
	}
}
