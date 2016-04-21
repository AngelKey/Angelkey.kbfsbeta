package libkbfs

import (
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol"
	"github.com/keybase/go-codec/codec"
	"github.com/stretchr/testify/require"
)

type privateMetadataFuture struct {
	PrivateMetadata
	Dir dirEntryFuture
	extra
}

func (pmf privateMetadataFuture) toCurrent() PrivateMetadata {
	pm := pmf.PrivateMetadata
	pm.Dir = DirEntry(pmf.Dir.toCurrent())
	pm.Changes.Ops = make(opsList, len(pmf.Changes.Ops))
	for i, opFuture := range pmf.Changes.Ops {
		currentOp := opFuture.(futureStruct).toCurrentStruct()
		// A generic version of "v := currentOp; ...Ops[i] = &v".
		v := reflect.New(reflect.TypeOf(currentOp))
		v.Elem().Set(reflect.ValueOf(currentOp))
		pm.Changes.Ops[i] = v.Interface().(op)
	}
	return pm
}

func (pmf privateMetadataFuture) toCurrentStruct() currentStruct {
	return pmf.toCurrent()
}

func makeFakePrivateMetadataFuture(t *testing.T) privateMetadataFuture {
	createOp := makeFakeCreateOpFuture(t)
	rmOp := makeFakeRmOpFuture(t)
	renameOp := makeFakeRenameOpFuture(t)
	syncOp := makeFakeSyncOpFuture(t)
	setAttrOp := makeFakeSetAttrOpFuture(t)
	resolutionOp := makeFakeResolutionOpFuture(t)
	rekeyOp := makeFakeRekeyOpFuture(t)
	gcOp := makeFakeGcOpFuture(t)

	pmf := privateMetadataFuture{
		PrivateMetadata{
			DirEntry{},
			MakeTLFPrivateKey([32]byte{0xb}),
			BlockChanges{
				makeFakeBlockInfo(t),
				opsList{
					&createOp,
					&rmOp,
					&renameOp,
					&syncOp,
					&setAttrOp,
					&resolutionOp,
					&rekeyOp,
					&gcOp,
				},
				0,
			},
			codec.UnknownFieldSetHandler{},
			BlockChanges{},
		},
		makeFakeDirEntryFuture(t),
		makeExtraOrBust("PrivateMetadata", t),
	}
	return pmf
}

func TestPrivateMetadataUnknownFields(t *testing.T) {
	testStructUnknownFields(t, makeFakePrivateMetadataFuture(t))
}

// Test that GetTlfHandle() works properly for public TLFs.
func TestRootMetadataGetTlfHandlePublic(t *testing.T) {
	h := makeTestTlfHandle(t, 14, true)
	tlfID := FakeTlfID(0, true)
	rmd := NewRootMetadata(h, tlfID)
	dirHandle := rmd.GetTlfHandle()
	require.Equal(t, h, dirHandle)
}

// Test that GetTlfHandle() works properly for non-public TLFs.
func TestRootMetadataGetTlfHandlePrivate(t *testing.T) {
	h := makeTestTlfHandle(t, 14, false)
	tlfID := FakeTlfID(0, false)
	rmd := NewRootMetadata(h, tlfID)
	dirHandle := rmd.GetTlfHandle()
	require.Equal(t, h, dirHandle)
}

// Test that key generations work as expected for private TLFs.
func TestRootMetadataLatestKeyGenerationPrivate(t *testing.T) {
	tlfID := FakeTlfID(0, false)
	h := makeTestTlfHandle(t, 14, false)
	rmd := NewRootMetadata(h, tlfID)
	if rmd.LatestKeyGeneration() != 0 {
		t.Errorf("Expected key generation to be invalid (0)")
	}
	AddNewEmptyKeysOrBust(t, rmd)
	if rmd.LatestKeyGeneration() != FirstValidKeyGen {
		t.Errorf("Expected key generation to be valid(%d)", FirstValidKeyGen)
	}
}

// Test that key generations work as expected for public TLFs.
func TestRootMetadataLatestKeyGenerationPublic(t *testing.T) {
	tlfID := FakeTlfID(0, true)
	h := makeTestTlfHandle(t, 14, true)
	rmd := NewRootMetadata(h, tlfID)
	if rmd.LatestKeyGeneration() != PublicKeyGen {
		t.Errorf("Expected key generation to be public (%d)", PublicKeyGen)
	}
}

