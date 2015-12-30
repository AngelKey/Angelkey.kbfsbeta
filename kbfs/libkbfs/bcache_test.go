package libkbfs

import "testing"

func blockCacheTestInit(t *testing.T, capacity int) Config {
	config := MakeTestConfigOrBust(t, "test")
	b := NewBlockCacheStandard(config, capacity)
	config.SetBlockCache(b)
	return config
}

func testBcachePutWithBlock(t *testing.T, id BlockID, bcache BlockCache, lifetime BlockCacheLifetime, block Block) {
	ptr := BlockPointer{ID: id}
	tlf := FakeTlfID(1, false)
	branch := MasterBranch

	// put the block
	if err := bcache.Put(ptr, tlf, block, lifetime); err != nil {
		t.Errorf("Got error on Put for block %s: %v", id, err)
	}

	// make sure we can get it successfully
	if block2, err := bcache.Get(ptr, branch); err != nil {
		t.Errorf("Got error on get for block %s: %v", id, err)
	} else if block2 != block {
		t.Errorf("Got %v, expected %v", block2, block)
	}

	// make sure its dirty status is right
	if bcache.IsDirty(ptr, branch) {
		t.Errorf("Block %s unexpectedly dirty", id)
	}
}

func testBcachePut(t *testing.T, id BlockID, bcache BlockCache, lifetime BlockCacheLifetime) {
	block := NewFileBlock()
	testBcachePutWithBlock(t, id, bcache, lifetime, block)
}

func testBcachePutDirty(t *testing.T, id BlockID, bcache BlockCache) {
	block := NewFileBlock()
	ptr := BlockPointer{ID: id}
	branch := MasterBranch

	// put the block
	if err := bcache.PutDirty(ptr, branch, block); err != nil {
		t.Errorf("Got error on PutDirty for block %s: %v", id, err)
	}

	// make sure we can get it successfully
	if block2, err := bcache.Get(ptr, branch); err != nil {
		t.Errorf("Got error on get for block %s: %v", id, err)
	} else if block2 != block {
		t.Errorf("Got back unexpected block: %v", block2)
	}

	// make sure its dirty status is right
	if !bcache.IsDirty(ptr, branch) {
		t.Errorf("Block %s unexpectedly not dirty", id)
	}
}

func testExpectedMissing(t *testing.T, id BlockID, bcache BlockCache) {
	expectedErr := NoSuchBlockError{id}
	ptr := BlockPointer{ID: id}
	if _, err := bcache.Get(ptr, MasterBranch); err == nil {
		t.Errorf("No expected error on 1st get: %v", err)
	} else if err != expectedErr {
		t.Errorf("Got unexpected error on 1st get: %v", err)
	}
}

func TestBcachePut(t *testing.T) {
	config := blockCacheTestInit(t, 100)
	defer CheckConfigAndShutdown(t, config)
	testBcachePut(t, fakeBlockID(1), config.BlockCache(), TransientEntry)
	testBcachePut(t, fakeBlockID(2), config.BlockCache(), PermanentEntry)
}

func TestBcachePutDirty(t *testing.T) {
	config := blockCacheTestInit(t, 100)
	defer CheckConfigAndShutdown(t, config)
	testBcachePutDirty(t, fakeBlockID(1), config.BlockCache())
}

func TestBcachePutPastCapacity(t *testing.T) {
	config := blockCacheTestInit(t, 2)
	defer CheckConfigAndShutdown(t, config)
	bcache := config.BlockCache()
	id1 := fakeBlockID(1)
	testBcachePut(t, id1, bcache, TransientEntry)
	id2 := fakeBlockID(2)
	testBcachePut(t, id2, bcache, TransientEntry)
	testBcachePut(t, fakeBlockID(3), bcache, TransientEntry)

	// now block 1 should have been kicked out
	testExpectedMissing(t, id1, bcache)

	// but 2 should still be there
	if _, err := bcache.Get(BlockPointer{ID: id2}, MasterBranch); err != nil {
		t.Errorf("Got unexpected error on 2nd get: %v", err)
	}

	// permanent blocks don't count
	testBcachePut(t, fakeBlockID(4), config.BlockCache(), PermanentEntry)

	// dirty blocks don't count
	testBcachePutDirty(t, fakeBlockID(5), bcache)
	testBcachePutDirty(t, fakeBlockID(6), bcache)
	testBcachePutDirty(t, fakeBlockID(7), bcache)
	testBcachePutDirty(t, fakeBlockID(8), bcache)

	// 2 should still be there
	if _, err := bcache.Get(BlockPointer{ID: id2}, MasterBranch); err != nil {
		t.Errorf("Got unexpected error on 2nd get: %v", err)
	}
}

