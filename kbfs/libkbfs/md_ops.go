package libkbfs

import (
	"fmt"

	"github.com/keybase/client/go/libkb"
	"golang.org/x/net/context"
)

// MDOpsStandard provides plaintext RootMetadata objects to upper
// layers, and processes RootMetadataSigned objects (encrypted and
// signed) suitable for passing to/from the MDServer backend.
type MDOpsStandard struct {
	config Config
}

// convertVerifyingKeyError gives a better error when the TLF was
// signed by a key that is no longer associated with the last writer.
func (md *MDOpsStandard) convertVerifyingKeyError(ctx context.Context,
	rmds *RootMetadataSigned, err error) error {
	if _, ok := err.(KeyNotFoundError); !ok {
		return err
	}

	tlf := rmds.MD.GetTlfHandle().GetCanonicalPath(ctx, md.config)
	writer, nameErr := md.config.KBPKI().GetNormalizedUsername(ctx,
		rmds.MD.LastModifyingWriter)
	if nameErr != nil {
		writer = libkb.NormalizedUsername("uid: " +
			rmds.MD.LastModifyingWriter.String())
	}
	return UnverifiableTlfUpdateError{tlf, writer, err}
}

func (md *MDOpsStandard) processMetadata(ctx context.Context,
	handle *TlfHandle, rmds *RootMetadataSigned) error {
	crypto := md.config.Crypto()
	codec := md.config.Codec()
	// verify signature and deserialize root data, if the sig is not blank.
	// a blank sig means this is a brand new MD object, nothing to check
	if rmds.IsInitialized() {
		if handle.IsPublic() {
			if err := codec.Decode(rmds.MD.SerializedPrivateMetadata, &rmds.MD.data); err != nil {
				return err
			}
		} else {
			// decrypt the root data for non-public directories
			var encryptedPrivateMetadata EncryptedPrivateMetadata
			if err := codec.Decode(rmds.MD.SerializedPrivateMetadata, &encryptedPrivateMetadata); err != nil {
				return err
			}

			k, err := md.config.KeyManager().
				GetTLFCryptKeyForMDDecryption(ctx, &rmds.MD)

			privateMetadata := &PrivateMetadata{}
			if err != nil {
				// Get current UID.
				_, uid, uidErr := md.config.KBPKI().GetCurrentUserInfo(ctx)
				if uidErr != nil {
					return uidErr
				}
				isReader := handle.IsReader(uid)
				_, isSelfRekeyError := err.(NeedSelfRekeyError)
				_, isOtherRekeyError := err.(NeedOtherRekeyError)
				if isReader && (isOtherRekeyError || isSelfRekeyError) {
					// Rekey errors are expected if this client is a
					// valid folder participant but doesn't have the
					// shared crypt key.
				} else {
					return err
				}
			} else {
				privateMetadata, err = crypto.DecryptPrivateMetadata(encryptedPrivateMetadata, k)
				if err != nil {
					return err
				}
			}
			rmds.MD.data = *privateMetadata
		}

		// Make sure the last writer is really a valid writer
		writer := rmds.MD.LastModifyingWriter
		if !handle.IsWriter(writer) {
			return MDMismatchError{
				handle.ToString(ctx, md.config),
				fmt.Sprintf("Writer MD (id=%s) was written by a non-writer %s",
					rmds.MD.ID, writer)}
		}

		// re-marshal the WriterMetadata
		// TODO: can we somehow avoid the re-marshaling by saving the
		// marshalled metadata somewhere?
		buf, err := codec.Encode(rmds.MD.WriterMetadata)
		if err != nil {
			return err
		}

		// TODO: what do we do if the signature is from a revoked
		// key?
		kbpki := md.config.KBPKI()
		err = kbpki.HasVerifyingKey(ctx, writer,
			rmds.MD.WriterMetadataSigInfo.VerifyingKey,
			rmds.untrustedServerTimestamp)
		if err != nil {
			return md.convertVerifyingKeyError(ctx, rmds, err)
		}

		err = crypto.Verify(buf, rmds.MD.WriterMetadataSigInfo)
		if err != nil {
			return err
		}

		// Make sure the last user to change the blob is really a valid reader
		user := rmds.MD.LastModifyingUser
		if !handle.IsReader(user) {
			return MDMismatchError{
				handle.ToString(ctx, md.config),
				fmt.Sprintf("MD (id=%s) was changed by a non-reader %s",
					rmds.MD.ID, user),
			}
		}

		// re-marshal the whole RootMetadata
		// TODO: can we somehow avoid the re-marshaling by saving the
		// marshalled metadata somewhere?
		buf, err = codec.Encode(rmds.MD)
		if err != nil {
			return err
		}

		// TODO: what do we do if the signature is from a revoked
		// key?
		err = kbpki.HasVerifyingKey(ctx, user, rmds.SigInfo.VerifyingKey,
			rmds.untrustedServerTimestamp)
		if err != nil {
			return md.convertVerifyingKeyError(ctx, rmds, err)
		}

		err = crypto.Verify(buf, rmds.SigInfo)
		if err != nil {
			return err
		}
	}
	return nil
}