// Test that old encoded WriterMetadata objects (i.e., without any
// extra fields) can be deserialized and serialized to the same form,
// which is important for RootMetadata.VerifyWriterMetadata().
func TestWriterMetadataUnchangedEncoding(t *testing.T) {
	encodedWm := []byte{
		0x89, 0xa3, 0x42, 0x49, 0x44, 0xc4, 0x10, 0x0,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0xa9,
		0x44, 0x69, 0x73, 0x6b, 0x55, 0x73, 0x61, 0x67,
		0x65, 0x64, 0xa2, 0x49, 0x44, 0xc4, 0x10, 0x1,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x16, 0xb3,
		0x4c, 0x61, 0x73, 0x74, 0x4d, 0x6f, 0x64, 0x69,
		0x66, 0x79, 0x69, 0x6e, 0x67, 0x57, 0x72, 0x69,
		0x74, 0x65, 0x72, 0xa4, 0x75, 0x69, 0x64, 0x31,
		0xa8, 0x52, 0x65, 0x66, 0x42, 0x79, 0x74, 0x65,
		0x73, 0x63, 0xaa, 0x55, 0x6e, 0x72, 0x65, 0x66,
		0x42, 0x79, 0x74, 0x65, 0x73, 0x65, 0xa6, 0x57,
		0x46, 0x6c, 0x61, 0x67, 0x73, 0xa, 0xa7, 0x57,
		0x72, 0x69, 0x74, 0x65, 0x72, 0x73, 0x92, 0xa4,
		0x75, 0x69, 0x64, 0x31, 0xa4, 0x75, 0x69, 0x64,
		0x32, 0xa4, 0x64, 0x61, 0x74, 0x61, 0xc4, 0x2,
		0xa, 0xb,
	}

	expectedWm := WriterMetadata{
		SerializedPrivateMetadata: []byte{0xa, 0xb},
		LastModifyingWriter:       "uid1",
		Writers:                   []keybase1.UID{"uid1", "uid2"},
		ID:                        FakeTlfID(1, false),
		BID:                       NullBranchID,
		WFlags:                    0xa,
		DiskUsage:                 100,
		RefBytes:                  99,
		UnrefBytes:                101,
	}

	c := NewCodecMsgpack()

	var wm WriterMetadata
	err := c.Decode(encodedWm, &wm)
	require.Nil(t, err)

	require.Equal(t, expectedWm, wm)

	buf, err := c.Encode(wm)
	require.Nil(t, err)
	require.Equal(t, encodedWm, buf)
}