func TestBcachePutDuplicateDirty(t *testing.T) {
	config := blockCacheTestInit(t, 2)
	defer CheckConfigAndShutdown(t, config)
	bcache := config.BlockCache()
	// put one under the default block pointer and branch name (clean)
	id1 := fakeBlockID(1)
	testBcachePut(t, id1, bcache, TransientEntry)
	cleanBranch := MasterBranch

	// Then dirty a different reference nonce, and make sure the
	// original is still clean
	newNonce := BlockRefNonce([8]byte{1, 0, 0, 0, 0, 0, 0, 0})
	newNonceBlock := NewFileBlock()
	err := bcache.PutDirty(BlockPointer{ID: id1, RefNonce: newNonce},
		MasterBranch, newNonceBlock)
	if err != nil {
		t.Errorf("Unexpected error on PutDirty: %v", err)
	}

	// make sure the original dirty status is right
	if bcache.IsDirty(BlockPointer{ID: id1}, cleanBranch) {
		t.Errorf("Original block is now unexpectedly dirty")
	}
	if !bcache.IsDirty(BlockPointer{ID: id1, RefNonce: newNonce}, cleanBranch) {
		t.Errorf("New refnonce block is now unexpectedly clean")
	}

	// Then dirty a different branch, and make sure the
	// original is still clean
	newBranch := BranchName("dirtyBranch")
	newBranchBlock := NewFileBlock()
	err = bcache.PutDirty(BlockPointer{ID: id1}, newBranch, newBranchBlock)
	if err != nil {
		t.Errorf("Unexpected error on PutDirty: %v", err)
	}

	// make sure the original dirty status is right
	if bcache.IsDirty(BlockPointer{ID: id1}, cleanBranch) {
		t.Errorf("Original block is now unexpectedly dirty")
	}
	if !bcache.IsDirty(BlockPointer{ID: id1, RefNonce: newNonce}, cleanBranch) {
		t.Errorf("New refnonce block is now unexpectedly clean")
	}
	if !bcache.IsDirty(BlockPointer{ID: id1}, newBranch) {
		t.Errorf("New branch block is now unexpectedly clean")
	}
}

func TestBcacheCheckPtrSuccess(t *testing.T) {
	config := blockCacheTestInit(t, 100)
	defer CheckConfigAndShutdown(t, config)
	bcache := config.BlockCache()

	block := NewFileBlock().(*FileBlock)
	block.Contents = []byte{1, 2, 3, 4}
	id := fakeBlockID(1)
	ptr := BlockPointer{ID: id}
	tlf := FakeTlfID(1, false)

	err := bcache.Put(ptr, tlf, block, TransientEntry)
	if err != nil {
		t.Errorf("Couldn't put block: %v", err)
	}

	checkedPtr, err := bcache.CheckForKnownPtr(tlf, block)
	if err != nil {
		t.Errorf("Unexpected error checking id: %v", err)
	} else if checkedPtr != ptr {
		t.Errorf("Unexpected pointer; got %v, expected %v", checkedPtr, id)
	}
}

func TestBcacheCheckPtrPermanent(t *testing.T) {
	config := blockCacheTestInit(t, 100)
	defer config.Shutdown()
	bcache := config.BlockCache()

	block := NewFileBlock().(*FileBlock)
	block.Contents = []byte{1, 2, 3, 4}
	id := fakeBlockID(1)
	ptr := BlockPointer{ID: id}
	tlf := FakeTlfID(1, false)

	err := bcache.Put(ptr, tlf, block, PermanentEntry)
	if err != nil {
		t.Errorf("Couldn't put block: %v", err)
	}

	checkedPtr, err := bcache.CheckForKnownPtr(tlf, block)
	if err != nil {
		t.Errorf("Unexpected error checking id: %v", err)
	} else if checkedPtr != (BlockPointer{}) {
		t.Errorf("Unexpected non-zero pointer %v", checkedPtr)
	}
}

