package libkbfs

import (
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/keybase/client/go/libkb"
	keybase1 "github.com/keybase/client/protocol/go"
	"golang.org/x/net/context"
)

func mdOpsInit(t *testing.T) (mockCtrl *gomock.Controller,
	config *ConfigMock, ctx context.Context) {
	ctr := NewSafeTestReporter(t)
	mockCtrl = gomock.NewController(ctr)
	config = NewConfigMock(mockCtrl, ctr)
	mdops := &MDOpsStandard{config}
	config.SetMDOps(mdops)
	ctx = context.Background()
	return
}

func mdOpsShutdown(mockCtrl *gomock.Controller, config *ConfigMock) {
	config.ctr.CheckForFailures()
	mockCtrl.Finish()
}

func newDir(t *testing.T, config *ConfigMock, x byte, share bool, public bool) (
	TlfID, *TlfHandle, *RootMetadataSigned) {
	revision := MetadataRevision(1)
	id, h, rmds := NewFolder(t, x, revision, share, public)
	expectUsernameCalls(h, config)
	config.mockKbpki.EXPECT().GetCurrentUID(gomock.Any()).AnyTimes().
		Return(h.Writers[0], nil)
	return id, h, rmds
}

func verifyMDForPublic(config *ConfigMock, rmds *RootMetadataSigned,
	id TlfID, hasVerifyingKeyErr error, verifyErr error) {
	config.mockCodec.EXPECT().Decode(rmds.MD.SerializedPrivateMetadata, &rmds.MD.data).
		Return(nil)

	packedData := []byte{4, 3, 2, 1}
	config.mockCodec.EXPECT().Encode(rmds.MD).Return(packedData, nil)
	config.mockKbpki.EXPECT().HasVerifyingKey(gomock.Any(), gomock.Any(),
		gomock.Any()).AnyTimes().Return(hasVerifyingKeyErr)
	if hasVerifyingKeyErr == nil {
		config.mockCrypto.EXPECT().Verify(packedData, rmds.SigInfo).Return(verifyErr)
	}
}

