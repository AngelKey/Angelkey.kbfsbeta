package libkbfs

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/keybase/client/go/libkb"
	keybase1 "github.com/keybase/client/go/protocol"
)

const (
	// ReaderSep is the string that separates readers from writers in a
	// TLF name.
	ReaderSep = "#"

	// PublicUIDName is the name given to keybase1.PublicUID.
	PublicUIDName = "public"
)

// disallowedPrefixes must not be allowed at the beginning of any
// user-created directory entry name.
var disallowedPrefixes = [...]string{".kbfs"}

// UserInfo contains all the info about a keybase user that kbfs cares
// about.
type UserInfo struct {
	Name            libkb.NormalizedUsername
	UID             keybase1.UID
	VerifyingKeys   []VerifyingKey
	CryptPublicKeys []CryptPublicKey
}

// SessionInfo contains all the info about the keybase session that
// kbfs cares about.
type SessionInfo struct {
	UID            keybase1.UID
	Token          string
	CryptPublicKey CryptPublicKey
	VerifyingKey   VerifyingKey
}

// SigVer denotes a signature version.
type SigVer int

const (
	// SigED25519 is the signature type for ED25519
	SigED25519 SigVer = 1
)

// IsNil returns true if this SigVer is nil.
func (v SigVer) IsNil() bool {
	return int(v) == 0
}

// SignatureInfo contains all the info needed to verify a signature
// for a message.
type SignatureInfo struct {
	// Exported only for serialization purposes.
	Version      SigVer       `codec:"v"`
	Signature    []byte       `codec:"s"`
	VerifyingKey VerifyingKey `codec:"k"`
}

// IsNil returns true if this SignatureInfo is nil.
func (s SignatureInfo) IsNil() bool {
	return s.Version.IsNil() && len(s.Signature) == 0 && s.VerifyingKey.IsNil()
}

// DeepCopy makes a comlete copy of this SignatureInfo.
func (s SignatureInfo) DeepCopy() SignatureInfo {
	signature := make([]byte, len(s.Signature))
	copy(signature[:], s.Signature[:])
	return SignatureInfo{s.Version, signature, s.VerifyingKey}
}

// String implements the fmt.Stringer interface for SignatureInfo.
func (s SignatureInfo) String() string {
	return fmt.Sprintf("SignatureInfo{Version: %d, Signature: %s, "+
		"VerifyingKey: %s}", s.Version, hex.EncodeToString(s.Signature[:]),
		&s.VerifyingKey)
}

// TLFEphemeralPublicKeys stores a list of TLFEphemeralPublicKey
type TLFEphemeralPublicKeys []TLFEphemeralPublicKey

// DeepCopy makes a complete copy of a TLFEphemeralPublicKeys
func (tepk TLFEphemeralPublicKeys) DeepCopy() TLFEphemeralPublicKeys {
	keys := make(TLFEphemeralPublicKeys, len(tepk))
	copy(keys, tepk)
	return keys
}

// EncryptionVer denotes a version for the encryption method.
type EncryptionVer int

const (
	// EncryptionSecretbox is the encryption version that uses
	// nacl/secretbox or nacl/box.
	EncryptionSecretbox EncryptionVer = 1
)

// encryptedData is encrypted data with a nonce and a version.
type encryptedData struct {
	// Exported only for serialization purposes. Should only be
	// used by implementations of Crypto.
	Version       EncryptionVer `codec:"v"`
	EncryptedData []byte        `codec:"e"`
	Nonce         []byte        `codec:"n"`
}

// EncryptedTLFCryptKeyClientHalf is an encrypted
// TLFCryptKeyCLientHalf object.
type EncryptedTLFCryptKeyClientHalf encryptedData

// EncryptedPrivateMetadata is an encrypted PrivateMetadata object.
type EncryptedPrivateMetadata encryptedData

// EncryptedBlock is an encrypted Block.
type EncryptedBlock encryptedData

// DeepCopy returns a complete copy of this EncryptedTLFCryptKeyClientHalf.
func (ech EncryptedTLFCryptKeyClientHalf) DeepCopy() (echCopy EncryptedTLFCryptKeyClientHalf) {
	echCopy.Version = ech.Version
	echCopy.EncryptedData = make([]byte, len(ech.EncryptedData))
	copy(echCopy.EncryptedData, ech.EncryptedData)
	echCopy.Nonce = make([]byte, len(ech.Nonce))
	copy(echCopy.Nonce, ech.Nonce)
	return
}

