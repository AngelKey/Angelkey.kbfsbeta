package libkbfs

import (
	"reflect"
	"testing"
	"time"

	"github.com/keybase/client/go/libkb"
	"golang.org/x/net/context"
)

func readAndCompareData(t *testing.T, config Config, ctx context.Context,
	h *TlfHandle, expectedData []byte, user libkb.NormalizedUsername) {
	kbfsOps := config.KBFSOps()
	rootNode, _, err :=
		kbfsOps.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Errorf("Couldn't get folder: %v", err)
	}
	fileNode, _, err := kbfsOps.Lookup(ctx, rootNode, "a")
	if err != nil {
		t.Fatalf("Couldn't lookup file: %v", err)
	}
	data := make([]byte, 1)
	_, err = kbfsOps.Read(ctx, fileNode, data, 0)
	if err != nil {
		t.Fatalf("Couldn't read file: %v", err)
	}
	if data[0] != expectedData[0] {
		t.Errorf("User %s didn't see expected data: %v", user, data)
	}
}

type testCRObserver struct {
	c       chan<- struct{}
	changes []NodeChange
}

func (t *testCRObserver) LocalChange(ctx context.Context, node Node,
	write WriteRange) {
	// ignore
}

func (t *testCRObserver) BatchChanges(ctx context.Context,
	changes []NodeChange) {
	t.changes = append(t.changes, changes...)
	t.c <- struct{}{}
}

func checkStatus(t *testing.T, ctx context.Context, kbfsOps KBFSOps,
	staged bool, headWriter libkb.NormalizedUsername, dirtyPaths []string, fb FolderBranch,
	prefix string) {
	status, _, err := kbfsOps.Status(ctx, fb)
	if err != nil {
		t.Fatalf("%s: Couldn't get status", prefix)
	}
	if status.Staged != staged {
		t.Errorf("%s: Staged doesn't match, according to status", prefix)
	}
	if status.HeadWriter != headWriter {
		t.Errorf("%s: Unexpected head writer: %s", prefix, status.HeadWriter)
	}
	checkStringSlices(t, dirtyPaths, status.DirtyPaths)
}

func TestBasicMDUpdate(t *testing.T) {
	// simulate two users
	var userName1, userName2 libkb.NormalizedUsername = "u1", "u2"
	config1, uid1, ctx := kbfsOpsConcurInit(t, userName1, userName2)
	defer CheckConfigAndShutdown(t, config1)

	config2 := ConfigAsUser(config1.(*ConfigLocal), userName2)
	defer CheckConfigAndShutdown(t, config2)
	uid2, err := config2.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	h := NewTlfHandle()
	h.Writers = append(h.Writers, uid1)
	h.Writers = append(h.Writers, uid2)

	kbfsOps1 := config1.KBFSOps()
	rootNode1, _, err :=
		kbfsOps1.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	kbfsOps2 := config2.KBFSOps()
	rootNode2, _, err :=
		kbfsOps2.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't get root: %v", err)
	}

	_, statusChan, err := kbfsOps2.Status(ctx, rootNode2.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't get status")
	}

	// register client 2 as a listener before the create happens
	c := make(chan struct{})
	config2.Notifier().RegisterForChanges(
		[]FolderBranch{rootNode1.GetFolderBranch()}, &testCRObserver{c, nil})

	// user 1 creates a file
	_, _, err = kbfsOps1.CreateFile(ctx, rootNode1, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}

	// Wait for the next batch change notification
	<-c

	entries, err := kbfsOps2.GetDirChildren(ctx, rootNode2)
	if err != nil {
		t.Fatalf("User 2 couldn't see the root dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("User 2 sees wrong number of entries in root dir: %d vs 1",
			len(entries))
	}
	if _, ok := entries["a"]; !ok {
		t.Fatalf("User 2 doesn't see file a")
	}

	// The status should have fired as well (though in this case the
	// writer is the same as before)
	<-statusChan
	checkStatus(t, ctx, kbfsOps1, false, userName1, nil,
		rootNode1.GetFolderBranch(), "Node 1")
	checkStatus(t, ctx, kbfsOps2, false, userName1, nil,
		rootNode2.GetFolderBranch(), "Node 2")
}

