package libkbfs

import (
	"fmt"

	"golang.org/x/net/context"
)

// crChain represents the set of operations that happened to a
// particular KBFS node (e.g., individual file or directory) over a
// given set of MD updates.  It also tracks the starting and ending
// block pointers for the node.
type crChain struct {
	ops                  []op
	original, mostRecent BlockPointer
}

// collapse finds complementary pairs of operations that cancel each
// other out, and remove the relevant operations from the chain.
// Examples include:
//  * A create followed by a remove for the same name (delete both ops)
//  * A create followed by a create (renamed == true) for the same name
//    (delete the create op)
func (cc *crChain) collapse() {
	createsSeen := make(map[string]int)
	indicesToRemove := make(map[int]bool)
	for i, op := range cc.ops {
		switch realOp := op.(type) {
		case *createOp:
			if prevCreateIndex, ok :=
				createsSeen[realOp.NewName]; realOp.renamed && ok {
				// A rename has papered over the first create, so
				// just drop it.
				indicesToRemove[prevCreateIndex] = true
			}
			createsSeen[realOp.NewName] = i
		case *rmOp:
			if prevCreateIndex, ok := createsSeen[realOp.OldName]; ok {
				delete(createsSeen, realOp.OldName)
				// The rm cancels out the create, so remove both.
				indicesToRemove[prevCreateIndex] = true
				indicesToRemove[i] = true
			}
		case *setAttrOp:
			// TODO: Collapse opposite setex pairs
		default:
			// ignore other op types
		}
	}

	if len(indicesToRemove) > 0 {
		ops := make([]op, 0, len(cc.ops)-len(indicesToRemove))
		for i, op := range cc.ops {
			if !indicesToRemove[i] {
				ops = append(ops, op)
			}
		}
		cc.ops = ops
	}
}

func (cc *crChain) getActionsToMerge(renamer ConflictRenamer, mergedPath path,
	mergedChain *crChain) (crActionList, error) {
	// Check each op against all ops in the corresponding merged
	// chain, looking for conflicts.  If there is a conflict, return
	// it as part of the action list.  If there are no conflicts for
	// that op, return the op's default actions.
	var actions crActionList
	for _, unmergedOp := range cc.ops {
		conflict := false
		if mergedChain != nil {
			for _, mergedOp := range mergedChain.ops {
				action, err :=
					unmergedOp.CheckConflict(renamer, mergedOp)
				if err != nil {
					return nil, err
				}
				if action != nil {
					conflict = true
					actions = append(actions, action)
				}
			}
		}
		// no conflicts!
		if !conflict {
			actions = append(actions, unmergedOp.GetDefaultAction(mergedPath))
		}
	}

	return actions, nil
}

func (cc *crChain) isFile() bool {
	if len(cc.ops) == 0 {
		return false
	}

	// If the first op is setAttr or sync, this is a file chain.
	switch cc.ops[0].(type) {
	case *syncOp:
		return true
	case *setAttrOp:
		return true
	}
	return false
}

type renameInfo struct {
	originalOldParent BlockPointer
	oldName           string
	originalNewParent BlockPointer
	newName           string
}

// crChains contains a crChain for every KBFS node affected by the
// operations over a given set of MD updates.  The chains are indexed
// by both the starting (original) and ending (most recent) pointers.
// It also keeps track of which chain points to the root of the folder.
type crChains struct {
	byOriginal   map[BlockPointer]*crChain
	byMostRecent map[BlockPointer]*crChain
	originalRoot BlockPointer

	// The original blockpointers for nodes that have been
	// unreferenced or initially referenced during this chain.
	deletedOriginals map[BlockPointer]bool
	createdOriginals map[BlockPointer]bool

	// A map from original blockpointer to the full rename operation
	// of the node (from the original location of the node to the
	// final locations).
	renamedOriginals map[BlockPointer]renameInfo

	// Also keep a reference to the most recent MD that's part of this
	// chain.
	mostRecentMD *RootMetadata

	// We need to be able to track ANY BlockPointer, at any point in
	// the chain, back to its original.
	originals map[BlockPointer]BlockPointer
}