// EncryptedTLFCryptKeyClientAndEphemeral has what's needed to
// request a client half decryption.
type EncryptedTLFCryptKeyClientAndEphemeral struct {
	// PublicKey contains the wrapped Key ID of the public key
	PubKey CryptPublicKey
	// ClientHalf contains the encrypted client half of the TLF key
	ClientHalf EncryptedTLFCryptKeyClientHalf
	// EPubKey contains the ephemeral public key used to encrypt ClientHalf
	EPubKey TLFEphemeralPublicKey
}

// KeyGen is the type of a key generation for a top-level folder.
type KeyGen int

const (
	// PublicKeyGen is the value used for public TLFs. Note that
	// it is not considered a valid key generation.
	PublicKeyGen KeyGen = -1
	// FirstValidKeyGen is the first value that is considered a
	// valid key generation. Note that the nil value is not
	// considered valid.
	FirstValidKeyGen = 1
)

// MetadataVer is the type of a version for marshalled KBFS metadata
// structures.
type MetadataVer int

const (
	// FirstValidMetadataVer is the first value that is considered a
	// valid data version. For historical reasons 0 is considered
	// valid.
	FirstValidMetadataVer = 0
)

// DataVer is the type of a version for marshalled KBFS data
// structures.
type DataVer int

const (
	// FirstValidDataVer is the first value that is considered a
	// valid data version. Note that the nil value is not
	// considered valid.
	FirstValidDataVer = 1
)

// BlockRefNonce is a 64-bit unique sequence of bytes for identifying
// this reference of a block ID from other references to the same
// (duplicated) block.
type BlockRefNonce [8]byte

// zeroBlockRefNonce is a special BlockRefNonce used for the initial
// reference to a block.
var zeroBlockRefNonce = BlockRefNonce([8]byte{0, 0, 0, 0, 0, 0, 0, 0})

func (nonce BlockRefNonce) String() string {
	return hex.EncodeToString(nonce[:])
}

// BlockPointer contains the identifying information for a block in KBFS.
type BlockPointer struct {
	ID      BlockID `codec:"i"`
	KeyGen  KeyGen  `codec:"k"` // if valid, which generation of the TLFKeyBundle to use.
	DataVer DataVer `codec:"d"` // if valid, which version of the KBFS data structures is pointed to
	// Creator is the UID that was first charged for the initial
	// reference to this block.
	Creator keybase1.UID `codec:"c"`
	// Writer is the UID that should be charged for this reference to
	// the block.  If empty, it defaults to Creator.
	Writer keybase1.UID `codec:"w,omitempty"`
	// When RefNonce is all 0s, this is the initial reference to a
	// particular block.  Using a constant refnonce for the initial
	// reference allows the server to identify and optimize for the
	// common case where there is only one reference for a block.  Two
	// initial references cannot happen simultaneously, because the
	// encrypted block contents (and thus the block ID) will be
	// randomized by the server-side block crypt key half.  All
	// subsequent references to the same block must have a random
	// RefNonce (it can't be a monotonically increasing number because
	// that would require coordination among clients).
	RefNonce BlockRefNonce `codec:"r,omitempty"`
}

// IsValid returns whether the block pointer is valid. A zero block
// pointer is considered invalid.
func (p BlockPointer) IsValid() bool {
	if !p.ID.IsValid() {
		return false
	}

	// TODO: Should also check KeyGen, DataVer, and Creator. (A
	// bunch of tests use invalid values for one of these.)

	return true
}

func (p BlockPointer) String() string {
	s := fmt.Sprintf("BlockPointer{ID: %s, KeyGen: %d, DataVer: %d, Creator: %s", p.ID, p.KeyGen, p.DataVer, p.Creator)
	if len(p.Writer) > 0 {
		s += fmt.Sprintf(", Writer: %s", p.Writer)
	}
	if p.RefNonce != zeroBlockRefNonce {
		s += fmt.Sprintf(", RefNonce: %s", p.RefNonce)
	}
	s += "}"
	return s
}

