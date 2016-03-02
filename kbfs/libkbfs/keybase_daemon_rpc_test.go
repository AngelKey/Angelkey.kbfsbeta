package libkbfs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
	rpc "github.com/keybase/go-framed-msgpack-rpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
)

type blockingClient struct {
	ctlChan chan struct{}
}

var _ rpc.GenericClient = blockingClient{}

func (b blockingClient) Call(ctx context.Context, s string, args interface{},
	res interface{}) error {
	// Say we're ready, and wait for cancellation.
	b.ctlChan <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func (b blockingClient) Notify(ctx context.Context, s string, args interface{}) error {
	return nil
}

func newKeybaseDaemonRPCWithFakeClient(t *testing.T) (
	*KeybaseDaemonRPC, chan struct{}) {
	ctlChan := make(chan struct{})
	c := newKeybaseDaemonRPCWithClient(
		nil,
		blockingClient{ctlChan},
		logger.NewTestLogger(t))
	return c, ctlChan
}

// If we cancel the RPC before the RPC returns, the call should error quickly.
func TestKeybaseDaemonRPCIdentifyCanceled(t *testing.T) {
	c, ctlChan := newKeybaseDaemonRPCWithFakeClient(t)
	f := func(ctx context.Context) error {
		_, err := c.Identify(ctx, "", "")
		return err
	}
	testWithCanceledContext(t, context.Background(), ctlChan, f)
}

// If we cancel the RPC before the RPC returns, the call should error quickly.
func TestKeybaseDaemonRPCGetCurrentSessionCanceled(t *testing.T) {
	c, ctlChan := newKeybaseDaemonRPCWithFakeClient(t)
	f := func(ctx context.Context) error {
		_, err := c.CurrentSession(ctx, 0)
		return err
	}
	testWithCanceledContext(t, context.Background(), ctlChan, f)
}

// TODO: Add tests for Favorite* methods, too.

type fakeKeybaseClient struct {
	session                SessionInfo
	users                  map[keybase1.UID]UserInfo
	currentSessionCalled   bool
	identifyCalled         bool
	loadUserPlusKeysCalled bool
}

var _ rpc.GenericClient = (*fakeKeybaseClient)(nil)

func (c *fakeKeybaseClient) Call(ctx context.Context, s string, args interface{},
	res interface{}) error {
	switch s {
	case "keybase.1.session.currentSession":
		*res.(*keybase1.Session) = keybase1.Session{
			Uid:             c.session.UID,
			Username:        "fake username",
			Token:           c.session.Token,
			DeviceSubkeyKid: c.session.CryptPublicKey.kid,
			DeviceSibkeyKid: c.session.VerifyingKey.kid,
		}

		c.currentSessionCalled = true
		return nil

	case "keybase.1.identify.identify2":
		arg := args.([]interface{})[0].(keybase1.Identify2Arg)
		uidStr := strings.TrimPrefix(arg.UserAssertion, "uid:")
		if len(uidStr) == len(arg.UserAssertion) {
			return fmt.Errorf("Non-uid assertion %s", arg.UserAssertion)
		}

		uid := keybase1.UID(uidStr)
		userInfo, ok := c.users[uid]
		if !ok {
			return fmt.Errorf("Could not find user info for UID %s", uid)
		}

		*res.(*keybase1.Identify2Res) = keybase1.Identify2Res{
			Upk: keybase1.UserPlusKeys{
				Uid:      uid,
				Username: string(userInfo.Name),
			},
		}

		c.identifyCalled = true
		return nil

	case "keybase.1.user.loadUserPlusKeys":
		arg := args.([]interface{})[0].(keybase1.LoadUserPlusKeysArg)

		userInfo, ok := c.users[arg.Uid]
		if !ok {
			return fmt.Errorf("Could not find user info for UID %s", arg.Uid)
		}

		*res.(*keybase1.UserPlusKeys) = keybase1.UserPlusKeys{
			Uid:      arg.Uid,
			Username: string(userInfo.Name),
		}

		c.loadUserPlusKeysCalled = true
		return nil

	default:
		return fmt.Errorf("Unknown call: %s %v %v", s, args, res)
	}
}

func (c *fakeKeybaseClient) Notify(_ context.Context, s string, args interface{}) error {
	return fmt.Errorf("Unknown notify: %s %v", s, args)
}

const expectCall = true
const expectCached = false

func testCurrentSession(
	t *testing.T, client *fakeKeybaseClient, c *KeybaseDaemonRPC,
	expectedSession SessionInfo, expectedCalled bool) {
	client.currentSessionCalled = false

	ctx := context.Background()
	sessionID := 0
	session, err := c.CurrentSession(ctx, sessionID)
	require.Nil(t, err)

	assert.Equal(t, expectedSession, session)
	assert.Equal(t, expectedCalled, client.currentSessionCalled)
}

// Test that the session cache works and is invalidated as expected.
func TestKeybaseDaemonSessionCache(t *testing.T) {
	name := libkb.NormalizedUsername("fake username")
	k := MakeLocalUserCryptPublicKeyOrBust(name)
	v := MakeLocalUserVerifyingKeyOrBust(name)
	session := SessionInfo{
		Name:           name,
		UID:            keybase1.UID("fake uid"),
		Token:          "fake token",
		CryptPublicKey: k,
		VerifyingKey:   v,
	}

	client := &fakeKeybaseClient{session: session}
	c := newKeybaseDaemonRPCWithClient(
		nil, client, logger.NewTestLogger(t))

	// Should fill cache.
	testCurrentSession(t, client, c, session, expectCall)

	// Should be cached.
	testCurrentSession(t, client, c, session, expectCached)

	// Should invalidate cache.
	err := c.LoggedOut(context.Background())
	require.Nil(t, err)

	// Should fill cache again.
	testCurrentSession(t, client, c, session, expectCall)

	// Should be cached again.
	testCurrentSession(t, client, c, session, expectCached)

	// Should invalidate cache.
	c.OnDisconnected(UsingExistingConnection)

	// Should fill cache again.
	testCurrentSession(t, client, c, session, expectCall)
}

func testLoadUserPlusKeys(
	t *testing.T, client *fakeKeybaseClient, c *KeybaseDaemonRPC,
	uid keybase1.UID, expectedName libkb.NormalizedUsername,
	expectedCalled bool) {
	client.loadUserPlusKeysCalled = false

	ctx := context.Background()
	info, err := c.LoadUserPlusKeys(ctx, uid)
	require.Nil(t, err)

	assert.Equal(t, expectedName, info.Name)
	assert.Equal(t, expectedCalled, client.loadUserPlusKeysCalled)
}

func testIdentify(
	t *testing.T, client *fakeKeybaseClient, c *KeybaseDaemonRPC,
	uid keybase1.UID, expectedName libkb.NormalizedUsername,
	expectedCalled bool) {
	client.identifyCalled = false

	ctx := context.Background()
	info, err := c.Identify(ctx, "uid:"+string(uid), "")
	require.Nil(t, err)

	assert.Equal(t, expectedName, info.Name)
	assert.Equal(t, expectedCalled, client.identifyCalled)
}

// Test that the user cache works and is invalidated as expected.
func TestKeybaseDaemonUserCache(t *testing.T) {
	uid1 := keybase1.UID("uid1")
	uid2 := keybase1.UID("uid2")
	name1 := libkb.NewNormalizedUsername("name1")
	name2 := libkb.NewNormalizedUsername("name2")
	users := map[keybase1.UID]UserInfo{
		uid1: {Name: name1},
		uid2: {Name: name2},
	}
	client := &fakeKeybaseClient{users: users}
	c := newKeybaseDaemonRPCWithClient(
		nil, client, logger.NewTestLogger(t))

	// Should fill cache.
	testLoadUserPlusKeys(t, client, c, uid1, name1, expectCall)

	// Should be cached.
	testLoadUserPlusKeys(t, client, c, uid1, name1, expectCached)

	// Should fill cache.
	testIdentify(t, client, c, uid2, name2, expectCall)

	// Should be cached.
	testLoadUserPlusKeys(t, client, c, uid2, name2, expectCached)

	// Should not be cached.
	testIdentify(t, client, c, uid2, name2, expectCall)

	// Should invalidate cache for uid1.
	err := c.UserChanged(context.Background(), uid1)
	require.Nil(t, err)

	// Should fill cache again.
	testLoadUserPlusKeys(t, client, c, uid1, name1, expectCall)

	// Should be cached again.
	testLoadUserPlusKeys(t, client, c, uid1, name1, expectCached)

	// Should still be cached.
	testLoadUserPlusKeys(t, client, c, uid2, name2, expectCached)

	// Should invalidate cache for uid2.
	err = c.UserChanged(context.Background(), uid2)
	require.Nil(t, err)

	// Should fill cache again.
	testLoadUserPlusKeys(t, client, c, uid2, name2, expectCall)

	// Should be cached again.
	testLoadUserPlusKeys(t, client, c, uid2, name2, expectCached)

	// Should invalidate cache for all users.
	c.OnDisconnected(UsingExistingConnection)

	// Should fill cache again.
	testLoadUserPlusKeys(t, client, c, uid1, name1, expectCall)
	testLoadUserPlusKeys(t, client, c, uid2, name2, expectCall)
}
