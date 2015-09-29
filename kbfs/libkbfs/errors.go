package libkbfs

import (
	"fmt"
	"syscall"

	"bazil.org/fuse"

	keybase1 "github.com/keybase/client/protocol/go"
	"golang.org/x/net/context"
)

// ErrorFile is the name of the virtual file in KBFS that should
// contain the last reported error(s).
var ErrorFile = ".kbfs_error"

// WrapError simply wraps an error in a fmt.Stringer interface, so
// that it can be reported.
type WrapError struct {
	Err error
}

// String implements the fmt.Stringer interface for WrapError
func (e WrapError) String() string {
	return e.Err.Error()
}

// NameExistsError indicates that the user tried to create an entry
// for a name that already existed in a subdirectory.
type NameExistsError struct {
	Name string
}

// Error implements the error interface for NameExistsError
func (e NameExistsError) Error() string {
	return fmt.Sprintf("%s already exists", e.Name)
}

// NoSuchNameError indicates that the user tried to access a
// subdirectory entry that doesn't exist.
type NoSuchNameError struct {
	Name string
}

// Error implements the error interface for NoSuchNameError
func (e NoSuchNameError) Error() string {
	return fmt.Sprintf("%s doesn't exist", e.Name)
}

// NoSuchUserError indicates that the given user couldn't be resolved.
type NoSuchUserError struct {
	Input string
}

// Error implements the error interface for NoSuchUserError
func (e NoSuchUserError) Error() string {
	return fmt.Sprintf("No such user matching %s", e.Input)
}

var _ fuse.ErrorNumber = NoSuchUserError{""}

// Errno implements the fuse.ErrorNumber interface for
// NoSuchUserError
func (e NoSuchUserError) Errno() fuse.Errno {
	return fuse.Errno(syscall.ENOENT)
}

// BadTLFNameError indicates a top-level folder name that has an
// incorrect format.
type BadTLFNameError struct {
	Name string
}

// Error implements the error interface for BadTLFNameError.
func (e BadTLFNameError) Error() string {
	return fmt.Sprintf("TLF name %s is in an incorrect format", e.Name)
}

// InvalidPathError indicates an invalid (i.e., empty) path was encountered.
type InvalidPathError struct{}

// Error implements the error interface for InvalidPathError.
func (e InvalidPathError) Error() string {
	return "Invalid path"
}

// DirNotEmptyError indicates that the user tried to unlink a
// subdirectory that was not empty.
type DirNotEmptyError struct {
	Name string
}

// Error implements the error interface for DirNotEmptyError
func (e DirNotEmptyError) Error() string {
	return fmt.Sprintf("Directory %s is not empty and can't be removed", e.Name)
}

var _ fuse.ErrorNumber = DirNotEmptyError{""}

// Errno implements the fuse.ErrorNumber interface for
// DirNotEmptyError
func (e DirNotEmptyError) Errno() fuse.Errno {
	return fuse.Errno(syscall.ENOTEMPTY)
}

// TlfAccessError that the user tried to perform an unpermitted
// operation on a top-level folder.
type TlfAccessError struct {
	ID TlfID
}

// Error implements the error interface for TlfAccessError
func (e TlfAccessError) Error() string {
	return fmt.Sprintf("Operation not permitted on folder %s", e.ID)
}

// RenameAcrossDirsError indicates that the user tried to do an atomic
// rename across directories.
type RenameAcrossDirsError struct {
}

// Error implements the error interface for RenameAcrossDirsError
func (e RenameAcrossDirsError) Error() string {
	return fmt.Sprintf("Cannot rename across directories")
}

// ErrorFileAccessError indicates that the user tried to perform an
// operation on the ErrorFile that is not allowed.
type ErrorFileAccessError struct {
}

// Error implements the error interface for ErrorFileAccessError
func (e ErrorFileAccessError) Error() string {
	return fmt.Sprintf("Operation not allowed on file %s", ErrorFile)
}

// ReadAccessError indicates that the user tried to read from a
// top-level folder without read permission.
type ReadAccessError struct {
	User string
	Dir  string
}

// Error implements the error interface for ReadAccessError
func (e ReadAccessError) Error() string {
	return fmt.Sprintf("%s does not have read access to directory %s",
		e.User, e.Dir)
}

var _ fuse.ErrorNumber = ReadAccessError{}