func (ccs *crChains) addOp(ptr BlockPointer, op op) error {
	currChain, ok := ccs.byMostRecent[ptr]
	if !ok {
		return fmt.Errorf("Could not find chain for most recent ptr %v", ptr)
	}

	currChain.ops = append(currChain.ops, op)
	return nil
}

func (ccs *crChains) makeChainForOp(op op) error {
	// First set the pointers for all updates, and track what's been
	// created and destroyed.
	for _, update := range op.AllUpdates() {
		chain, ok := ccs.byMostRecent[update.Unref]
		if !ok {
			// No matching chain means it's time to start a new chain
			chain = &crChain{original: update.Unref}
			ccs.byOriginal[update.Unref] = chain
		}
		if chain.mostRecent.IsInitialized() {
			// delete the old most recent pointer, it's no longer needed
			delete(ccs.byMostRecent, chain.mostRecent)
		}
		chain.mostRecent = update.Ref
		ccs.byMostRecent[update.Ref] = chain
		if chain.original != update.Ref {
			// Always be able to track this one back to its original.
			ccs.originals[update.Ref] = chain.original
		}
	}

	for _, ptr := range op.Refs() {
		ccs.createdOriginals[ptr] = true
	}

	for _, ptr := range op.Unrefs() {
		// Look up the original pointer corresponding to this most
		// recent one.
		original := ptr
		if ptrChain, ok := ccs.byMostRecent[ptr]; ok {
			original = ptrChain.original
		}

		ccs.deletedOriginals[original] = true
	}

	// then set the op depending on the actual op type
	switch realOp := op.(type) {
	default:
		panic(fmt.Sprintf("Unrecognized operation: %v", op))
	case *createOp:
		err := ccs.addOp(realOp.Dir.Ref, op)
		if err != nil {
			return err
		}
	case *rmOp:
		err := ccs.addOp(realOp.Dir.Ref, op)
		if err != nil {
			return err
		}
	case *renameOp:
		// split rename op into two separate operations, one for
		// remove and one for create
		ro := newRmOp(realOp.OldName, realOp.OldDir.Unref)
		ro.setWriterName(realOp.getWriterName())
		ro.Dir.Ref = realOp.OldDir.Ref
		err := ccs.addOp(realOp.OldDir.Ref, ro)
		if err != nil {
			return err
		}

		ndu := realOp.NewDir.Unref
		ndr := realOp.NewDir.Ref
		if realOp.NewDir == (blockUpdate{}) {
			// this is a rename within the same directory
			ndu = realOp.OldDir.Unref
			ndr = realOp.OldDir.Ref
		}

		co := newCreateOp(realOp.NewName, ndu, realOp.RenamedType)
		co.setWriterName(realOp.getWriterName())
		co.renamed = true
		co.Dir.Ref = ndr
		err = ccs.addOp(ndr, co)
		if err != nil {
			return err
		}

		// also keep track of the new parent for the renamed node
		if realOp.Renamed.IsInitialized() {
			newParentChain, ok := ccs.byMostRecent[ndr]
			if !ok {
				return fmt.Errorf("While renaming, couldn't find the chain "+
					"for the new parent %v", ndr)
			}
			oldParentChain, ok := ccs.byMostRecent[realOp.OldDir.Ref]
			if !ok {
				return fmt.Errorf("While renaming, couldn't find the chain "+
					"for the old parent %v", ndr)
			}

			renamedOriginal := realOp.Renamed
			if renamedChain, ok := ccs.byMostRecent[realOp.Renamed]; ok {
				renamedOriginal = renamedChain.original
			}
			// Use the previous old info if there is one already,
			// in case this node has been renamed multiple times.
			ri, ok := ccs.renamedOriginals[renamedOriginal]
			if !ok {
				// Otherwise make a new one.
				ri = renameInfo{
					originalOldParent: oldParentChain.original,
					oldName:           realOp.OldName,
				}
			}
			ri.originalNewParent = newParentChain.original
			ri.newName = realOp.NewName
			ccs.renamedOriginals[renamedOriginal] = ri
			// Remember what you create, in case we need to merge
			// directories after a rename.
			co.AddRefBlock(renamedOriginal)
		}
	case *syncOp:
		err := ccs.addOp(realOp.File.Ref, op)
		if err != nil {
			return err
		}
	case *setAttrOp:
		// Because the attributes apply to the file, which doesn't
		// actually have an updated pointer, we may need to create a
		// new chain.
		_, ok := ccs.byMostRecent[realOp.File]
		if !ok {
			// pointer didn't change, so most recent is the same:
			chain := &crChain{original: realOp.File, mostRecent: realOp.File}
			ccs.byOriginal[realOp.File] = chain
			ccs.byMostRecent[realOp.File] = chain
		}

		err := ccs.addOp(realOp.File, op)
		if err != nil {
			return err
		}
	case *gcOp:
		// ignore gc op
	}

	return nil
}

