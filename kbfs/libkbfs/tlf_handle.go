package libkbfs

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/keybase/client/go/libkb"
	keybase1 "github.com/keybase/client/go/protocol"
	"golang.org/x/net/context"
)

// BareTlfHandle uniquely identifies top-level folders by readers and
// writers.
//
// TODO: Have separate types for writers vs. readers.
type BareTlfHandle struct {
	Writers           []keybase1.UID             `codec:"w,omitempty"`
	Readers           []keybase1.UID             `codec:"r,omitempty"`
	UnresolvedWriters []keybase1.SocialAssertion `codec:"uw,omitempty"`
	UnresolvedReaders []keybase1.SocialAssertion `codec:"ur,omitempty"`
}

// UIDList can be used to lexicographically sort UIDs.
type UIDList []keybase1.UID

func (u UIDList) Len() int {
	return len(u)
}

func (u UIDList) Less(i, j int) bool {
	return u[i].Less(u[j])
}

func (u UIDList) Swap(i, j int) {
	u[i], u[j] = u[j], u[i]
}

// SocialAssertionList can be used to lexicographically sort SocialAssertions.
type SocialAssertionList []keybase1.SocialAssertion

func (u SocialAssertionList) Len() int {
	return len(u)
}

func (u SocialAssertionList) Less(i, j int) bool {
	si := u[i].String()
	sj := u[j].String()
	return si < sj
}

func (u SocialAssertionList) Swap(i, j int) {
	u[i], u[j] = u[j], u[i]
}

// ErrNoWriters is the error returned by MakeBareTlfHandle if it is
// passed an empty list of writers.
var ErrNoWriters = errors.New("Cannot make TLF handle with no writers; need rekey?")

// ErrInvalidWriter is the error returned by MakeBareTlfHandle if it
// is passed an invalid writer.
var ErrInvalidWriter = errors.New("Cannot make TLF handle with invalid writer")

// ErrInvalidReader is the error returned by MakeBareTlfHandle if it
// is passed an invalid reader.
var ErrInvalidReader = errors.New("Cannot make TLF handle with invalid reader")

// MakeBareTlfHandle creates a BareTlfHandle from the given list of
// readers and writers.
func MakeBareTlfHandle(
	writers, readers []keybase1.UID,
	unresolvedWriters, unresolvedReaders []keybase1.SocialAssertion) (
	BareTlfHandle, error) {
	if len(writers) == 0 {
		return BareTlfHandle{}, ErrNoWriters
	}

	for _, w := range writers {
		if w == keybase1.PUBLIC_UID {
			return BareTlfHandle{}, ErrInvalidWriter
		}
	}

	if (len(readers) + len(unresolvedReaders)) > 1 {
		// If we have more than one reader, none of them
		// should be the public UID.
		for _, r := range readers {
			if r == keybase1.PUBLIC_UID {
				return BareTlfHandle{}, ErrInvalidReader
			}
		}
	}

	// TODO: Check for overlap between readers and writers, and
	// for duplicates.

	writersCopy := make([]keybase1.UID, len(writers))
	copy(writersCopy, writers)
	sort.Sort(UIDList(writersCopy))

	var readersCopy []keybase1.UID
	if len(readers) > 0 {
		readersCopy = make([]keybase1.UID, len(readers))
		copy(readersCopy, readers)
		sort.Sort(UIDList(readersCopy))
	}

	var unresolvedWritersCopy []keybase1.SocialAssertion
	if len(unresolvedWriters) > 0 {
		unresolvedWritersCopy = make([]keybase1.SocialAssertion, len(unresolvedWriters))
		copy(unresolvedWritersCopy, unresolvedWriters)
		sort.Sort(SocialAssertionList(unresolvedWritersCopy))
	}

	var unresolvedReadersCopy []keybase1.SocialAssertion
	if len(unresolvedReaders) > 0 {
		unresolvedReadersCopy = make([]keybase1.SocialAssertion, len(unresolvedReaders))
		copy(unresolvedReadersCopy, unresolvedReaders)
		sort.Sort(SocialAssertionList(unresolvedReadersCopy))
	}

	return BareTlfHandle{
		Writers:           writersCopy,
		Readers:           readersCopy,
		UnresolvedWriters: unresolvedWritersCopy,
		UnresolvedReaders: unresolvedReadersCopy,
	}, nil
}