// Errno implements the fuse.ErrorNumber interface for
// ReadAccessError.
func (e ReadAccessError) Errno() fuse.Errno {
	return fuse.Errno(syscall.EACCES)
}

// WriteAccessError indicates that the user tried to read from a
// top-level folder without read permission.
type WriteAccessError struct {
	User string
	Dir  string
}

// Error implements the error interface for WriteAccessError
func (e WriteAccessError) Error() string {
	return fmt.Sprintf("%s does not have write access to directory %s",
		e.User, e.Dir)
}

var _ fuse.ErrorNumber = WriteAccessError{}

// Errno implements the fuse.ErrorNumber interface for
// WriteAccessError.
func (e WriteAccessError) Errno() fuse.Errno {
	return fuse.Errno(syscall.EACCES)
}

// NewReadAccessError constructs a ReadAccessError for the given
// directory and user.
func NewReadAccessError(ctx context.Context, config Config, dir *TlfHandle,
	uid keybase1.UID) error {
	dirname := dir.ToString(ctx, config)
	if name, err2 := config.KBPKI().GetNormalizedUsername(ctx, uid); err2 == nil {
		return ReadAccessError{string(name), dirname}
	}
	return ReadAccessError{uid.String(), dirname}
}

// NewWriteAccessError constructs a WriteAccessError for the given
// directory and user.
func NewWriteAccessError(ctx context.Context, config Config, dir *TlfHandle,
	uid keybase1.UID) error {
	dirname := dir.ToString(ctx, config)
	if name, err2 := config.KBPKI().GetNormalizedUsername(ctx, uid); err2 == nil {
		return WriteAccessError{string(name), dirname}
	}
	return WriteAccessError{uid.String(), dirname}
}

// NotDirError indicates that the user tried to perform a
// directory-specific operation on something that isn't a
// subdirectory.
type NotDirError struct {
	path path
}

// Error implements the error interface for NotDirError
func (e NotDirError) Error() string {
	return fmt.Sprintf("%s is not a directory (in folder %s)",
		&e.path, e.path.Tlf)
}

// NotFileError indicates that the user tried to perform a
// file-specific operation on something that isn't a file.
type NotFileError struct {
	path path
}

// Error implements the error interface for NotFileError
func (e NotFileError) Error() string {
	return fmt.Sprintf("%s is not a file (folder %s)", e.path, e.path.Tlf)
}

// BadDataError indicates that KBFS is storing corrupt data for a block.
type BadDataError struct {
	ID BlockID
}

// Error implements the error interface for BadDataError
func (e BadDataError) Error() string {
	return fmt.Sprintf("Bad data for block %v", e.ID)
}

// NoSuchBlockError indicates that a block for the associated ID doesn't exist.
type NoSuchBlockError struct {
	ID BlockID
}

// Error implements the error interface for NoSuchBlockError
func (e NoSuchBlockError) Error() string {
	return fmt.Sprintf("Couldn't get block %v", e.ID)
}

// BadCryptoError indicates that KBFS performed a bad crypto operation.
type BadCryptoError struct {
	ID BlockID
}

// Error implements the error interface for BadCryptoError
func (e BadCryptoError) Error() string {
	return fmt.Sprintf("Bad crypto for block %v", e.ID)
}

// BadCryptoMDError indicates that KBFS performed a bad crypto
// operation, specifically on a MD object.
type BadCryptoMDError struct {
	ID TlfID
}

// Error implements the error interface for BadCryptoMDError
func (e BadCryptoMDError) Error() string {
	return fmt.Sprintf("Bad crypto for the metadata of directory %v", e.ID)
}

// BadMDError indicates that the system is storing corrupt MD object
// for the given TLF ID.
type BadMDError struct {
	ID TlfID
}

// Error implements the error interface for BadMDError
func (e BadMDError) Error() string {
	return fmt.Sprintf("Wrong format for metadata for directory %v", e.ID)
}

// MDMissingDataError indicates that we are trying to take get the
// metadata ID of a MD object with no serialized data field.
type MDMissingDataError struct {
	ID TlfID
}

// Error implements the error interface for MDMissingDataError
func (e MDMissingDataError) Error() string {
	return fmt.Sprintf("No serialized private data in the metadata "+
		"for directory %v", e.ID)
}

// MDMismatchError indicates an inconsistent or unverifiable MD object
// for the given top-level folder.
type MDMismatchError struct {
	Dir string
	Err string
}