func verifyMDForPrivate(config *ConfigMock, rmds *RootMetadataSigned,
	id TlfID) {
	config.mockCodec.EXPECT().Decode(rmds.MD.SerializedPrivateMetadata, gomock.Any()).
		Return(nil)
	expectGetTLFCryptKeyForMDDecryption(config, &rmds.MD)
	config.mockCrypto.EXPECT().DecryptPrivateMetadata(
		gomock.Any(), TLFCryptKey{}).Return(&rmds.MD.data, nil)

	packedData := []byte{4, 3, 2, 1}
	config.mockCodec.EXPECT().Encode(rmds.MD).Return(packedData, nil)
	config.mockKbpki.EXPECT().HasVerifyingKey(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().Return(nil)
	config.mockCrypto.EXPECT().Verify(packedData, rmds.SigInfo).Return(nil)
}

func putMDForPublic(config *ConfigMock, rmds *RootMetadataSigned,
	id TlfID) {
	packedData := []byte{4, 3, 2, 1}
	config.mockCodec.EXPECT().Encode(rmds.MD.data).Return(packedData, nil)
	config.mockCodec.EXPECT().Encode(gomock.Any()).AnyTimes().
		Return([]byte{0}, nil)

	config.mockCrypto.EXPECT().Sign(gomock.Any(), gomock.Any()).
		Return(SignatureInfo{}, nil)

	config.mockMdserv.EXPECT().Put(gomock.Any(), gomock.Any()).Return(nil)
	config.mockMdserv.EXPECT().PruneUnmerged(gomock.Any(), gomock.Any()).Return(nil)
}

func putMDForPrivate(config *ConfigMock, rmds *RootMetadataSigned,
	id TlfID) {
	expectGetTLFCryptKeyForEncryption(config, &rmds.MD)
	config.mockCrypto.EXPECT().EncryptPrivateMetadata(
		&rmds.MD.data, TLFCryptKey{}).Return(EncryptedPrivateMetadata{}, nil)

	packedData := []byte{4, 3, 2, 1}
	config.mockCodec.EXPECT().Encode(gomock.Any()).Return(packedData, nil).Times(2)

	config.mockCrypto.EXPECT().Sign(gomock.Any(), gomock.Any()).Return(SignatureInfo{}, nil)

	config.mockMdserv.EXPECT().Put(gomock.Any(), gomock.Any()).Return(nil)
}

func TestMDOpsGetForHandlePublicSuccess(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, and one to verify it
	id, h, rmds := newDir(t, config, 1, false, true)

	config.mockMdserv.EXPECT().GetForHandle(ctx, h, false).Return(NullTlfID, rmds, nil)
	verifyMDForPublic(config, rmds, id, nil, nil)

	if rmd2, err := config.MDOps().GetForHandle(ctx, h); err != nil {
		t.Errorf("Got error on get: %v", err)
	} else if rmd2.ID != id {
		t.Errorf("Got back wrong id on get: %v (expected %v)", rmd2.ID, id)
	} else if rmd2 != &rmds.MD {
		t.Errorf("Got back wrong data on get: %v (expected %v)", rmd2, &rmds.MD)
	}
}

func TestMDOpsGetForHandlePrivateSuccess(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, and one to verify it
	id, h, rmds := newDir(t, config, 1, true, false)

	config.mockMdserv.EXPECT().GetForHandle(ctx, h, false).Return(NullTlfID, rmds, nil)
	verifyMDForPrivate(config, rmds, id)

	if rmd2, err := config.MDOps().GetForHandle(ctx, h); err != nil {
		t.Errorf("Got error on get: %v", err)
	} else if rmd2.ID != id {
		t.Errorf("Got back wrong id on get: %v (expected %v)", rmd2.ID, id)
	} else if rmd2 != &rmds.MD {
		t.Errorf("Got back wrong data on get: %v (expected %v)", rmd2, &rmds.MD)
	}
}

func TestMDOpsGetForHandlePublicFailFindKey(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, and one to verify it
	id, h, rmds := newDir(t, config, 1, false, true)

	config.mockMdserv.EXPECT().GetForHandle(ctx, h, false).Return(NullTlfID, rmds, nil)

	verifyMDForPublic(config, rmds, id, KeyNotFoundError{}, nil)

	_, err := config.MDOps().GetForHandle(ctx, h)
	if _, ok := err.(KeyNotFoundError); !ok {
		t.Errorf("Got unexpected error on get: %v", err)
	}
}

func TestMDOpsGetForHandlePublicFailVerify(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, and one to verify it
	id, h, rmds := newDir(t, config, 1, false, true)

	config.mockMdserv.EXPECT().GetForHandle(ctx, h, false).Return(NullTlfID, rmds, nil)

	expectedErr := libkb.VerificationError{}
	verifyMDForPublic(config, rmds, id, nil, expectedErr)

	if _, err := config.MDOps().GetForHandle(ctx, h); err != expectedErr {
		t.Errorf("Got unexpected error on get: %v", err)
	}
}

func TestMDOpsGetForHandleFailGet(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, and fail it
	_, h, _ := newDir(t, config, 1, true, false)
	err := errors.New("Fake fail")

	// only the get happens, no verify needed with a blank sig
	config.mockMdserv.EXPECT().GetForHandle(ctx, h, false).Return(NullTlfID, nil, err)

	if _, err2 := config.MDOps().GetForHandle(ctx, h); err2 != err {
		t.Errorf("Got bad error on get: %v", err2)
	}
}

func TestMDOpsGetForHandleFailHandleCheck(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, and one to verify it, and fail that one
	id, h, rmds := newDir(t, config, 1, true, false)
	rmds.MD.cachedTlfHandle = NewTlfHandle()

	// add a new writer after the MD was made, to force a failure
	newWriter := keybase1.MakeTestUID(100)
	h.Writers = append(h.Writers, newWriter)
	expectUsernameCall(newWriter, config)
	config.mockMdserv.EXPECT().GetForHandle(ctx, h, false).Return(NullTlfID, rmds, nil)
	verifyMDForPrivate(config, rmds, id)

	if _, err := config.MDOps().GetForHandle(ctx, h); err == nil {
		t.Errorf("Got no error on bad handle check test")
	} else if _, ok := err.(MDMismatchError); !ok {
		t.Errorf("Got unexpected error on bad handle check test: %v", err)
	}
}

func TestMDOpsGetSuccess(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, and one to verify it
	id, _, rmds := newDir(t, config, 1, true, false)

	config.mockMdserv.EXPECT().GetForTLF(ctx, id, false).Return(rmds, nil)
	verifyMDForPrivate(config, rmds, id)

	if rmd2, err := config.MDOps().GetForTLF(ctx, id); err != nil {
		t.Errorf("Got error on get: %v", err)
	} else if rmd2 != &rmds.MD {
		t.Errorf("Got back wrong data on get: %v (expected %v)", rmd2, &rmds.MD)
	}
}

func TestMDOpsGetBlankSigSuccess(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, give back a blank sig that doesn't need
	// verification
	id, h, _ := newDir(t, config, 1, true, false)
	rmd := NewRootMetadataForTest(h, id)
	rmds := &RootMetadataSigned{
		MD: *rmd,
	}

	// only the get happens, no verify needed with a blank sig
	config.mockMdserv.EXPECT().GetForTLF(ctx, id, false).Return(rmds, nil)

	if rmd2, err := config.MDOps().GetForTLF(ctx, id); err != nil {
		t.Errorf("Got error on get: %v", err)
	} else if rmd2 != &rmds.MD {
		t.Errorf("Got back wrong data on get: %v (expected %v)", rmd2, &rmds.MD)
	}
}

func TestMDOpsGetFailGet(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, and fail it
	id, _, _ := newDir(t, config, 1, true, false)
	err := errors.New("Fake fail")

	// only the get happens, no verify needed with a blank sig
	config.mockMdserv.EXPECT().GetForTLF(ctx, id, false).Return(nil, err)

	if _, err2 := config.MDOps().GetForTLF(ctx, id); err2 != err {
		t.Errorf("Got bad error on get: %v", err2)
	}
}

func TestMDOpsGetFailIdCheck(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, and one to verify it, and fail that one
	_, _, rmds := newDir(t, config, 1, true, false)
	id2, _, _ := newDir(t, config, 2, true, false)

	config.mockMdserv.EXPECT().GetForTLF(ctx, id2, false).Return(rmds, nil)

	if _, err := config.MDOps().GetForTLF(ctx, id2); err == nil {
		t.Errorf("Got no error on bad id check test")
	} else if _, ok := err.(MDMismatchError); !ok {
		t.Errorf("Got unexpected error on bad id check test: %v", err)
	}
}

func testMDOpsGetRangeSuccess(t *testing.T, fromStart bool) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, and one to verify it
	id, _, rmds1 := newDir(t, config, 1, true, false)
	_, _, rmds2 := newDir(t, config, 1, true, false)
	rmds2.MD.mdID = fakeMdID(42)
	rmds1.MD.PrevRoot = rmds2.MD.mdID
	rmds1.MD.Revision = 102
	_, _, rmds3 := newDir(t, config, 1, true, false)
	rmds3.MD.mdID = fakeMdID(43)
	rmds2.MD.PrevRoot = rmds3.MD.mdID
	rmds2.MD.Revision = 101
	mdID4 := fakeMdID(44)
	rmds3.MD.PrevRoot = mdID4
	rmds3.MD.Revision = 100

	start, stop := MetadataRevision(100), MetadataRevision(102)
	if fromStart {
		start = 0
	}

	allRMDSs := []*RootMetadataSigned{rmds3, rmds2, rmds1}

	config.mockMdserv.EXPECT().GetRange(ctx, id, false, start, stop).
		Return(allRMDSs, nil)
	verifyMDForPrivate(config, rmds3, id)
	verifyMDForPrivate(config, rmds2, id)
	verifyMDForPrivate(config, rmds1, id)

	allRMDs, err := config.MDOps().GetRange(ctx, id, start, stop)
	if err != nil {
		t.Errorf("Got error on GetRange: %v", err)
	} else if len(allRMDs) != 3 {
		t.Errorf("Got back wrong number of RMDs: %d", len(allRMDs))
	}
}