// IsPublic returns whether or not this BareTlfHandle represents a
// public top-level folder.
func (h BareTlfHandle) IsPublic() bool {
	return len(h.Readers) == 1 && h.Readers[0].Equal(keybase1.PublicUID)
}

func (h BareTlfHandle) findUserInList(user keybase1.UID,
	users []keybase1.UID) bool {
	// TODO: this could be more efficient with a cached map/set
	for _, u := range users {
		if u == user {
			return true
		}
	}
	return false
}

// IsWriter returns whether or not the given user is a writer for the
// top-level folder represented by this BareTlfHandle.
func (h BareTlfHandle) IsWriter(user keybase1.UID) bool {
	return h.findUserInList(user, h.Writers)
}

// IsReader returns whether or not the given user is a reader for the
// top-level folder represented by this BareTlfHandle.
func (h BareTlfHandle) IsReader(user keybase1.UID) bool {
	return h.IsPublic() || h.findUserInList(user, h.Readers) || h.IsWriter(user)
}

// Users returns a list of all reader and writer UIDs for the tlf,
// separated out into resolved and unresolved.
func (h BareTlfHandle) Users() ([]keybase1.UID, []keybase1.SocialAssertion) {
	var resolvedUsers []keybase1.UID
	resolvedUsers = append(resolvedUsers, h.Writers...)
	resolvedUsers = append(resolvedUsers, h.Readers...)
	var unresolvedUsers []keybase1.SocialAssertion
	unresolvedUsers = append(unresolvedUsers, h.UnresolvedWriters...)
	unresolvedUsers = append(unresolvedUsers, h.UnresolvedReaders...)
	return resolvedUsers, unresolvedUsers
}

// CanonicalTlfName is a string containing the canonical name of a TLF.
type CanonicalTlfName string

// TlfHandle is like BareTlfHandle but it also contains a canonical
// TLF name.  It is go-routine-safe.
type TlfHandle struct {
	BareTlfHandle
	name CanonicalTlfName
}

type nameUIDPair struct {
	name libkb.NormalizedUsername
	uid  keybase1.UID
}

type resolvableUser interface {
	// resolve must do exactly one of the following:
	//
	//   - return a non-zero nameUIDPair;
	//   - return a non-zero keybase1.SocialAssertion;
	//   - return a non-nil error.
	resolve(context.Context) (nameUIDPair, keybase1.SocialAssertion, error)
}

func resolveOneUser(
	ctx context.Context, user resolvableUser,
	errCh chan<- error, userInfoResults chan<- nameUIDPair,
	socialAssertionResults chan<- keybase1.SocialAssertion) {
	userInfo, socialAssertion, err := user.resolve(ctx)
	if err != nil {
		select {
		case errCh <- err:
		default:
			// another worker reported an error before us;
			// first one wins
		}
		return
	}
	if userInfo != (nameUIDPair{}) {
		userInfoResults <- userInfo
		return
	}

	if socialAssertion != (keybase1.SocialAssertion{}) {
		socialAssertionResults <- socialAssertion
		return
	}

	errCh <- fmt.Errorf("Resolving %v resulted in empty userInfo and empty socialAssertion", user)
}