// Error implements the error interface for MDMismatchError
func (e MDMismatchError) Error() string {
	return fmt.Sprintf("Could not verify metadata for directory %s: %s",
		e.Dir, e.Err)
}

// NoSuchMDError indicates that there is no MD object for the given
// folder, revision, and merged status.
type NoSuchMDError struct {
	Tlf     TlfID
	Rev     MetadataRevision
	MStatus MergeStatus
}

// Error implements the error interface for NoSuchMDError
func (e NoSuchMDError) Error() string {
	return fmt.Sprintf("Couldn't get metadata for folder %v, revision %d, "+
		"%s", e.Tlf, e.Rev, e.MStatus)
}

// InvalidDataVersionError indicates that an invalid data version was
// used.
type InvalidDataVersionError struct {
	DataVer DataVer
}

// Error implements the error interface for InvalidDataVersionError.
func (e InvalidDataVersionError) Error() string {
	return fmt.Sprintf("Invalid data version %d", int(e.DataVer))
}

// NewDataVersionError indicates that the data at the given path has
// been written using a new data version that our client doesn't
// understand.
type NewDataVersionError struct {
	path    path
	DataVer DataVer
}

// Error implements the error interface for NewDataVersionError.
func (e NewDataVersionError) Error() string {
	return fmt.Sprintf(
		"The data at path %s is of a version (%d) that we can't read "+
			"(in folder %s)",
		e.path, e.DataVer, e.path.Tlf)
}

// InvalidKeyGenerationError indicates that an invalid key generation
// was used.
type InvalidKeyGenerationError struct {
	TlfHandle *TlfHandle
	KeyGen    KeyGen
}

// Error implements the error interface for InvalidKeyGenerationError.
func (e InvalidKeyGenerationError) Error() string {
	return fmt.Sprintf("Invalid key generation %d for %v", int(e.KeyGen), e.TlfHandle)
}

// NewKeyGenerationError indicates that the data at the given path has
// been written using keys that our client doesn't have.
type NewKeyGenerationError struct {
	TlfHandle *TlfHandle
	KeyGen    KeyGen
}

// Error implements the error interface for NewKeyGenerationError.
func (e NewKeyGenerationError) Error() string {
	return fmt.Sprintf(
		"The data for %v is keyed with a key generation (%d) that "+
			"we don't know", e.TlfHandle, e.KeyGen)
}

// BadSplitError indicates that the BlockSplitter has an error.
type BadSplitError struct {
}

// Error implements the error interface for BadSplitError
func (e BadSplitError) Error() string {
	return "Unexpected bad block split"
}

// TooLowByteCountError indicates that size of a block is smaller than
// the expected size.
type TooLowByteCountError struct {
	ExpectedMinByteCount int
	ByteCount            int
}

// Error implements the error interface for TooLowByteCountError
func (e TooLowByteCountError) Error() string {
	return fmt.Sprintf("Expected at least %d bytes, got %d bytes",
		e.ExpectedMinByteCount, e.ByteCount)
}

// InconsistentEncodedSizeError is raised when a dirty block has a
// non-zero encoded size.
type InconsistentEncodedSizeError struct {
	info BlockInfo
}

// Error implements the error interface for InconsistentEncodedSizeError
func (e InconsistentEncodedSizeError) Error() string {
	return fmt.Sprintf("Block pointer to dirty block %v with non-zero "+
		"encoded size = %d bytes", e.info.ID, e.info.EncodedSize)
}

// WriteNeededInReadRequest indicates that the system needs write
// permissions to successfully complete a read operation, so it should
// retry in write mode.
type WriteNeededInReadRequest struct {
}

// Error implements the error interface for WriteNeededInReadRequest
func (e WriteNeededInReadRequest) Error() string {
	return "This request needs exclusive access, but doesn't have it."
}

// UnknownSigVer indicates that we can't process a signature because
// it has an unknown version.
type UnknownSigVer struct {
	sigVer SigVer
}

// Error implements the error interface for UnknownSigVer
func (e UnknownSigVer) Error() string {
	return fmt.Sprintf("Unknown signature version %d", int(e.sigVer))
}

// TLFEphemeralPublicKeyNotFoundError indicates that an ephemeral
// public key matching the user and device KID couldn't be found.
type TLFEphemeralPublicKeyNotFoundError struct {
	uid keybase1.UID
	kid keybase1.KID
}