// BlockInfo contains all information about a block in KBFS and its
// contents.
//
// TODO: Move everything but ID and RefNonce from BlockPointer into
// this type.
type BlockInfo struct {
	BlockPointer
	// When non-zero, the size of the encoded (and possibly
	// encrypted) data contained in the block. When non-zero,
	// always at least the size of the plaintext data contained in
	// the block.
	EncodedSize uint32 `codec:"e"`
}

// GetCreator implements the BlockContext interface for BlockPointer.
func (p BlockPointer) GetCreator() keybase1.UID {
	return p.Creator
}

// GetWriter implements the BlockContext interface for BlockPointer.
func (p BlockPointer) GetWriter() keybase1.UID {
	if !p.Writer.IsNil() {
		return p.Writer
	}
	return p.Creator
}

// SetWriter sets the Writer field, if necessary.
func (p *BlockPointer) SetWriter(newWriter keybase1.UID) {
	if p.Creator != newWriter {
		p.Writer = newWriter
	} else {
		// save some bytes by not populating the separate Writer
		// field if it matches the creator.
		p.Writer = ""
	}
}

// GetRefNonce implements the BlockContext interface for BlockPointer.
func (p BlockPointer) GetRefNonce() BlockRefNonce {
	return p.RefNonce
}

// IsInitialized returns whether or not this BlockPointer has non-nil data.
func (p BlockPointer) IsInitialized() bool {
	return p.ID != BlockID{}
}

// IsFirstRef returns whether or not p represents the first reference
// to the corresponding BlockID.
func (p BlockPointer) IsFirstRef() bool {
	return p.RefNonce == zeroBlockRefNonce
}

// ReadyBlockData is a block that has been encoded (and encrypted).
type ReadyBlockData struct {
	// These fields should not be used outside of BlockOps.Put().
	buf        []byte
	serverHalf BlockCryptKeyServerHalf
}

// GetEncodedSize returns the size of the encoded (and encrypted)
// block data.
func (r ReadyBlockData) GetEncodedSize() int {
	return len(r.buf)
}

// Favorite is a top-level favorited folder name.
type Favorite struct {
	Name   string
	Public bool
}

// NewFavoriteFromFolder creates a Favorite from a
// keybase1.Folder.
func NewFavoriteFromFolder(folder keybase1.Folder) *Favorite {
	const publicSuffix = ReaderSep + PublicUIDName
	name := strings.TrimSuffix(folder.Name, publicSuffix)
	return &Favorite{
		Name:   name,
		Public: len(name) != len(folder.Name),
	}
}

// PathNode is a single node along an KBFS path, pointing to the top
// block for that node of the path.
type pathNode struct {
	BlockPointer
	Name string
}

func (n pathNode) isValid() bool {
	return n.BlockPointer.IsValid()
}

// DebugString returns a string representation of the node with all
// pointer information.
func (n pathNode) DebugString() string {
	return fmt.Sprintf("%s(ptr=%s)", n.Name, n.BlockPointer)
}

// BranchName is the name given to a KBFS branch, for a particular
// top-level folder.  Currently, the notion of a "branch" is
// client-side only, and can be used to specify which root to use for
// a top-level folder.  (For example, viewing a historical archive
// could use a different branch name.)
type BranchName string

const (
	// MasterBranch represents the mainline branch for a top-level
	// folder.  Set to the empty string so that the default will be
	// the master branch.
	MasterBranch BranchName = ""
)

// FolderBranch represents a unique pair of top-level folder and a
// branch of that folder.
type FolderBranch struct {
	Tlf    TlfID
	Branch BranchName // master branch, by default
}

func (fb FolderBranch) String() string {
	s := fb.Tlf.String()
	if len(fb.Branch) > 0 {
		s += fmt.Sprintf("(branch=%s)", fb.Branch)
	}
	return s
}

// path represents the full KBFS path to a particular location, so
// that a flush can traverse backwards and fix up ids along the way.
type path struct {
	FolderBranch
	path []pathNode
}

// isValid() returns true if the path has at least one node (for the
// root).
func (p path) isValid() bool {
	if len(p.path) < 1 {
		return false
	}

	for _, n := range p.path {
		if !n.isValid() {
			return false
		}
	}

	return true
}

// hasValidParent() returns true if this path is valid and
// parentPath() is a valid path.
func (p path) hasValidParent() bool {
	return len(p.path) >= 2 && p.parentPath().isValid()
}