func testMultipleMDUpdates(t *testing.T, unembedChanges bool) {
	// simulate two users
	var userName1, userName2 libkb.NormalizedUsername = "u1", "u2"
	config1, uid1, ctx := kbfsOpsConcurInit(t, userName1, userName2)
	defer CheckConfigAndShutdown(t, config1)

	config2 := ConfigAsUser(config1.(*ConfigLocal), userName2)
	defer CheckConfigAndShutdown(t, config2)
	uid2, err := config2.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if unembedChanges {
		bss1, ok1 := config1.BlockSplitter().(*BlockSplitterSimple)
		bss2, ok2 := config2.BlockSplitter().(*BlockSplitterSimple)
		if !ok1 || !ok2 {
			t.Fatalf("Couldn't convert BlockSplitters!")
		}
		bss1.blockChangeEmbedMaxSize = 3
		bss2.blockChangeEmbedMaxSize = 3
	}

	h := NewTlfHandle()
	h.Writers = append(h.Writers, uid1)
	h.Writers = append(h.Writers, uid2)

	kbfsOps1 := config1.KBFSOps()
	rootNode1, _, err :=
		kbfsOps1.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	// user 1 creates a file
	_, _, err = kbfsOps1.CreateFile(ctx, rootNode1, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}

	// user 2 looks up the directory (and sees the file)
	kbfsOps2 := config2.KBFSOps()
	rootNode2, _, err :=
		kbfsOps2.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Errorf("Couldn't get root: %v", err)
	}

	// register client 2 as a listener before the create happens
	c := make(chan struct{})
	config2.Notifier().RegisterForChanges(
		[]FolderBranch{rootNode1.GetFolderBranch()}, &testCRObserver{c, nil})

	// now user 1 renames the old file, and creates a new one
	err = kbfsOps1.Rename(ctx, rootNode1, "a", rootNode1, "b")
	if err != nil {
		t.Fatalf("Couldn't rename file: %v", err)
	}
	_, _, err = kbfsOps1.CreateFile(ctx, rootNode1, "c", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}

	// Wait for the next 2 batch change notifications
	<-c
	<-c

	entries, err := kbfsOps2.GetDirChildren(ctx, rootNode2)
	if err != nil {
		t.Fatalf("User 2 couldn't see the root dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("User 2 sees wrong number of entries in root dir: %d vs 2",
			len(entries))
	}
	if _, ok := entries["b"]; !ok {
		t.Fatalf("User 2 doesn't see file b")
	}
	if _, ok := entries["c"]; !ok {
		t.Fatalf("User 2 doesn't see file c")
	}
}

func TestMultipleMDUpdates(t *testing.T) {
	testMultipleMDUpdates(t, false)
}

func TestMultipleMDUpdatesUnembedChanges(t *testing.T) {
	testMultipleMDUpdates(t, true)
}