func TestMDOpsGetRangeSuccess(t *testing.T) {
	testMDOpsGetRangeSuccess(t, false)
}

func TestMDOpsGetRangeFromStartSuccess(t *testing.T) {
	testMDOpsGetRangeSuccess(t, true)
}

func TestMDOpsGetRangeFailBadPrevRoot(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to fetch MD, and one to verify it
	id, _, rmds1 := newDir(t, config, 1, true, false)
	_, _, rmds2 := newDir(t, config, 1, true, false)
	rmds2.MD.mdID = fakeMdID(42)
	rmds1.MD.PrevRoot = fakeMdID(46) // points to some random ID
	rmds1.MD.Revision = 202
	_, _, rmds3 := newDir(t, config, 1, true, false)
	rmds3.MD.mdID = fakeMdID(43)
	rmds2.MD.PrevRoot = rmds3.MD.mdID
	rmds2.MD.Revision = 201
	mdID4 := fakeMdID(44)
	rmds3.MD.PrevRoot = mdID4
	rmds3.MD.Revision = 200

	allRMDSs := []*RootMetadataSigned{rmds3, rmds2, rmds1}

	start, stop := MetadataRevision(200), MetadataRevision(202)
	config.mockMdserv.EXPECT().GetRange(ctx, id, false, start, stop).
		Return(allRMDSs, nil)
	verifyMDForPrivate(config, rmds3, id)
	verifyMDForPrivate(config, rmds2, id)

	_, err := config.MDOps().GetRange(ctx, id, start, stop)
	if err == nil {
		t.Errorf("Got no expected error on GetSince")
	} else if _, ok := err.(MDMismatchError); !ok {
		t.Errorf("Got unexpected error on GetSince with bad PrevRoot chain: %v",
			err)
	}
}