func TestBcacheCheckPtrNotFound(t *testing.T) {
	config := blockCacheTestInit(t, 100)
	defer CheckConfigAndShutdown(t, config)
	bcache := config.BlockCache()

	block := NewFileBlock().(*FileBlock)
	block.Contents = []byte{1, 2, 3, 4}
	id := fakeBlockID(1)
	ptr := BlockPointer{ID: id}
	tlf := FakeTlfID(1, false)

	err := bcache.Put(ptr, tlf, block, TransientEntry)
	if err != nil {
		t.Errorf("Couldn't put block: %v", err)
	}

	block2 := NewFileBlock().(*FileBlock)
	block2.Contents = []byte{4, 3, 2, 1}
	checkedPtr, err := bcache.CheckForKnownPtr(tlf, block2)
	if err != nil {
		t.Errorf("Unexpected error checking id: %v", err)
	} else if checkedPtr.IsInitialized() {
		t.Errorf("Unexpected ID; got %v, expected null", checkedPtr)
	}
}

func TestBcacheDeletePermanent(t *testing.T) {
	config := blockCacheTestInit(t, 100)
	defer CheckConfigAndShutdown(t, config)
	bcache := config.BlockCache()

	id1 := fakeBlockID(1)
	testBcachePut(t, id1, bcache, PermanentEntry)

	id2 := fakeBlockID(2)
	block2 := NewFileBlock()
	testBcachePutWithBlock(t, id2, bcache, TransientEntry, block2)
	testBcachePutWithBlock(t, id2, bcache, PermanentEntry, block2)

	bcache.DeletePermanent(id1)
	bcache.DeletePermanent(id2)
	testExpectedMissing(t, id1, bcache)

	// 2 should still be there
	if _, err := bcache.Get(BlockPointer{ID: id2}, MasterBranch); err != nil {
		t.Errorf("Got unexpected error on 2nd get: %v", err)
	}
}

func TestBcacheDeleteDirty(t *testing.T) {
	config := blockCacheTestInit(t, 100)
	defer CheckConfigAndShutdown(t, config)
	bcache := config.BlockCache()

	id1 := fakeBlockID(1)
	testBcachePutDirty(t, id1, bcache)
	id2 := fakeBlockID(2)
	testBcachePut(t, id2, bcache, TransientEntry)

	bcache.DeleteDirty(BlockPointer{ID: id1}, MasterBranch)
	testExpectedMissing(t, id1, bcache)

	// 2 should still be there
	if _, err := bcache.Get(BlockPointer{ID: id2}, MasterBranch); err != nil {
		t.Errorf("Got unexpected error on 2nd get: %v", err)
	}
}

func TestBcacheEmptyTransient(t *testing.T) {
	config := blockCacheTestInit(t, 0)
	defer config.Shutdown()

	bcache := config.BlockCache()

	block := NewFileBlock()
	id := fakeBlockID(1)
	ptr := BlockPointer{ID: id}
	tlf := FakeTlfID(1, false)
	branch := MasterBranch

	// Make sure all the operations work even if the cache has no
	// transient capacity.

	if err := bcache.Put(ptr, tlf, block, TransientEntry); err != nil {
		t.Errorf("Got error on Put for block %s: %v", id, err)
	}

	_, err := bcache.Get(ptr, branch)
	if _, ok := err.(NoSuchBlockError); !ok {
		t.Errorf("Got unexpected error %v", err)
	}

	if bcache.IsDirty(ptr, branch) {
		t.Errorf("Block %s unexpectedly dirty", id)
	}

	err = bcache.DeletePermanent(id)
	if err != nil {
		t.Errorf("Got unexpected error %v", err)
	}

	_, err = bcache.CheckForKnownPtr(tlf, block.(*FileBlock))
	if err != nil {
		t.Errorf("Got unexpected error %v", err)
	}
}