// Error implements the error interface for TLFEphemeralPublicKeyNotFoundError.
func (e TLFEphemeralPublicKeyNotFoundError) Error() string {
	return fmt.Sprintf("Could not find ephemeral public key for "+
		"user %s, device KID %v", e.uid, e.kid)
}

// KeyNotFoundError indicates that a key matching the given KID
// couldn't be found.
type KeyNotFoundError struct {
	kid keybase1.KID
}

// Error implements the error interface for KeyNotFoundError.
func (e KeyNotFoundError) Error() string {
	return fmt.Sprintf("Could not find key with kid=%s", e.kid)
}

// KeyCacheMissError indicates that a key matching the given TlfID
// and key generation wasn't found in cache.
type KeyCacheMissError struct {
	tlf    TlfID
	keyGen KeyGen
}

// Error implements the error interface for KeyCacheMissError.
func (e KeyCacheMissError) Error() string {
	return fmt.Sprintf("Could not find key with tlf=%s, keyGen=%d", e.tlf, e.keyGen)
}

// KeyCacheHitError indicates that a key matching the given TlfID
// and key generation was found in cache but the object type was unknown.
type KeyCacheHitError struct {
	tlf    TlfID
	keyGen KeyGen
}

// Error implements the error interface for KeyCacheHitError.
func (e KeyCacheHitError) Error() string {
	return fmt.Sprintf("Invalid key with tlf=%s, keyGen=%d", e.tlf, e.keyGen)
}

// UnexpectedShortCryptoRandRead indicates that fewer bytes were read
// from crypto.rand.Read() than expected.
type UnexpectedShortCryptoRandRead struct {
}

// Error implements the error interface for UnexpectedShortRandRead.
func (e UnexpectedShortCryptoRandRead) Error() string {
	return "Unexpected short read from crypto.rand.Read()"
}

// UnknownEncryptionVer indicates that we can't decrypt an
// encryptedData object because it has an unknown version.
type UnknownEncryptionVer struct {
	ver EncryptionVer
}

// Error implements the error interface for UnknownEncryptionVer.
func (e UnknownEncryptionVer) Error() string {
	return fmt.Sprintf("Unknown encryption version %d", int(e.ver))
}

// InvalidNonceError indicates that an invalid cryptographic nonce was
// detected.
type InvalidNonceError struct {
	nonce []byte
}

// Error implements the error interface for InvalidNonceError.
func (e InvalidNonceError) Error() string {
	return fmt.Sprintf("Invalid nonce %v", e.nonce)
}

// InvalidPublicTLFOperation indicates that an invalid operation was
// attempted on a public TLF.
type InvalidPublicTLFOperation struct {
	id     TlfID
	opName string
}

// Error implements the error interface for InvalidPublicTLFOperation.
func (e InvalidPublicTLFOperation) Error() string {
	return fmt.Sprintf("Tried to do invalid operation %s on public TLF %v",
		e.opName, e.id)
}

// WrongOpsError indicates that an unexpected path got passed into a
// FolderBranchOps instance
type WrongOpsError struct {
	nodeFB FolderBranch
	opsFB  FolderBranch
}

// Error implements the error interface for WrongOpsError.
func (e WrongOpsError) Error() string {
	return fmt.Sprintf("Ops for folder %v, branch %s, was given path %s, "+
		"branch %s", e.opsFB.Tlf, e.opsFB.Branch, e.nodeFB.Tlf, e.nodeFB.Branch)
}

// NodeNotFoundError indicates that we tried to find a node for the
// given BlockPointer and failed.
type NodeNotFoundError struct {
	ptr BlockPointer
}

// Error implements the error interface for NodeNotFoundError.
func (e NodeNotFoundError) Error() string {
	return fmt.Sprintf("No node found for pointer %v", e.ptr)
}

// ParentNodeNotFoundError indicates that we tried to update a Node's
// parent with a BlockPointer that we don't yet know about.
type ParentNodeNotFoundError struct {
	parent BlockPointer
}

// Error implements the error interface for ParentNodeNotFoundError.
func (e ParentNodeNotFoundError) Error() string {
	return fmt.Sprintf("No such parent node found for pointer %v", e.parent)
}

// EmptyNameError indicates that the user tried to use an empty name
// for the given BlockPointer.
type EmptyNameError struct {
	ptr BlockPointer
}

// Error implements the error interface for EmptyNameError.
func (e EmptyNameError) Error() string {
	return fmt.Sprintf("Cannot use empty name for pointer %v", e.ptr)
}