func TestMDOpsPutPublicSuccess(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to sign MD, and one to put it
	id, _, rmds := newDir(t, config, 1, false, true)
	putMDForPublic(config, rmds, id)

	if err := config.MDOps().Put(ctx, &rmds.MD); err != nil {
		t.Errorf("Got error on put: %v", err)
	}
}

func TestMDOpsPutPrivateSuccess(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to sign MD, and one to put it
	id, _, rmds := newDir(t, config, 1, true, false)
	putMDForPrivate(config, rmds, id)

	if err := config.MDOps().PutUnmerged(ctx, &rmds.MD); err != nil {
		t.Errorf("Got error on put: %v", err)
	}
}

func TestMDOpsPutFailEncode(t *testing.T) {
	mockCtrl, config, ctx := mdOpsInit(t)
	defer mdOpsShutdown(mockCtrl, config)

	// expect one call to sign MD, and fail it
	id, h, _ := newDir(t, config, 1, true, false)
	rmd := NewRootMetadataForTest(h, id)

	expectGetTLFCryptKeyForEncryption(config, rmd)
	config.mockCrypto.EXPECT().EncryptPrivateMetadata(
		&rmd.data, TLFCryptKey{}).Return(EncryptedPrivateMetadata{}, nil)

	err := errors.New("Fake fail")
	config.mockCodec.EXPECT().Encode(gomock.Any()).Return(nil, err)

	if err2 := config.MDOps().Put(ctx, rmd); err2 != err {
		t.Errorf("Got bad error on put: %v", err2)
	}
}