// Tests that, in the face of a conflict, a user will commit its
// changes to a private branch, which will persist after restart (and
// the other user will be unaffected).
func TestUnmergedAfterRestart(t *testing.T) {
	// simulate two users
	var userName1, userName2 libkb.NormalizedUsername = "u1", "u2"
	config1, uid1, ctx := kbfsOpsConcurInit(t, userName1, userName2)
	defer CheckConfigAndShutdown(t, config1)

	config2 := ConfigAsUser(config1.(*ConfigLocal), userName2)
	defer CheckConfigAndShutdown(t, config2)
	uid2, err := config2.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	h := NewTlfHandle()
	h.Writers = append(h.Writers, uid1)
	h.Writers = append(h.Writers, uid2)

	// user1 creates a file in a shared dir
	kbfsOps1 := config1.KBFSOps()
	rootNode1, _, err :=
		kbfsOps1.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	fileNode1, _, err := kbfsOps1.CreateFile(ctx, rootNode1, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}

	// then user2 write to the file
	kbfsOps2 := config2.KBFSOps()
	rootNode2, _, err :=
		kbfsOps2.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	fileNode2, _, err := kbfsOps2.Lookup(ctx, rootNode2, "a")
	if err != nil {
		t.Fatalf("Couldn't lookup file: %v", err)
	}
	data2 := []byte{2}
	err = kbfsOps2.Write(ctx, fileNode2, data2, 0)
	if err != nil {
		t.Fatalf("Couldn't write file: %v", err)
	}
	checkStatus(t, ctx, kbfsOps2, false, userName1, []string{"u1,u2/a"},
		rootNode2.GetFolderBranch(), "Node 2 (after write)")
	err = kbfsOps2.Sync(ctx, fileNode2)
	if err != nil {
		t.Fatalf("Couldn't sync file: %v", err)
	}

	DisableCRForTesting(config1, rootNode1.GetFolderBranch())

	// Now when user 1 tries to write to file 1 and sync, it will
	// become unmerged.  Because this happens in the same goroutine as
	// the above Sync, we can be sure that the updater on client 1
	// hasn't yet seen the MD update, and so its Sync will present a
	// conflict.
	data1 := []byte{1}
	err = kbfsOps1.Write(ctx, fileNode1, data1, 0)
	if err != nil {
		t.Fatalf("Couldn't write file: %v", err)
	}
	checkStatus(t, ctx, kbfsOps1, false, userName1, []string{"u1,u2/a"},
		rootNode1.GetFolderBranch(), "Node 1 (after write)")
	err = kbfsOps1.Sync(ctx, fileNode1)
	if err != nil {
		t.Fatalf("Couldn't sync file: %v", err)
	}

	checkStatus(t, ctx, kbfsOps1, true, userName1, nil,
		rootNode1.GetFolderBranch(), "Node 1")
	checkStatus(t, ctx, kbfsOps2, false, userName2, nil,
		rootNode2.GetFolderBranch(), "Node 2")

	// now re-login the users, and make sure 1 can see the changes,
	// but 2 can't
	config1B := ConfigAsUser(config1.(*ConfigLocal), userName1)
	defer CheckConfigAndShutdown(t, config1B)
	config2B := ConfigAsUser(config1.(*ConfigLocal), userName2)
	defer CheckConfigAndShutdown(t, config2B)

	DisableCRForTesting(config1B, rootNode1.GetFolderBranch())

	readAndCompareData(t, config1B, ctx, h, data1, userName1)
	readAndCompareData(t, config2B, ctx, h, data2, userName2)

	checkStatus(t, ctx, config1B.KBFSOps(), true, userName1, nil,
		rootNode1.GetFolderBranch(), "Node 1")
	checkStatus(t, ctx, config2B.KBFSOps(), false, userName2, nil,
		rootNode2.GetFolderBranch(), "Node 2")

	// register as a listener before the unstaging happens
	c := make(chan struct{}, 2)
	cro := &testCRObserver{c, nil}
	config1B.Notifier().RegisterForChanges(
		[]FolderBranch{rootNode1.GetFolderBranch()}, cro)

	// Unstage user 1's changes, and make sure everyone is back in
	// sync.  TODO: remove this once we have automatic conflict
	// resolution.
	err = config1B.KBFSOps().UnstageForTesting(ctx, rootNode1.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't unstage: %v", err)
	}

	// we should have had two updates, one for the unstaging and one
	// for the fast-forward
	<-c
	<-c
	// make sure we see two sync op changes, on the same node
	if len(cro.changes) != 2 {
		t.Errorf("Unexpected number of changes: %d", len(cro.changes))
	}
	var n Node
	for _, change := range cro.changes {
		if n == nil {
			n = change.Node
		} else if n.GetID() != change.Node.GetID() {
			t.Errorf("Changes involve different nodes, %v vs %v\n",
				n.GetID(), change.Node.GetID())
		}
	}

	readAndCompareData(t, config1B, ctx, h, data2, userName2)
	readAndCompareData(t, config2B, ctx, h, data2, userName2)
	checkStatus(t, ctx, config1B.KBFSOps(), false, userName2, nil,
		rootNode1.GetFolderBranch(), "Node 1 (after unstage)")
	checkStatus(t, ctx, config2B.KBFSOps(), false, userName2, nil,
		rootNode2.GetFolderBranch(), "Node 2 (after unstage)")
}