func (md *MDOpsStandard) getForHandle(ctx context.Context, handle *TlfHandle,
	mStatus MergeStatus) (
	*RootMetadata, error) {
	mdserv := md.config.MDServer()
	id, rmds, err := mdserv.GetForHandle(ctx, handle, mStatus)
	if err != nil {
		return nil, err
	}
	if rmds == nil {
		if mStatus == Unmerged {
			// don't automatically create unmerged MDs
			return nil, nil
		}
		// create one if it doesn't exist
		rmd := NewRootMetadata(handle, id)
		rmds = &RootMetadataSigned{MD: *rmd}
	}
	if err := md.processMetadata(ctx, handle, rmds); err != nil {
		return nil, err
	}
	if rmds.IsInitialized() {
		// Make the the signed-over UIDs in the latest Keys match the handle
		handleString := handle.ToString(ctx, md.config)
		fetchedHandleString := rmds.MD.GetTlfHandle().ToString(ctx, md.config)
		if fetchedHandleString != handleString {
			return nil, MDMismatchError{
				handleString,
				fmt.Sprintf("MD (id=%s) contained unexpected handle %s",
					rmds.MD.ID, fetchedHandleString),
			}
		}
	}
	return &rmds.MD, nil
}

// GetForHandle implements the MDOps interface for MDOpsStandard.
func (md *MDOpsStandard) GetForHandle(ctx context.Context, handle *TlfHandle) (
	*RootMetadata, error) {
	return md.getForHandle(ctx, handle, Merged)
}

// GetUnmergedForHandle implements the MDOps interface for MDOpsStandard.
func (md *MDOpsStandard) GetUnmergedForHandle(ctx context.Context, handle *TlfHandle) (
	*RootMetadata, error) {
	return md.getForHandle(ctx, handle, Unmerged)
}

func (md *MDOpsStandard) processMetadataWithID(ctx context.Context,
	id TlfID, bid BranchID, rmds *RootMetadataSigned) error {
	// Make sure the signed-over ID matches
	if id != rmds.MD.ID {
		return MDMismatchError{
			id.String(),
			fmt.Sprintf("MD contained unexpected folder id %s, expected %s",
				rmds.MD.ID.String(), id.String()),
		}
	}
	// Make sure the signed-over branch ID matches
	if bid != rmds.MD.BID {
		return MDMismatchError{
			id.String(),
			fmt.Sprintf("MD contained unexpected branch id %s, expected %s, "+
				"folder id %s", rmds.MD.BID.String(), bid.String(), id.String()),
		}
	}
	return md.processMetadata(ctx, rmds.MD.GetTlfHandle(), rmds)
}

func (md *MDOpsStandard) getForTLF(ctx context.Context, id TlfID,
	bid BranchID, mStatus MergeStatus) (*RootMetadata, error) {
	rmds, err := md.config.MDServer().GetForTLF(ctx, id, bid, mStatus)
	if err != nil {
		return nil, err
	}
	if rmds == nil {
		// Possible if mStatus is Unmerged
		return nil, nil
	}
	err = md.processMetadataWithID(ctx, id, bid, rmds)
	if err != nil {
		return nil, err
	}
	return &rmds.MD, nil
}

// GetForTLF implements the MDOps interface for MDOpsStandard.
func (md *MDOpsStandard) GetForTLF(ctx context.Context, id TlfID) (*RootMetadata, error) {
	return md.getForTLF(ctx, id, NullBranchID, Merged)
}

// GetUnmergedForTLF implements the MDOps interface for MDOpsStandard.
func (md *MDOpsStandard) GetUnmergedForTLF(ctx context.Context, id TlfID, bid BranchID) (
	*RootMetadata, error) {
	return md.getForTLF(ctx, id, bid, Unmerged)
}

func (md *MDOpsStandard) processRange(ctx context.Context, id TlfID,
	bid BranchID, rmds []*RootMetadataSigned) (
	[]*RootMetadata, error) {
	if rmds == nil {
		return nil, nil
	}

	// verify each of the MD objects, and verify the PrevRoot pointers
	// are correct
	lastRoot, lastRevision := MdID{}, MetadataRevision(0)
	rmd := make([]*RootMetadata, 0, len(rmds))
	for _, r := range rmds {
		currRoot, err := r.MD.MetadataID(md.config)
		if err != nil {
			return nil, err
		}
		//
		// make sure the chain is correct
		//
		// (1) check revision
		if r.MD.Revision != lastRevision+1 && lastRevision != 0 {
			return nil, MDMismatchError{
				r.MD.GetTlfHandle().ToString(ctx, md.config),
				fmt.Sprintf("MD (id=%v) is at an unexpected revision (%d) "+
					"instead of %d", currRoot, r.MD.Revision.Number(),
					lastRevision.Number()+1),
			}
		}
		// (2) check PrevRoot pointer
		if r.MD.PrevRoot != lastRoot && lastRoot != (MdID{}) {
			return nil, MDMismatchError{
				r.MD.GetTlfHandle().ToString(ctx, md.config),
				fmt.Sprintf("MD (id=%v) points to an unexpected root (%v) "+
					"instead of %v", currRoot, r.MD.PrevRoot, lastRoot),
			}
		}

		err = md.processMetadataWithID(ctx, id, bid, r)
		if err != nil {
			return nil, err
		}
		lastRoot, lastRevision = currRoot, r.MD.Revision
		rmd = append(rmd, &r.MD)
	}

	// TODO: in the case where lastRoot == MdID{}, should we verify
	// that the starting PrevRoot points back to something that's
	// actually a valid part of this history?  If the MD signature is
	// indeed valid, this probably isn't a huge deal, but it may let
	// the server rollback or truncate unmerged history...

	return rmd, nil
}