// Test that WriterMetadata has only a fixed (frozen) set of fields.
func TestWriterMetadataEncodedFields(t *testing.T) {
	sa1, _ := libkb.NormalizeSocialAssertion("uid1@twitter")
	sa2, _ := libkb.NormalizeSocialAssertion("uid2@twitter")
	// Usually exactly one of Writers/WKeys is filled in, but we
	// fill in both here for testing.
	wm := WriterMetadata{
		ID:      FakeTlfID(0xa, false),
		Writers: []keybase1.UID{"uid1", "uid2"},
		WKeys:   TLFWriterKeyGenerations{{}},
		Extra: WriterMetadataExtra{
			UnresolvedWriters: []keybase1.SocialAssertion{sa1, sa2},
		},
	}

	c := NewCodecMsgpack()

	buf, err := c.Encode(wm)
	require.Nil(t, err)

	var m map[string]interface{}
	err = c.Decode(buf, &m)
	require.Nil(t, err)

	expectedFields := []string{
		"BID",
		"DiskUsage",
		"ID",
		"LastModifyingWriter",
		"RefBytes",
		"UnrefBytes",
		"WFlags",
		"WKeys",
		"Writers",
		"data",
		"x",
	}

	var fields []string
	for field := range m {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	require.Equal(t, expectedFields, fields)
}

type writerMetadataExtraFuture struct {
	WriterMetadataExtra
	extra
}

func (wmef writerMetadataExtraFuture) toCurrent() WriterMetadataExtra {
	return wmef.WriterMetadataExtra
}

type tlfWriterKeyGenerationsFuture []*tlfWriterKeyBundleFuture

func (wkgf tlfWriterKeyGenerationsFuture) toCurrent() TLFWriterKeyGenerations {
	wkg := make(TLFWriterKeyGenerations, len(wkgf))
	for i, wkbf := range wkgf {
		wkb := wkbf.toCurrent()
		wkg[i] = wkb
	}
	return wkg
}

type writerMetadataFuture struct {
	WriterMetadata
	// Override WriterMetadata.WKeys.
	WKeys tlfWriterKeyGenerationsFuture
	// Override WriterMetadata.Extra.
	Extra writerMetadataExtraFuture `codec:"x,omitempty,omitemptycheckstruct"`
}

func (wmf writerMetadataFuture) toCurrent() WriterMetadata {
	wm := wmf.WriterMetadata
	wm.WKeys = wmf.WKeys.toCurrent()
	wm.Extra = wmf.Extra.toCurrent()
	return wm
}

func (wmf writerMetadataFuture) toCurrentStruct() currentStruct {
	return wmf.toCurrent()
}

func makeFakeWriterMetadataFuture(t *testing.T) writerMetadataFuture {
	wmd := WriterMetadata{
		// This needs to be list format so it fails to compile if new fields
		// are added, effectively checking at compile time whether new fields
		// have been added
		[]byte{0xa, 0xb},
		"uid1",
		[]keybase1.UID{"uid1", "uid2"},
		nil,
		FakeTlfID(1, false),
		NullBranchID,
		0xa,
		100,
		99,
		101,
		WriterMetadataExtra{},
	}
	wkb := makeFakeTLFWriterKeyBundleFuture(t)
	sa, _ := libkb.NormalizeSocialAssertion("foo@twitter")
	return writerMetadataFuture{
		wmd,
		tlfWriterKeyGenerationsFuture{&wkb},
		writerMetadataExtraFuture{
			WriterMetadataExtra{
				// This needs to be list format so it fails to compile if new
				// fields are added, effectively checking at compile time
				// whether new fields have been added
				[]keybase1.SocialAssertion{sa},
				codec.UnknownFieldSetHandler{},
			},
			makeExtraOrBust("WriterMetadata", t),
		},
	}
}

func TestWriterMetadataUnknownFields(t *testing.T) {
	testStructUnknownFields(t, makeFakeWriterMetadataFuture(t))
}

type tlfReaderKeyGenerationsFuture []*tlfReaderKeyBundleFuture

func (rkgf tlfReaderKeyGenerationsFuture) toCurrent() TLFReaderKeyGenerations {
	rkg := make(TLFReaderKeyGenerations, len(rkgf))
	for i, rkbf := range rkgf {
		rkb := rkbf.toCurrent()
		rkg[i] = rkb
	}
	return rkg
}

// rootMetadataWrapper exists only to add extra depth to fields
// in RootMetadata, so that they may be overridden in
// rootMetadataFuture.
type rootMetadataWrapper struct {
	RootMetadata
}

type rootMetadataFuture struct {
	// Override RootMetadata.WriterMetadata. Put it first to work
	// around a bug in codec's field lookup code.
	//
	// TODO: Report and fix this bug upstream.
	writerMetadataFuture

	rootMetadataWrapper
	// Override RootMetadata.RKeys.
	RKeys tlfReaderKeyGenerationsFuture `codec:",omitempty"`
	extra
}

func (rmf *rootMetadataFuture) toCurrent() *RootMetadata {
	rm := rmf.rootMetadataWrapper.RootMetadata
	rm.WriterMetadata = WriterMetadata(rmf.writerMetadataFuture.toCurrent())
	rm.RKeys = rmf.RKeys.toCurrent()
	return &rm
}

func (rmf *rootMetadataFuture) toCurrentStruct() currentStruct {
	return rmf.toCurrent()
}

func makeFakeRootMetadataFuture(t *testing.T) *rootMetadataFuture {
	wmf := makeFakeWriterMetadataFuture(t)
	rkb := makeFakeTLFReaderKeyBundleFuture(t)
	h, err := DefaultHash([]byte("fake buf"))
	require.Nil(t, err)
	sa, _ := libkb.NormalizeSocialAssertion("bar@github")
	rmf := rootMetadataFuture{
		wmf,
		rootMetadataWrapper{
			RootMetadata{
				// This needs to be list format so it fails to compile if new
				// fields are added, effectively checking at compile time
				// whether new fields have been added
				WriterMetadata{},
				SignatureInfo{
					100,
					[]byte{0xc},
					MakeFakeVerifyingKeyOrBust("fake kid"),
				},
				"uid1",
				0xb,
				5,
				MdID{h},
				nil,
				[]keybase1.SocialAssertion{sa},
				codec.UnknownFieldSetHandler{},
				PrivateMetadata{},
				nil,
				sync.RWMutex{},
				MdID{},
			},
		},
		[]*tlfReaderKeyBundleFuture{&rkb},
		makeExtraOrBust("RootMetadata", t),
	}
	return &rmf
}

func TestRootMetadataUnknownFields(t *testing.T) {
	testStructUnknownFields(t, makeFakeRootMetadataFuture(t))
}