// tailName returns the name of the final node in the Path. Must be
// called with a valid path.
func (p path) tailName() string {
	return p.path[len(p.path)-1].Name
}

// tailPointer returns the BlockPointer of the final node in the Path.
// Must be called with a valid path.
func (p path) tailPointer() BlockPointer {
	return p.path[len(p.path)-1].BlockPointer
}

// DebugString returns a string representation of the path with all
// branch and pointer information.
func (p path) DebugString() string {
	debugNames := make([]string, 0, len(p.path))
	for _, node := range p.path {
		debugNames = append(debugNames, node.DebugString())
	}
	return fmt.Sprintf("%s:%s", p.FolderBranch, strings.Join(debugNames, "/"))
}

// String implements the fmt.Stringer interface for Path.
func (p path) String() string {
	names := make([]string, 0, len(p.path))
	for _, node := range p.path {
		names = append(names, node.Name)
	}
	return strings.Join(names, "/")
}

// parentPath returns a new Path representing the parent subdirectory
// of this Path. Must be called with a valid path. Should not be
// called with a path of only a single node, as that would produce an
// invalid path.
func (p path) parentPath() *path {
	return &path{p.FolderBranch, p.path[:len(p.path)-1]}
}

// ChildPath returns a new Path with the addition of a new entry
// with the given name and BlockPointer.
func (p path) ChildPath(name string, ptr BlockPointer) path {
	child := path{
		FolderBranch: p.FolderBranch,
		path:         make([]pathNode, len(p.path), len(p.path)+1),
	}
	copy(child.path, p.path)
	child.path = append(child.path, pathNode{Name: name, BlockPointer: ptr})
	return child
}

// ChildPathNoPtr returns a new Path with the addition of a new entry
// with the given name.  That final PathNode will have no BlockPointer.
func (p path) ChildPathNoPtr(name string) path {
	return p.ChildPath(name, BlockPointer{})
}

// hasPublic returns whether or not this is a top-level folder that
// should have a "public" subdirectory.
func (p path) hasPublic() bool {
	// This directory has a corresponding public subdirectory if the
	// path has only one node and the top-level directory is not
	// already public TODO: Ideally, we'd also check if there are no
	// explicit readers, but for now we expect the caller to check
	// that.
	return len(p.path) == 1 && !p.Tlf.IsPublic()
}

// BlockChanges tracks the set of blocks that changed in a commit, and
// the operations that made the changes.  It might consist of just a
// BlockPointer if the list is too big to embed in the MD structure
// directly.
//
// If this commit represents a conflict-resolution merge, which may
// comprise multiple individual operations, then there will be an
// ordered list of the changes for individual operations.  This lets
// the notification and conflict resolution strategies figure out the
// difference between a renamed file and a modified file, for example.
type BlockChanges struct {
	// If this is set, the actual changes are stored in a block (where
	// the block contains a serialized version of BlockChanges)
	Info BlockInfo `codec:"p,omitempty"`
	// An ordered list of operations completed in this update
	Ops opsList `codec:"o,omitempty"`
	// Estimate the number of bytes that this set of changes will take to encode
	sizeEstimate uint64
}

// Equals returns true if the given BlockChanges is equal to this
// BlockChanges.  Currently does not check for equality at the
// operation level.
func (bc BlockChanges) Equals(other BlockChanges) bool {
	if bc.Info != other.Info || len(bc.Ops) != len(other.Ops) ||
		bc.sizeEstimate != other.sizeEstimate {
		return false
	}
	// TODO: check for op equality?
	return true
}

func (bc *BlockChanges) addBPSize() {
	// We want an estimate of the codec-encoded size, but the
	// in-memory size is good enough.
	bc.sizeEstimate += uint64(reflect.TypeOf(BlockPointer{}).Size())
}

// AddRefBlock adds the newly-referenced block to this BlockChanges
// and updates the size estimate.
func (bc *BlockChanges) AddRefBlock(ptr BlockPointer) {
	bc.Ops[len(bc.Ops)-1].AddRefBlock(ptr)
	bc.addBPSize()
}

// AddUnrefBlock adds the newly unreferenced block to this BlockChanges
// and updates the size estimate.
func (bc *BlockChanges) AddUnrefBlock(ptr BlockPointer) {
	bc.Ops[len(bc.Ops)-1].AddUnrefBlock(ptr)
	bc.addBPSize()
}