func (ccs *crChains) makeChainForNewOpWithUpdate(
	targetPtr BlockPointer, newOp op, update *blockUpdate) error {
	oldUnref := update.Unref
	update.Unref = targetPtr
	update.Ref = update.Unref // so that most recent == original
	defer func() {
		// reset the update to its original state before returning.
		update.Unref = oldUnref
		update.Ref = BlockPointer{}
	}()
	err := ccs.makeChainForOp(newOp)
	if err != nil {
		return err
	}
	return nil
}

// makeChainForNewOp makes a new chain for an op that does not yet
// have its pointers initialized.  It does so by setting Unref and Ref
// to be the same for the duration of this function, and calling the
// usual makeChainForOp method.  This function is not goroutine-safe
// with respect to newOp.  Also note that rename ops will not be split
// into two ops; they will be placed only in the new directory chain.
func (ccs *crChains) makeChainForNewOp(targetPtr BlockPointer, newOp op) error {
	switch realOp := newOp.(type) {
	case *createOp:
		return ccs.makeChainForNewOpWithUpdate(targetPtr, newOp, &realOp.Dir)
	case *rmOp:
		return ccs.makeChainForNewOpWithUpdate(targetPtr, newOp, &realOp.Dir)
	case *renameOp:
		// In this case, we don't want to split the rename chain, so
		// just make up a new operation and later overwrite it with
		// the rename op.
		co := newCreateOp(realOp.NewName, realOp.NewDir.Unref, File)
		err := ccs.makeChainForNewOpWithUpdate(targetPtr, co, &co.Dir)
		if err != nil {
			return err
		}
		chain, ok := ccs.byMostRecent[targetPtr]
		if !ok {
			return fmt.Errorf("Couldn't find chain for %v after making it",
				targetPtr)
		}
		if len(chain.ops) != 1 {
			return fmt.Errorf("Chain of unexpected length for %v after "+
				"making it", targetPtr)
		}
		chain.ops[0] = realOp
		return nil
	case *setAttrOp:
		return ccs.makeChainForNewOpWithUpdate(targetPtr, newOp, &realOp.Dir)
	case *syncOp:
		return ccs.makeChainForNewOpWithUpdate(targetPtr, newOp, &realOp.File)
	default:
		return fmt.Errorf("Couldn't make chain with unknown operation %s",
			newOp)
	}
}

func (ccs *crChains) mostRecentFromOriginal(original BlockPointer) (
	BlockPointer, error) {
	chain, ok := ccs.byOriginal[original]
	if !ok {
		return BlockPointer{}, NoChainFoundError{original}
	}
	return chain.mostRecent, nil
}

func (ccs *crChains) originalFromMostRecent(mostRecent BlockPointer) (
	BlockPointer, error) {
	chain, ok := ccs.byMostRecent[mostRecent]
	if !ok {
		return BlockPointer{}, NoChainFoundError{mostRecent}
	}
	return chain.original, nil
}

func (ccs *crChains) isCreated(original BlockPointer) bool {
	return ccs.createdOriginals[original]
}

func (ccs *crChains) isDeleted(original BlockPointer) bool {
	return ccs.deletedOriginals[original]
}

func (ccs *crChains) renamedParentAndName(original BlockPointer) (
	BlockPointer, string, bool) {
	info, ok := ccs.renamedOriginals[original]
	if !ok {
		return BlockPointer{}, "", false
	}
	return info.originalNewParent, info.newName, true
}