func (md *MDOpsStandard) getRange(ctx context.Context, id TlfID,
	bid BranchID, mStatus MergeStatus, start, stop MetadataRevision) (
	[]*RootMetadata, error) {
	rmds, err := md.config.MDServer().GetRange(ctx, id, bid, mStatus, start,
		stop)
	if err != nil {
		return nil, err
	}
	rmd, err := md.processRange(ctx, id, bid, rmds)
	if err != nil {
		return nil, err
	}
	return rmd, nil
}

// GetRange implements the MDOps interface for MDOpsStandard.
func (md *MDOpsStandard) GetRange(ctx context.Context, id TlfID,
	start, stop MetadataRevision) ([]*RootMetadata, error) {
	return md.getRange(ctx, id, NullBranchID, Merged, start, stop)
}

// GetUnmergedRange implements the MDOps interface for MDOpsStandard.
func (md *MDOpsStandard) GetUnmergedRange(ctx context.Context, id TlfID,
	bid BranchID, start, stop MetadataRevision) ([]*RootMetadata, error) {
	return md.getRange(ctx, id, bid, Unmerged, start, stop)
}

func (md *MDOpsStandard) readyMD(ctx context.Context, rmd *RootMetadata) (
	rms *RootMetadataSigned, err error) {
	_, me, err := md.config.KBPKI().GetCurrentUserInfo(ctx)
	if err != nil {
		return nil, err
	}

	codec := md.config.Codec()
	crypto := md.config.Crypto()

	if rmd.ID.IsPublic() || !rmd.IsWriterMetadataCopiedSet() {
		// Record the last writer to modify this writer metadata
		rmd.LastModifyingWriter = me

		if rmd.ID.IsPublic() {
			// Encode the private metadata
			encodedPrivateMetadata, err := codec.Encode(rmd.data)
			if err != nil {
				return nil, err
			}
			rmd.SerializedPrivateMetadata = encodedPrivateMetadata
		} else if !rmd.IsWriterMetadataCopiedSet() {
			// Encrypt and encode the private metadata
			k, err := md.config.KeyManager().GetTLFCryptKeyForEncryption(ctx, rmd)
			if err != nil {
				return nil, err
			}
			encryptedPrivateMetadata, err := crypto.EncryptPrivateMetadata(&rmd.data, k)
			if err != nil {
				return nil, err
			}
			encodedEncryptedPrivateMetadata, err := codec.Encode(encryptedPrivateMetadata)
			if err != nil {
				return nil, err
			}
			rmd.SerializedPrivateMetadata = encodedEncryptedPrivateMetadata
		}

		// Sign the writer metadata
		buf, err := codec.Encode(rmd.WriterMetadata)
		if err != nil {
			return nil, err
		}

		sigInfo, err := crypto.Sign(ctx, buf)
		if err != nil {
			return nil, err
		}
		rmd.WriterMetadataSigInfo = sigInfo
	}

	// Record the last user to modify this metadata
	rmd.LastModifyingUser = me

	// encode the root metadata and sign it
	buf, err := codec.Encode(rmd)
	if err != nil {
		return nil, err
	}

	rmds := &RootMetadataSigned{}
	rmds.MD = *rmd
	// Sign normally using the local device private key
	sigInfo, err := crypto.Sign(ctx, buf)
	if err != nil {
		return nil, err
	}
	rmds.SigInfo = sigInfo

	return rmds, nil
}

// Put implements the MDOps interface for MDOpsStandard.
func (md *MDOpsStandard) Put(ctx context.Context, rmd *RootMetadata) error {
	rmds, err := md.readyMD(ctx, rmd)
	if err != nil {
		return err
	}
	err = md.config.MDServer().Put(ctx, rmds)
	if err != nil {
		return err
	}
	return nil
}

// PutUnmerged implements the MDOps interface for MDOpsStandard.
func (md *MDOpsStandard) PutUnmerged(ctx context.Context, rmd *RootMetadata, bid BranchID) error {
	rmd.WFlags |= MetadataFlagUnmerged
	rmd.BID = bid
	return md.Put(ctx, rmd)
}