// AddUpdate adds the newly updated block to this BlockChanges
// and updates the size estimate.
func (bc *BlockChanges) AddUpdate(oldPtr BlockPointer, newPtr BlockPointer) {
	bc.Ops[len(bc.Ops)-1].AddUpdate(oldPtr, newPtr)
	// add sizes for both block pointers
	bc.addBPSize()
	bc.addBPSize()
}

// AddOp starts a new operation for this BlockChanges.  Subsequent
// Add* calls will populate this operation.
func (bc *BlockChanges) AddOp(o op) {
	bc.Ops = append(bc.Ops, o)
	bc.sizeEstimate += o.SizeExceptUpdates()
}

// EntryType is the type of a directory entry.
type EntryType int

const (
	// File is a regular file.
	File EntryType = iota
	// Exec is an executable file.
	Exec
	// Dir is a directory.
	Dir
	// Sym is a symbolic link.
	Sym
)

// String implements the fmt.Stringer interface for EntryType
func (et EntryType) String() string {
	switch et {
	case File:
		return "FILE"
	case Exec:
		return "EXEC"
	case Dir:
		return "DIR"
	case Sym:
		return "SYM"
	}
	return "<invalid EntryType>"
}

// EntryInfo is the (non-block-related) info a directory knows about
// its child.
type EntryInfo struct {
	Type    EntryType
	Size    uint64
	SymPath string `codec:",omitempty"` // must be within the same root dir
	// Mtime is in unix nanoseconds
	Mtime int64
	// Ctime is in unix nanoseconds
	Ctime int64
}

// DirEntry is all the data info a directory know about its child.
type DirEntry struct {
	BlockInfo
	EntryInfo
}

// IsInitialized returns true if this DirEntry has been initialized.
func (de *DirEntry) IsInitialized() bool {
	return de.BlockPointer.IsInitialized()
}

// IndirectDirPtr pairs an indirect dir block with the start of that
// block's range of directory entries (inclusive)
type IndirectDirPtr struct {
	// TODO: Make sure that the block is not dirty when the EncodedSize
	// field is non-zero.
	BlockInfo
	Off string `codec:"o"`
}

// IndirectFilePtr pairs an indirect file block with the start of that
// block's range of bytes (inclusive)
type IndirectFilePtr struct {
	// When the EncodedSize field is non-zero, the block must not
	// be dirty.
	BlockInfo
	Off int64 `codec:"o"`
}

// CommonBlock holds block data that is common for both subdirectories
// and files.
type CommonBlock struct {
	// IsInd indicates where this block is so big it requires indirect pointers
	IsInd bool `codec:"s"`
	// cachedEncodedSize is the locally-cached (non-serialized)
	// encoded size for this block.
	cachedEncodedSize uint32
}

// GetEncodedSize implements the Block interface for CommonBlock
func (cb CommonBlock) GetEncodedSize() uint32 {
	return cb.cachedEncodedSize
}

// SetEncodedSize implements the Block interface for CommonBlock
func (cb *CommonBlock) SetEncodedSize(size uint32) {
	cb.cachedEncodedSize = size
}

// NewCommonBlock returns a generic block, unsuitable for caching.
func NewCommonBlock() Block {
	return &CommonBlock{}
}

// DirBlock is the contents of a directory
type DirBlock struct {
	CommonBlock
	// if not indirect, a map of path name to directory entry
	Children map[string]DirEntry `codec:"c,omitempty"`
	// if indirect, contains the indirect pointers to the next level of blocks
	IPtrs []IndirectDirPtr `codec:"i,omitempty"`
}

// NewDirBlock creates a new, empty DirBlock.
func NewDirBlock() Block {
	return &DirBlock{
		Children: make(map[string]DirEntry),
	}
}

// DeepCopy makes a complete copy of a DirBlock
func (db DirBlock) DeepCopy() *DirBlock {
	// copy the block if it's for writing
	dblockCopy := NewDirBlock().(*DirBlock)
	*dblockCopy = db
	// deep copy of children
	dblockCopy.Children = make(map[string]DirEntry)
	for k, v := range db.Children {
		dblockCopy.Children[k] = v
	}
	// TODO: deep copy of IPtrs once we have indirect dir blocks
	// TODO: copy padding once we support it.
	return dblockCopy
}