func makeTlfHandleHelper(
	ctx context.Context, public bool, writers, readers []resolvableUser) (*TlfHandle, error) {
	if public && len(readers) > 0 {
		return nil, errors.New("public folder cannot have readers")
	}

	// parallelize the resolutions for each user
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)

	wc := make(chan nameUIDPair, len(writers))
	uwc := make(chan keybase1.SocialAssertion, len(writers))
	for _, writer := range writers {
		go resolveOneUser(ctx, writer, errCh, wc, uwc)
	}

	rc := make(chan nameUIDPair, len(readers))
	urc := make(chan keybase1.SocialAssertion, len(readers))
	for _, reader := range readers {
		go resolveOneUser(ctx, reader, errCh, rc, urc)
	}

	usedWNames := make(map[keybase1.UID]libkb.NormalizedUsername, len(writers))
	usedRNames := make(map[keybase1.UID]libkb.NormalizedUsername, len(readers))
	usedUnresolvedWriters := make(map[keybase1.SocialAssertion]bool)
	usedUnresolvedReaders := make(map[keybase1.SocialAssertion]bool)
	for i := 0; i < len(writers)+len(readers); i++ {
		select {
		case err := <-errCh:
			return nil, err
		case userInfo := <-wc:
			usedWNames[userInfo.uid] = userInfo.name
		case userInfo := <-rc:
			usedRNames[userInfo.uid] = userInfo.name
		case socialAssertion := <-uwc:
			usedUnresolvedWriters[socialAssertion] = true
		case socialAssertion := <-urc:
			usedUnresolvedReaders[socialAssertion] = true
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	for uid := range usedWNames {
		delete(usedRNames, uid)
	}

	for sa := range usedUnresolvedWriters {
		delete(usedUnresolvedReaders, sa)
	}

	writerUIDs, unresolvedWriters, writerNames :=
		getSortedHandleLists(usedWNames, usedUnresolvedWriters)

	canonicalName := strings.Join(writerNames, ",")

	var readerUIDs []keybase1.UID
	var unresolvedReaders []keybase1.SocialAssertion
	if public {
		readerUIDs = []keybase1.UID{keybase1.PublicUID}
	} else {
		var readerNames []string
		readerUIDs, unresolvedReaders, readerNames =
			getSortedHandleLists(usedRNames, usedUnresolvedReaders)
		if len(readerNames) > 0 {
			canonicalName += ReaderSep + strings.Join(readerNames, ",")
		}
	}

	bareHandle, err := MakeBareTlfHandle(
		writerUIDs, readerUIDs, unresolvedWriters, unresolvedReaders)
	if err != nil {
		return nil, err
	}

	h := &TlfHandle{
		BareTlfHandle: bareHandle,
		name:          CanonicalTlfName(canonicalName),
	}

	return h, nil
}

type resolvableUID struct {
	nug normalizedUsernameGetter
	uid keybase1.UID
}

func (ruid resolvableUID) resolve(ctx context.Context) (nameUIDPair, keybase1.SocialAssertion, error) {
	name, err := ruid.nug.GetNormalizedUsername(ctx, ruid.uid)
	if err != nil {
		return nameUIDPair{}, keybase1.SocialAssertion{}, err
	}
	return nameUIDPair{
		name: name,
		uid:  ruid.uid,
	}, keybase1.SocialAssertion{}, nil
}

type resolvableSocialAssertion keybase1.SocialAssertion

func (rsa resolvableSocialAssertion) resolve(ctx context.Context) (nameUIDPair, keybase1.SocialAssertion, error) {
	return nameUIDPair{}, keybase1.SocialAssertion(rsa), nil
}

// MakeTlfHandle creates a TlfHandle from the given BareTlfHandle and
// the given normalizedUsernameGetter (which is usually a KBPKI).
func MakeTlfHandle(
	ctx context.Context, bareHandle BareTlfHandle,
	nug normalizedUsernameGetter) (*TlfHandle, error) {
	writers := make([]resolvableUser, 0, len(bareHandle.Writers)+len(bareHandle.UnresolvedWriters))
	for _, w := range bareHandle.Writers {
		writers = append(writers, resolvableUID{nug, w})
	}
	for _, uw := range bareHandle.UnresolvedWriters {
		writers = append(writers, resolvableSocialAssertion(uw))
	}

	var readers []resolvableUser
	if !bareHandle.IsPublic() {
		readers = make([]resolvableUser, 0, len(bareHandle.Readers)+len(bareHandle.UnresolvedReaders))
		for _, r := range bareHandle.Readers {
			readers = append(readers, resolvableUID{nug, r})
		}
		for _, ur := range bareHandle.UnresolvedReaders {
			readers = append(readers, resolvableSocialAssertion(ur))
		}
	}

	h, err := makeTlfHandleHelper(ctx, bareHandle.IsPublic(), writers, readers)
	if err != nil {
		return nil, err
	}

	if !reflect.DeepEqual(h.BareTlfHandle, bareHandle) {
		panic(fmt.Errorf("h.BareTlfHandle=%+v unexpectedly not equal to bareHandle=%+v", h.BareTlfHandle, bareHandle))
	}

	return h, nil
}

func (h *TlfHandle) deepCopy(codec Codec) (*TlfHandle, error) {
	var copy TlfHandle

	err := CodecUpdate(codec, &copy, h)
	if err != nil {
		return nil, err
	}

	copy.name = h.name
	return &copy, nil
}

// GetCanonicalName returns the canonical name of this TLF.
func (h *TlfHandle) GetCanonicalName() CanonicalTlfName {
	if h.name == "" {
		panic(fmt.Sprintf("TlfHandle %v with no name", h))
	}

	return h.name
}

func buildCanonicalPath(public bool, canonicalName CanonicalTlfName) string {
	var folderType string
	if public {
		folderType = "public"
	} else {
		folderType = "private"
	}
	// TODO: Handle windows paths?
	return fmt.Sprintf("/keybase/%s/%s", folderType, canonicalName)
}

// GetCanonicalPath returns the full canonical path of this TLF.
func (h *TlfHandle) GetCanonicalPath() string {
	return buildCanonicalPath(h.IsPublic(), h.GetCanonicalName())
}

// ToFavorite converts a TlfHandle into a Favorite, suitable for
// Favorites calls.
func (h *TlfHandle) ToFavorite() Favorite {
	return Favorite{
		Name:   string(h.GetCanonicalName()),
		Public: h.IsPublic(),
	}
}

func getSortedHandleLists(
	uidToName map[keybase1.UID]libkb.NormalizedUsername,
	unresolved map[keybase1.SocialAssertion]bool) (
	[]keybase1.UID, []keybase1.SocialAssertion, []string) {
	var uids []keybase1.UID
	var assertions []keybase1.SocialAssertion
	var names []string
	for uid, name := range uidToName {
		uids = append(uids, uid)
		names = append(names, name.String())
	}
	for sa := range unresolved {
		assertions = append(assertions, sa)
		names = append(names, sa.String())
	}
	sort.Sort(UIDList(uids))
	sort.Sort(SocialAssertionList(assertions))
	sort.Sort(sort.StringSlice(names))
	return uids, assertions, names
}

func splitNormalizedTLFNameIntoWritersAndReaders(name string, public bool) (
	writerNames, readerNames []string, err error) {
	splitNames := strings.SplitN(name, ReaderSep, 3)
	if len(splitNames) > 2 {
		return nil, nil, BadTLFNameError{name}
	}
	writerNames = strings.Split(splitNames[0], ",")
	if len(splitNames) > 1 {
		readerNames = strings.Split(splitNames[1], ",")
	}

	hasPublic := len(readerNames) == 0

	if public && !hasPublic {
		// No public folder exists for this folder.
		return nil, nil, NoSuchNameError{Name: name}
	}

	isValidUser := libkb.CheckUsername.F
	for _, name := range append(writerNames, readerNames...) {
		if !(isValidUser(name) || libkb.IsSocialAssertion(name)) {
			return nil, nil, BadTLFNameError{name}
		}
	}

	normalizedName := normalizeNamesInTLF(writerNames, readerNames)
	if normalizedName != name {
		return nil, nil, TlfNameNotCanonical{name, normalizedName}
	}

	return writerNames, readerNames, nil
}

func normalizeAssertionOrName(s string) string {
	// TODO: Ideally, this would do error-checking, e.g. if
	// isSocialAssertion is false because it's an invalid
	// username, or if a bare username is invalid.
	socialAssertion, isSocialAssertion := libkb.NormalizeSocialAssertion(s)
	if isSocialAssertion {
		return socialAssertion.String()
	}
	return libkb.NewNormalizedUsername(s).String()
}

// normalizeNamesInTLF takes a split TLF name and, without doing any
// resolutions or identify calls, normalizes all elements of the
// name. It then returns the normalized name.
func normalizeNamesInTLF(writerNames, readerNames []string) string {
	sortedWriterNames := make([]string, len(writerNames))
	for i, w := range writerNames {
		sortedWriterNames[i] = normalizeAssertionOrName(w)
	}
	sort.Strings(sortedWriterNames)
	normalizedName := strings.Join(sortedWriterNames, ",")
	if len(readerNames) > 0 {
		sortedReaderNames := make([]string, len(readerNames))
		for i, r := range readerNames {
			sortedReaderNames[i] = normalizeAssertionOrName(r)
		}
		sort.Strings(sortedReaderNames)
		normalizedName += ReaderSep + strings.Join(sortedReaderNames, ",")
	}
	return normalizedName
}

type resolvableAssertion struct {
	sharingBeforeSignupEnabled bool
	kbpki                      KBPKI
	assertion                  string
}

func (ra resolvableAssertion) resolve(ctx context.Context) (
	nameUIDPair, keybase1.SocialAssertion, error) {
	if ra.assertion == PublicUIDName {
		return nameUIDPair{}, keybase1.SocialAssertion{}, fmt.Errorf("Invalid name %s", ra.assertion)
	}
	name, uid, err := ra.kbpki.Resolve(ctx, ra.assertion)
	switch err := err.(type) {
	default:
		return nameUIDPair{}, keybase1.SocialAssertion{}, err
	case nil:
		return nameUIDPair{
			name: name,
			uid:  uid,
		}, keybase1.SocialAssertion{}, nil
	case NoSuchUserError:
		if !ra.sharingBeforeSignupEnabled {
			return nameUIDPair{}, keybase1.SocialAssertion{}, err
		}
		socialAssertion, ok := libkb.NormalizeSocialAssertion(ra.assertion)
		if !ok {
			return nameUIDPair{}, keybase1.SocialAssertion{}, err
		}
		return nameUIDPair{}, socialAssertion, nil
	}
}

// ParseTlfHandle parses a TlfHandle from an encoded string. See
// TlfHandle.GetCanonicalName() for the opposite direction.
//
// Some errors that may be returned and can be specially handled:
//
// TlfNameNotCanonical: Returned when the given name is not canonical
// -- another name to try (which itself may not be canonical) is in
// the error. Usually, you want to treat this as a symlink to the name
// to try.
//
// NoSuchNameError: Returned when public is set and the given folder
// has no public folder.
func ParseTlfHandle(
	ctx context.Context, kbpki KBPKI, name string, public bool,
	sharingBeforeSignupEnabled bool) (*TlfHandle, error) {
	// Before parsing the tlf handle (which results in identify
	// calls that cause tracker popups), first see if there's any
	// quick normalization of usernames we can do.  For example,
	// this avoids an identify in the case of "HEAD" which might
	// just be a shell trying to look for a git repo rather than a
	// real user lookup for "head" (KBFS-531).  Note that the name
	// might still contain assertions, which will result in
	// another alias in a subsequent lookup.
	writerNames, readerNames, err := splitNormalizedTLFNameIntoWritersAndReaders(name, public)
	if err != nil {
		return nil, err
	}

	hasPublic := len(readerNames) == 0

	if public && !hasPublic {
		// No public folder exists for this folder.
		return nil, NoSuchNameError{Name: name}
	}

	normalizedName := normalizeNamesInTLF(writerNames, readerNames)
	if normalizedName != name {
		return nil, TlfNameNotCanonical{name, normalizedName}
	}

	writers := make([]resolvableUser, len(writerNames))
	for i, w := range writerNames {
		writers[i] = resolvableAssertion{sharingBeforeSignupEnabled, kbpki, w}
	}
	readers := make([]resolvableUser, len(readerNames))
	for i, r := range readerNames {
		readers[i] = resolvableAssertion{sharingBeforeSignupEnabled, kbpki, r}
	}
	h, err := makeTlfHandleHelper(ctx, public, writers, readers)
	if err != nil {
		return nil, err
	}

	if !public {
		currentUsername, currentUID, err := kbpki.GetCurrentUserInfo(ctx)
		if err != nil {
			return nil, err
		}

		canRead := false

		for _, uid := range append(h.Writers, h.Readers...) {
			if uid == currentUID {
				canRead = true
				break
			}
		}

		if !canRead {
			return nil, ReadAccessError{currentUsername, h.GetCanonicalName(), public}
		}
	}

	if string(h.GetCanonicalName()) == name {
		// Name is already canonical (i.e., all usernames and
		// no assertions) so we can delay the identify until
		// the node is actually used.
		return h, nil
	}

	// Otherwise, identify before returning the canonical name.
	err = identifyHandle(ctx, kbpki, kbpki, h)
	if err != nil {
		return nil, err
	}

	return nil, TlfNameNotCanonical{name, string(h.GetCanonicalName())}
}

// CheckTlfHandleOffline does light checks whether a TLF handle looks ok,
// it avoids all network calls.
func CheckTlfHandleOffline(
	ctx context.Context, name string, public bool) error {
	_, _, err := splitNormalizedTLFNameIntoWritersAndReaders(name, public)
	return err
}