// Tests that multiple users can write to the same file sequentially
// without any problems.
func TestMultiUserWrite(t *testing.T) {
	// simulate two users
	var userName1, userName2 libkb.NormalizedUsername = "u1", "u2"
	config1, uid1, ctx := kbfsOpsConcurInit(t, userName1, userName2)
	defer CheckConfigAndShutdown(t, config1)

	config2 := ConfigAsUser(config1.(*ConfigLocal), userName2)
	defer CheckConfigAndShutdown(t, config2)
	uid2, err := config2.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	h := NewTlfHandle()
	h.Writers = append(h.Writers, uid1)
	h.Writers = append(h.Writers, uid2)

	// user1 creates a file in a shared dir
	kbfsOps1 := config1.KBFSOps()
	rootNode1, _, err :=
		kbfsOps1.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Errorf("Couldn't create folder: %v", err)
	}
	_, _, err = kbfsOps1.CreateFile(ctx, rootNode1, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}

	// then user2 write to the file
	kbfsOps2 := config2.KBFSOps()
	rootNode2, _, err :=
		kbfsOps2.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Errorf("Couldn't create folder: %v", err)
	}
	fileNode2, _, err := kbfsOps2.Lookup(ctx, rootNode2, "a")
	if err != nil {
		t.Fatalf("Couldn't lookup file: %v", err)
	}

	data2 := []byte{2}
	err = kbfsOps2.Write(ctx, fileNode2, data2, 0)
	if err != nil {
		t.Fatalf("Couldn't write file: %v", err)
	}

	// The writer should be user 2, even before the Sync
	ops := kbfsOps2.(*KBFSOpsStandard).getOpsByNode(fileNode2)
	de, err := ops.statEntry(ctx, fileNode2)
	if err != nil {
		t.Fatalf("Couldn't lookup file: %v", err)
	}
	if de.GetWriter() != uid2 {
		t.Errorf("After user 2's first write, Writer is wrong: %v",
			de.GetWriter())
	}

	err = kbfsOps2.Sync(ctx, fileNode2)
	if err != nil {
		t.Fatalf("Couldn't sync file: %v", err)
	}
	data3 := []byte{3}
	err = kbfsOps2.Write(ctx, fileNode2, data3, 0)
	if err != nil {
		t.Fatalf("Couldn't write file: %v", err)
	}
	err = kbfsOps2.Sync(ctx, fileNode2)
	if err != nil {
		t.Fatalf("Couldn't sync file: %v", err)
	}

	readAndCompareData(t, config2, ctx, h, data3, userName2)
}

// Tests that two users can make independent writes while forked, and
// conflict resolution will merge them correctly.
func TestBasicCRNoConflict(t *testing.T) {
	// simulate two users
	var userName1, userName2 libkb.NormalizedUsername = "u1", "u2"
	config1, uid1, ctx := kbfsOpsConcurInit(t, userName1, userName2)
	defer CheckConfigAndShutdown(t, config1)

	config2 := ConfigAsUser(config1.(*ConfigLocal), userName2)
	defer CheckConfigAndShutdown(t, config2)
	uid2, err := config2.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	h := NewTlfHandle()
	h.Writers = append(h.Writers, uid1)
	h.Writers = append(h.Writers, uid2)

	// user1 creates a file in a shared dir
	kbfsOps1 := config1.KBFSOps()
	rootNode1, _, err :=
		kbfsOps1.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	_, _, err = kbfsOps1.CreateFile(ctx, rootNode1, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}

	// look it up on user2
	kbfsOps2 := config2.KBFSOps()
	rootNode2, _, err :=
		kbfsOps2.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	_, _, err = kbfsOps2.Lookup(ctx, rootNode2, "a")
	if err != nil {
		t.Fatalf("Couldn't lookup file: %v", err)
	}

	// disable updates on user 2
	c, err := DisableUpdatesForTesting(config2, rootNode2.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't disable updates: %v", err)
	}

	// User 1 makes a new file
	_, _, err = kbfsOps1.CreateFile(ctx, rootNode1, "b", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}

	// User 2 makes a new different file
	_, _, err = kbfsOps2.CreateFile(ctx, rootNode2, "c", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}

	// re-enable updates, and wait for CR to complete
	c <- struct{}{}
	err = kbfsOps2.SyncFromServer(ctx, rootNode2.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't sync from server: %v", err)
	}

	err = kbfsOps1.SyncFromServer(ctx, rootNode1.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't sync from server: %v", err)
	}

	// Make sure they both see the same set of children
	expectedChildren := []string{"a", "b", "c"}
	children1, err := kbfsOps1.GetDirChildren(ctx, rootNode1)
	if err != nil {
		t.Fatalf("Couldn't get children: %v", err)
	}

	children2, err := kbfsOps2.GetDirChildren(ctx, rootNode2)
	if err != nil {
		t.Fatalf("Couldn't get children: %v", err)
	}

	if g, e := len(children1), len(expectedChildren); g != e {
		t.Errorf("Wrong number of children: %d vs %d", g, e)
	}

	for _, child := range expectedChildren {
		if _, ok := children1[child]; !ok {
			t.Errorf("Couldn't find child %s", child)
		}
	}

	if !reflect.DeepEqual(children1, children2) {
		t.Fatalf("Users 1 and 2 see different children: %v vs %v",
			children1, children2)
	}
}