// FileBlock is the contents of a file
type FileBlock struct {
	CommonBlock
	// if not indirect, the full contents of this block
	Contents []byte `codec:"c,omitempty"`
	// if indirect, contains the indirect pointers to the next level of blocks
	IPtrs []IndirectFilePtr `codec:"i,omitempty"`
}

// NewFileBlock creates a new, empty FileBlock.
func NewFileBlock() Block {
	return &FileBlock{
		Contents: make([]byte, 0, 0),
	}
}

// DeepCopy makes a complete copy of a FileBlock
func (fb FileBlock) DeepCopy() *FileBlock {
	fblockCopy := NewFileBlock().(*FileBlock)
	*fblockCopy = fb
	// deep copy of contents and iptrs
	fblockCopy.Contents = make([]byte, len(fb.Contents))
	copy(fblockCopy.Contents, fb.Contents)
	fblockCopy.IPtrs = make([]IndirectFilePtr, len(fb.IPtrs))
	copy(fblockCopy.IPtrs, fb.IPtrs)
	// TODO: copy padding once we support it.
	return fblockCopy
}

// extCode is used to register codec extensions
type extCode uint64

// these track the start of a range of unique extCodes for various
// types of extensions.
const (
	extCodeOpsRangeStart  = 1
	extCodeListRangeStart = 101
)

// ReportedError represents an error reported by KBFS.
type ReportedError struct {
	Level ReportingLevel
	Time  time.Time
	Error fmt.Stringer
	Stack []uintptr
}

// MergeStatus represents the merge status of a TLF.
type MergeStatus int

const (
	// Merged means that the TLF is merged and no conflict
	// resolution needs to be done.
	Merged MergeStatus = iota
	// Unmerged means that the TLF is unmerged and conflict
	// resolution needs to be done. Metadata blocks which
	// represent unmerged history should have a non-null
	// branch ID defined.
	Unmerged
)

func (m MergeStatus) String() string {
	switch m {
	case Merged:
		return "merged"
	case Unmerged:
		return "unmerged"
	default:
		return "unknown"
	}
}

//UsageStat is a tuple containing quota usage and amount of archived bytes
type UsageStat struct {
	Usage    int64
	Archived int64
}

//UserQuotaInfo contains a user's quota usage information
type UserQuotaInfo struct {
	Folders map[string]*UsageStat
	Total   UsageStat
	Limit   int64
}

// Accum combines changes to the existing UserQuotaInfo object using accumulation function accumF.
func (u *UserQuotaInfo) Accum(another *UserQuotaInfo, accumF func(int64, int64) int64) {
	if another == nil {
		return
	}
	u.Total.Usage = accumF(u.Total.Usage, another.Total.Usage)
	u.Total.Archived = accumF(u.Total.Archived, another.Total.Archived)
	for f, change := range another.Folders {
		if _, ok := u.Folders[f]; !ok {
			u.Folders[f] = &UsageStat{}
		}
		u.Folders[f].Usage = accumF(u.Folders[f].Usage, change.Usage)
		u.Folders[f].Archived = accumF(u.Folders[f].Archived, change.Archived)
	}
}

// ToBytes marshals this UserQuotaInfo
func (u *UserQuotaInfo) ToBytes(config Config) ([]byte, error) {
	return config.Codec().Encode(u)
}

// UserQuotaInfoDecode decodes b into a UserQuotaInfo
func UserQuotaInfoDecode(b []byte, config Config) (*UserQuotaInfo, error) {
	var info UserQuotaInfo
	err := config.Codec().Decode(b, &info)
	if err != nil {
		return nil, err
	}

	return &info, nil
}

// OpSummary describes the changes performed by a single op, and is
// suitable for encoding directly as JSON.
type OpSummary struct {
	Op      string
	Refs    []string
	Unrefs  []string
	Updates map[string]string
}

// UpdateSummary describes the operations done by a single MD revision.
type UpdateSummary struct {
	Revision  MetadataRevision
	Date      time.Time
	Writer    string
	LiveBytes uint64 // the "DiskUsage" for the TLF as of this revision
	Ops       []OpSummary
}

// TLFUpdateHistory gives all the summaries of all updates in a TLF's
// history.
type TLFUpdateHistory struct {
	ID      string
	Name    string
	Updates []UpdateSummary
}