// PaddedBlockReadError occurs if the number of bytes read do not
// equal the number of bytes specified.
type PaddedBlockReadError struct {
	ActualLen   int
	ExpectedLen int
}

// Error implements the error interface of PaddedBlockReadError.
func (e PaddedBlockReadError) Error() string {
	return fmt.Sprintf("Reading block data out of padded block resulted in %d bytes, expected %d",
		e.ActualLen, e.ExpectedLen)
}

// NotDirectFileBlockError indicates that a direct file block was
// expected, but something else (e.g., an indirect file block) was
// given instead.
type NotDirectFileBlockError struct {
}

func (e NotDirectFileBlockError) Error() string {
	return fmt.Sprintf("Unexpected block type; expected a direct file block")
}

// IncrementMissingBlockError indicates that we tried to increment the
// reference count of a block that the server doesn't have.
type IncrementMissingBlockError struct {
	ID BlockID
}

func (e IncrementMissingBlockError) Error() string {
	return fmt.Sprintf("Tried to increment ref count for block %v, but no "+
		"such block exists on the server", e.ID)
}

// MDInvalidGetArguments indicates either the handle or top-level folder ID
// specified in a get request was invalid.
type MDInvalidGetArguments struct {
	id     TlfID
	handle *TlfHandle
}

// Error implements the error interface for MDInvalidGetArguments.
func (e MDInvalidGetArguments) Error() string {
	return fmt.Sprintf("Invalid arguments for MD get, id: %v, handle: %v",
		e.id, e.handle)
}

// MDInvalidTlfID indicates whether the folder ID returned from the
// MD server was not parsable/invalid.
type MDInvalidTlfID struct {
	id string
}

// Error implements the error interface for MDInvalidTlfID.
func (e MDInvalidTlfID) Error() string {
	return fmt.Sprintf("Invalid TLF ID returned from server: %s", e.id)
}

// KeyHalfMismatchError is returned when the key server doesn't return the expected key half.
type KeyHalfMismatchError struct {
	Expected TLFCryptKeyServerHalfID
	Actual   TLFCryptKeyServerHalfID
}

// Error implements the error interface for KeyHalfMismatchError.
func (e KeyHalfMismatchError) Error() string {
	return fmt.Sprintf("Key mismatch, expected ID: %s, actual ID: %s",
		e.Expected, e.Actual)
}

// InvalidHashError is returned whenever an invalid hash is
// detected.
type InvalidHashError struct {
	H Hash
}

func (e InvalidHashError) Error() string {
	return fmt.Sprintf("Invalid hash %s", e.H)
}

// UnknownHashTypeError is returned whenever a hash with an unknown
// hash type is attempted to be used for verification.
type UnknownHashTypeError struct {
	T HashType
}

func (e UnknownHashTypeError) Error() string {
	return fmt.Sprintf("Unknown hash type %s", e.T)
}

// HashMismatchError is returned whenever a hash mismatch is detected.
type HashMismatchError struct {
	ExpectedH Hash
	ActualH   Hash
}

func (e HashMismatchError) Error() string {
	return fmt.Sprintf("Hash mismatch: expected %s, got %s",
		e.ExpectedH, e.ActualH)
}

// MDServerDisconnected indicates the MDServer has been disconnected for clients waiting
// on an update channel.
type MDServerDisconnected struct {
}

// Error implements the error interface for MDServerDisconnected.
func (e MDServerDisconnected) Error() string {
	return "MDServer is disconnected"
}

// MDUpdateApplyError indicates that we tried to apply a revision that
// was not the next in line.
type MDUpdateApplyError struct {
	rev  MetadataRevision
	curr MetadataRevision
}

// Error implements the error interface for MDUpdateApplyError.
func (e MDUpdateApplyError) Error() string {
	return fmt.Sprintf("MD revision %d isn't next in line for our "+
		"current revision %d", e.rev, e.curr)
}

// MDUpdateInvertError indicates that we tried to apply a revision that
// was not the next in line.
type MDUpdateInvertError struct {
	rev  MetadataRevision
	curr MetadataRevision
}

// Error implements the error interface for MDUpdateInvertError.
func (e MDUpdateInvertError) Error() string {
	return fmt.Sprintf("MD revision %d isn't next in line for our "+
		"current revision %d while inverting", e.rev, e.curr)
}