func newCRChainsEmpty() *crChains {
	return &crChains{
		byOriginal:       make(map[BlockPointer]*crChain),
		byMostRecent:     make(map[BlockPointer]*crChain),
		deletedOriginals: make(map[BlockPointer]bool),
		createdOriginals: make(map[BlockPointer]bool),
		renamedOriginals: make(map[BlockPointer]renameInfo),
		originals:        make(map[BlockPointer]BlockPointer),
	}
}

func newCRChains(ctx context.Context, kbpki KBPKI, rmds []*RootMetadata) (
	ccs *crChains, err error) {
	ccs = newCRChainsEmpty()

	// For each MD update, turn each update in each op into map
	// entries and create chains for the BlockPointers that are
	// affected directly by the operation.
	for _, rmd := range rmds {
		writerName, err := kbpki.GetNormalizedUsername(ctx, rmd.data.LastWriter)
		if err != nil {
			return nil, err
		}

		for _, op := range rmd.data.Changes.Ops {
			op.setWriterName(writerName)
			err := ccs.makeChainForOp(op)
			if err != nil {
				return nil, err
			}
		}

		if !ccs.originalRoot.IsInitialized() {
			// Find the original pointer for the root directory
			if rootChain, ok :=
				ccs.byMostRecent[rmd.data.Dir.BlockPointer]; ok {
				ccs.originalRoot = rootChain.original
			}
		}
	}

	for _, chain := range ccs.byOriginal {
		chain.collapse()
		// NOTE: even if we've removed all its ops, still keep the
		// chain around so we can see the mapping between the original
		// and most recent pointers.
	}

	if len(rmds) > 0 {
		ccs.mostRecentMD = rmds[len(rmds)-1]
	}

	return ccs, nil
}

type crChainSummary struct {
	Path string
	Ops  []string
}

func (ccs *crChains) summary(identifyChains *crChains,
	nodeCache NodeCache) (res []*crChainSummary) {
	for _, chain := range ccs.byOriginal {
		summary := &crChainSummary{}
		res = append(res, summary)

		// first stringify all the ops so they are displayed even if
		// we can't find the path.
		for _, op := range chain.ops {
			summary.Ops = append(summary.Ops, op.String())
		}

		// find the path name using the identified most recent pointer
		n := nodeCache.Get(chain.mostRecent)
		if n == nil {
			summary.Path = fmt.Sprintf("Unknown path: %v", chain.mostRecent)
			continue
		}

		path := nodeCache.PathFromNode(n)
		summary.Path = path.String()
	}

	return res
}

func (ccs *crChains) removeChain(ptr BlockPointer) {
	delete(ccs.byOriginal, ptr)
	delete(ccs.byMostRecent, ptr)
}

// changeOriginal converts the original of a chain to a different original.
func (ccs *crChains) changeOriginal(oldOriginal BlockPointer,
	newOriginal BlockPointer) error {
	chain, ok := ccs.byOriginal[oldOriginal]
	if !ok {
		return NoChainFoundError{oldOriginal}
	}
	if _, ok := ccs.byOriginal[newOriginal]; ok {
		return fmt.Errorf("crChains.changeOriginal: New original %v "+
			"already exists", newOriginal)
	}

	delete(ccs.byOriginal, oldOriginal)
	chain.original = newOriginal
	ccs.byOriginal[newOriginal] = chain
	ccs.originals[oldOriginal] = newOriginal

	if _, ok := ccs.deletedOriginals[oldOriginal]; ok {
		delete(ccs.deletedOriginals, oldOriginal)
		ccs.deletedOriginals[newOriginal] = true
	}
	if _, ok := ccs.createdOriginals[oldOriginal]; ok {
		delete(ccs.createdOriginals, oldOriginal)
		ccs.createdOriginals[newOriginal] = true
	}
	if ri, ok := ccs.renamedOriginals[oldOriginal]; ok {
		delete(ccs.renamedOriginals, oldOriginal)
		ccs.renamedOriginals[newOriginal] = ri
	}
	return nil
}