// Tests that two users can make independent writes while forked, and
// conflict resolution will merge them correctly.
func TestBasicCRFileConflict(t *testing.T) {
	// simulate two users
	var userName1, userName2 libkb.NormalizedUsername = "u1", "u2"
	config1, uid1, ctx := kbfsOpsConcurInit(t, userName1, userName2)
	defer CheckConfigAndShutdown(t, config1)

	config2 := ConfigAsUser(config1.(*ConfigLocal), userName2)
	defer CheckConfigAndShutdown(t, config2)
	uid2, err := config2.KBPKI().GetCurrentUID(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	config2.SetClock(&TestClock{now})

	h := NewTlfHandle()
	h.Writers = append(h.Writers, uid1)
	h.Writers = append(h.Writers, uid2)

	// user1 creates a file in a shared dir
	kbfsOps1 := config1.KBFSOps()
	rootNode1, _, err :=
		kbfsOps1.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	dirA1, _, err := kbfsOps1.CreateDir(ctx, rootNode1, "a")
	if err != nil {
		t.Fatalf("Couldn't create dir: %v", err)
	}
	fileB1, _, err := kbfsOps1.CreateFile(ctx, dirA1, "b", false)
	if err != nil {
		t.Fatalf("Couldn't create file: %v", err)
	}

	// look it up on user2
	kbfsOps2 := config2.KBFSOps()
	rootNode2, _, err :=
		kbfsOps2.GetOrCreateRootNodeForHandle(ctx, h, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	dirA2, _, err := kbfsOps2.Lookup(ctx, rootNode2, "a")
	if err != nil {
		t.Fatalf("Couldn't lookup dir: %v", err)
	}
	fileB2, _, err := kbfsOps2.Lookup(ctx, dirA2, "b")
	if err != nil {
		t.Fatalf("Couldn't lookup file: %v", err)
	}

	// disable updates on user 2
	c, err := DisableUpdatesForTesting(config2, rootNode2.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't disable updates: %v", err)
	}

	// User 1 writes the file
	data1 := []byte{1, 2, 3, 4, 5}
	err = kbfsOps1.Write(ctx, fileB1, data1, 0)
	if err != nil {
		t.Fatalf("Couldn't write file: %v", err)
	}
	err = kbfsOps1.Sync(ctx, fileB1)
	if err != nil {
		t.Fatalf("Couldn't sync file: %v", err)
	}

	// User 2 makes a new different file
	data2 := []byte{5, 4, 3, 2, 1}
	err = kbfsOps2.Write(ctx, fileB2, data2, 0)
	if err != nil {
		t.Fatalf("Couldn't write file: %v", err)
	}
	err = kbfsOps2.Sync(ctx, fileB2)
	if err != nil {
		t.Fatalf("Couldn't sync file: %v", err)
	}

	// re-enable updates, and wait for CR to complete
	c <- struct{}{}
	err = kbfsOps2.SyncFromServer(ctx, rootNode2.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't sync from server: %v", err)
	}

	err = kbfsOps1.SyncFromServer(ctx, rootNode1.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't sync from server: %v", err)
	}

	// Make sure they both see the same set of children
	expectedChildren := []string{
		"b",
		"b.conflict.u2." + now.Format(time.RFC3339Nano),
	}
	children1, err := kbfsOps1.GetDirChildren(ctx, dirA1)
	if err != nil {
		t.Fatalf("Couldn't get children: %v", err)
	}

	children2, err := kbfsOps2.GetDirChildren(ctx, dirA2)
	if err != nil {
		t.Fatalf("Couldn't get children: %v", err)
	}

	if g, e := len(children1), len(expectedChildren); g != e {
		t.Errorf("Wrong number of children: %d vs %d", g, e)
	}

	for _, child := range expectedChildren {
		if _, ok := children1[child]; !ok {
			t.Errorf("Couldn't find child %s", child)
		}
	}

	if !reflect.DeepEqual(children1, children2) {
		t.Fatalf("Users 1 and 2 see different children: %v vs %v",
			children1, children2)
	}
}
