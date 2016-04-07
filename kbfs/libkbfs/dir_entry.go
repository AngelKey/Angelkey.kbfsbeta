package libkbfs

import "github.com/keybase/go-codec/codec"

// DirEntry is all the data info a directory know about its child.
type DirEntry struct {
	BlockInfo
	EntryInfo

	codec.UnknownFieldSetHandler
}

// IsInitialized returns true if this DirEntry has been initialized.
func (de *DirEntry) IsInitialized() bool {
	return de.BlockPointer.IsInitialized()
}
