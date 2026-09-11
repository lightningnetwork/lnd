package rpcperms

import (
	"context"
	"encoding/hex"
	"net"
	"path"
	"strings"
	"testing"

	"github.com/btcsuite/btclog/v2"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"gopkg.in/macaroon-bakery.v2/bakery"
	macaroon "gopkg.in/macaroon.v2"
)

const (
	uriOpenChannel      = "/lnrpc.Lightning/OpenChannel"
	uriOpenChannelSync  = "/lnrpc.Lightning/OpenChannelSync"
	uriBatchOpenChannel = "/lnrpc.Lightning/BatchOpenChannel"
	uriCloseChannel     = "/lnrpc.Lightning/CloseChannel"
	uriUpdateChanPolicy = "/lnrpc.Lightning/UpdateChannelPolicy"
	uriGetInfo          = "/lnrpc.Lightning/GetInfo"
)

var testPassword = []byte("hello")

// TestChannelManagementV1FieldRules runs the channel-management-v1 profile's
// field rules against a matrix of requests and makes sure exactly the value
// redirection vectors are denied.
func TestChannelManagementV1FieldRules(t *testing.T) {
	t.Parallel()

	profile := protectorProfiles[ChannelManagementV1]
	require.NotNil(t, profile)

	// A fully populated request that only uses vetted fields, to make
	// sure allowed fields never trip the rules.
	cleanOpen := &lnrpc.OpenChannelRequest{
		NodePubkey:         []byte{0x02, 0x03},
		LocalFundingAmount: 1_000_000,
		SatPerVbyte:        10,
		TargetConf:         6,
		Private:            true,
		MinHtlcMsat:        1000,
		RemoteCsvDelay:     144,
		MinConfs:           1,
		SpendUnconfirmed:   false,
		CommitmentType:     lnrpc.CommitmentType_ANCHORS,
		ZeroConf:           true,
		ScidAlias:          true,
		BaseFee:            1000,
		FeeRate:            1,
		UseBaseFee:         true,
		UseFeeRate:         true,
		FundMax:            true,
		Memo:               "chan mgmt service",
		Outpoints: []*lnrpc.OutPoint{{
			TxidStr:     "abcd",
			OutputIndex: 1,
		}},
	}

	testCases := []struct {
		name    string
		uri     string
		req     protoMessage
		wantErr string
	}{{
		name: "open clean",
		uri:  uriOpenChannel,
		req:  cleanOpen,
	}, {
		name: "open sync clean",
		uri:  uriOpenChannelSync,
		req:  cleanOpen,
	}, {
		name: "open push_sat denied",
		uri:  uriOpenChannel,
		req: &lnrpc.OpenChannelRequest{
			LocalFundingAmount: 1_000_000,
			PushSat:            1,
		},
		wantErr: "push_sat",
	}, {
		name: "open sync push_sat denied",
		uri:  uriOpenChannelSync,
		req: &lnrpc.OpenChannelRequest{
			LocalFundingAmount: 1_000_000,
			PushSat:            1,
		},
		wantErr: "push_sat",
	}, {
		name: "open close_address denied",
		uri:  uriOpenChannel,
		req: &lnrpc.OpenChannelRequest{
			LocalFundingAmount: 1_000_000,
			CloseAddress:       "bc1qattacker",
		},
		wantErr: "close_address",
	}, {
		name: "open funding_shim denied",
		uri:  uriOpenChannel,
		req: &lnrpc.OpenChannelRequest{
			LocalFundingAmount: 1_000_000,
			FundingShim:        &lnrpc.FundingShim{},
		},
		wantErr: "funding_shim",
	}, {
		name: "batch open clean",
		uri:  uriBatchOpenChannel,
		req: &lnrpc.BatchOpenChannelRequest{
			SatPerVbyte: 10,
			Label:       "batch",
			Channels: []*lnrpc.BatchOpenChannel{{
				NodePubkey:         []byte{0x02},
				LocalFundingAmount: 100_000,
			}, {
				NodePubkey:         []byte{0x03},
				LocalFundingAmount: 200_000,
				Private:            true,
			}},
		},
	}, {
		name: "batch open push_sat in second channel denied",
		uri:  uriBatchOpenChannel,
		req: &lnrpc.BatchOpenChannelRequest{
			Channels: []*lnrpc.BatchOpenChannel{{
				NodePubkey:         []byte{0x02},
				LocalFundingAmount: 100_000,
			}, {
				NodePubkey:         []byte{0x03},
				LocalFundingAmount: 200_000,
				PushSat:            1,
			}},
		},
		wantErr: "push_sat",
	}, {
		name: "batch open close_address denied",
		uri:  uriBatchOpenChannel,
		req: &lnrpc.BatchOpenChannelRequest{
			Channels: []*lnrpc.BatchOpenChannel{{
				NodePubkey:         []byte{0x02},
				LocalFundingAmount: 100_000,
				CloseAddress:       "bc1qattacker",
			}},
		},
		wantErr: "close_address",
	}, {
		name: "close clean",
		uri:  uriCloseChannel,
		req: &lnrpc.CloseChannelRequest{
			ChannelPoint: &lnrpc.ChannelPoint{},
			Force:        true,
			SatPerVbyte:  10,
			NoWait:       true,
		},
	}, {
		name: "close delivery_address denied",
		uri:  uriCloseChannel,
		req: &lnrpc.CloseChannelRequest{
			ChannelPoint:    &lnrpc.ChannelPoint{},
			DeliveryAddress: "bc1qattacker",
		},
		wantErr: "delivery_address",
	}, {
		name: "policy update fully populated passes",
		uri:  uriUpdateChanPolicy,
		req: &lnrpc.PolicyUpdateRequest{
			Scope: &lnrpc.PolicyUpdateRequest_Global{
				Global: true,
			},
			BaseFeeMsat:          1000,
			FeeRatePpm:           500,
			TimeLockDelta:        80,
			MaxHtlcMsat:          1_000_000,
			MinHtlcMsat:          1000,
			MinHtlcMsatSpecified: true,
			InboundFee:           &lnrpc.InboundFee{},
			CreateMissingEdge:    true,
		},
	}, {
		name: "uncovered method passes",
		uri:  uriGetInfo,
		req:  &lnrpc.GetInfoRequest{},
	}, {
		name: "wrong message type for covered method rejected",
		uri:  uriOpenChannel,
		req: &lnrpc.CloseChannelRequest{
			Force: true,
		},
		wantErr: "unexpected request message type",
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := profile.checkRequest(tc.uri, tc.req)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

// protoMessage is a local alias to keep the test table declaration readable.
type protoMessage = interface {
	ProtoReflect() protoreflect.Message
}

// TestProtectorUnclassifiedFieldFailsClosed makes sure enforcement itself
// rejects populated fields that a rule table failed to classify, so the
// exhaustiveness unit test is a development aid, not the security boundary.
func TestProtectorUnclassifiedFieldFailsClosed(t *testing.T) {
	t.Parallel()

	// A synthetic (incomplete) rule table for OpenChannelRequest that
	// does not classify the memo field at all.
	incomplete := &protectorProfile{
		name: "incomplete-test-profile",
		methods: map[string]*protectorFieldRules{
			uriOpenChannel: {
				msgName: proto.MessageName(
					&lnrpc.OpenChannelRequest{},
				),
				allowed: fieldSet("local_funding_amount"),
			},
		},
	}

	// The classified field passes.
	err := incomplete.checkRequest(
		uriOpenChannel, &lnrpc.OpenChannelRequest{
			LocalFundingAmount: 1_000_000,
		},
	)
	require.NoError(t, err)

	// The unclassified field is rejected at runtime.
	err = incomplete.checkRequest(
		uriOpenChannel, &lnrpc.OpenChannelRequest{
			LocalFundingAmount: 1_000_000,
			Memo:               "unclassified",
		},
	)
	require.ErrorContains(t, err, "not classified")
}

// TestProtectorProfilesExhaustive walks the proto descriptors of every
// message covered by every protector profile and asserts that:
//
//  1. Every field of the message is explicitly classified as either allowed,
//     denied or nested. This forces any field added to these messages in a
//     future release to be classified before it can ship.
//  2. Every classified field name actually exists in the message descriptor,
//     so a typo in a rule table cannot silently disable a denial.
//  3. Every method URI in a profile exists in the proto service definition
//     and its request type matches the rule set's message type, so rules can
//     never be bound to the wrong method.
func TestProtectorProfilesExhaustive(t *testing.T) {
	t.Parallel()

	for name, profile := range protectorProfiles {
		require.Equal(t, name, profile.name)
		require.NoError(
			t, macaroons.ValidateProtectorProfileName(name),
		)
		require.NotEmpty(t, profile.description)
		require.NotEmpty(t, profile.methods)

		for uri, rules := range profile.methods {
			assertMethodBinding(t, uri, rules)
			assertRulesExhaustive(t, uri, rules)
		}
	}
}

// assertMethodBinding makes sure the given URI refers to an existing RPC
// method whose request message type matches the rule set.
func assertMethodBinding(t *testing.T, uri string,
	rules *protectorFieldRules) {

	t.Helper()

	parts := strings.Split(strings.TrimPrefix(uri, "/"), "/")
	require.Len(t, parts, 2, "invalid method URI %s", uri)

	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(
		protoreflect.FullName(parts[0]),
	)
	require.NoError(t, err, "unknown service in URI %s", uri)

	svcDesc, ok := desc.(protoreflect.ServiceDescriptor)
	require.True(t, ok, "%s is not a service", parts[0])

	method := svcDesc.Methods().ByName(protoreflect.Name(parts[1]))
	require.NotNil(t, method, "unknown method in URI %s", uri)

	require.Equal(
		t, rules.msgName, method.Input().FullName(),
		"rules for %s are bound to the wrong message type", uri,
	)
}

// assertRulesExhaustive checks classification of all fields of the rule set's
// message, recursing into nested rule sets.
func assertRulesExhaustive(t *testing.T, uri string,
	rules *protectorFieldRules) {

	t.Helper()

	msgType, err := protoregistry.GlobalTypes.FindMessageByName(
		rules.msgName,
	)
	require.NoError(t, err, "unknown message type %s", rules.msgName)

	fields := msgType.Descriptor().Fields()
	fieldNames := make(map[protoreflect.Name]struct{}, fields.Len())

	// Every field of the message must be classified exactly once.
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		name := field.Name()
		fieldNames[name] = struct{}{}

		classifications := 0
		if _, ok := rules.allowed[name]; ok {
			classifications++
		}
		if _, ok := rules.denied[name]; ok {
			classifications++
		}
		if sub, ok := rules.nested[name]; ok {
			classifications++

			require.Equal(
				t, protoreflect.MessageKind, field.Kind(),
				"nested rule for non-message field %q of %s",
				name, rules.msgName,
			)
			require.False(
				t, field.IsMap(),
				"nested rules for map fields are not "+
					"supported (field %q of %s)",
				name, rules.msgName,
			)

			assertRulesExhaustive(t, uri, sub)
		}

		require.Equal(t, 1, classifications,
			"field %q of %s (method %s) must be classified as "+
				"exactly one of allowed/denied/nested; a new "+
				"proto field must be explicitly vetted here "+
				"before it can ship", name, rules.msgName, uri)
	}

	// Every classified name must exist in the descriptor, catching typos
	// that would otherwise silently disable a denial.
	for _, set := range []map[protoreflect.Name]struct{}{
		rules.allowed, rules.denied,
	} {
		for name := range set {
			require.Contains(t, fieldNames, name,
				"rule table for %s references unknown "+
					"field %q", rules.msgName, name)
		}
	}
	for name := range rules.nested {
		require.Contains(t, fieldNames, name,
			"rule table for %s references unknown field %q",
			rules.msgName, name)
	}
}

// newProtectorTestChain creates an InterceptorChain suitable for direct unit
// testing of the protector enforcement helpers.
func newProtectorTestChain(t *testing.T, noMacaroons bool) *InterceptorChain {
	chain := NewInterceptorChain(btclog.Disabled, noMacaroons, nil)
	require.NoError(t, chain.Start())
	t.Cleanup(func() {
		require.NoError(t, chain.Stop())
	})

	return chain
}

// dummyProtectorMacaroon creates a standalone macaroon (no bakery involved)
// with the given caveat strings.
func dummyProtectorMacaroon(t *testing.T, caveats ...string) []byte {
	mac, err := macaroon.New(
		[]byte("rootkey"), []byte("id"), "lnd", macaroon.LatestVersion,
	)
	require.NoError(t, err)

	for _, caveat := range caveats {
		require.NoError(t, mac.AddFirstPartyCaveat([]byte(caveat)))
	}

	macBytes, err := mac.MarshalBinary()
	require.NoError(t, err)

	return macBytes
}

// ctxWithMacaroon returns an incoming gRPC context carrying the given
// serialized macaroon.
func ctxWithMacaroon(macBytes []byte) context.Context {
	md := metadata.Pairs("macaroon", hex.EncodeToString(macBytes))
	return metadata.NewIncomingContext(context.Background(), md)
}

// TestEnforceProtectorCaveats tests the protector enforcement helper in
// isolation, without a macaroon validator involved.
func TestEnforceProtectorCaveats(t *testing.T) {
	t.Parallel()

	chain := newProtectorTestChain(t, false)

	protectedMac := dummyProtectorMacaroon(
		t, "protector "+ChannelManagementV1,
	)
	deniedReq := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 1_000_000,
		PushSat:            1,
	}
	cleanReq := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 1_000_000,
	}

	// Without a macaroon in the context there is nothing to enforce.
	err := chain.enforceProtectorCaveats(
		t.Context(), uriOpenChannel, deniedReq,
	)
	require.NoError(t, err)

	// A macaroon without protector caveats doesn't restrict anything.
	plainMac := dummyProtectorMacaroon(t)
	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(plainMac), uriOpenChannel, deniedReq,
	)
	require.NoError(t, err)

	// A protected macaroon rejects a denied field...
	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(protectedMac), uriOpenChannel, deniedReq,
	)
	require.ErrorContains(t, err, "push_sat")

	// ... on every covered method...
	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(protectedMac), uriCloseChannel,
		&lnrpc.CloseChannelRequest{DeliveryAddress: "bc1qattacker"},
	)
	require.ErrorContains(t, err, "delivery_address")

	// ... but passes clean requests and uncovered methods.
	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(protectedMac), uriOpenChannel, cleanReq,
	)
	require.NoError(t, err)

	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(protectedMac), uriGetInfo,
		&lnrpc.GetInfoRequest{},
	)
	require.NoError(t, err)

	// An unknown profile fails closed, with and without a request
	// message.
	unknownMac := dummyProtectorMacaroon(t, "protector future-v9")
	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(unknownMac), uriOpenChannel, cleanReq,
	)
	require.ErrorContains(t, err, "future-v9")

	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(unknownMac), uriOpenChannel, nil,
	)
	require.ErrorContains(t, err, "future-v9")

	// A malformed protector caveat fails closed as well.
	malformedMac := dummyProtectorMacaroon(t, "protector")
	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(malformedMac), uriOpenChannel, cleanReq,
	)
	require.Error(t, err)

	// A request that cannot be inspected (nil or non-proto) fails closed
	// when a protector caveat covers the method.
	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(protectedMac), uriOpenChannel, nil,
	)
	require.ErrorContains(t, err, "cannot be inspected")

	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(protectedMac), uriOpenChannel,
		struct{ notAProto bool }{},
	)
	require.ErrorContains(t, err, "cannot be inspected")

	// On a method the profile does NOT cover, even an uninspectable
	// request passes: profiles have no opinion on uncovered methods, and
	// externally registered services may use request types lnd cannot
	// inspect.
	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(protectedMac), uriGetInfo,
		struct{ notAProto bool }{},
	)
	require.NoError(t, err)

	// Duplicate protector caveats of the same profile are simply enforced
	// (twice); they cannot cancel each other out.
	dupCaveat := "protector " + ChannelManagementV1
	dupCaveatMac := dummyProtectorMacaroon(t, dupCaveat, dupCaveat)
	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(dupCaveatMac), uriOpenChannel, deniedReq,
	)
	require.ErrorContains(t, err, "push_sat")

	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(dupCaveatMac), uriOpenChannel, cleanReq,
	)
	require.NoError(t, err)

	// A macaroon combining a known and an unknown profile is rejected as
	// a whole; the known profile cannot make the unknown one acceptable.
	mixedMac := dummyProtectorMacaroon(
		t, "protector "+ChannelManagementV1, "protector future-v9",
	)
	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(mixedMac), uriOpenChannel, cleanReq,
	)
	require.ErrorContains(t, err, "future-v9")

	// Ambiguous macaroon metadata fails closed: more than one macaroon
	// value could let a lenient external validator authenticate one
	// macaroon while enforcement inspects another, so it is rejected
	// outright.
	dupCtx := metadata.NewIncomingContext(
		t.Context(), metadata.Pairs(
			"macaroon", hex.EncodeToString(protectedMac),
			"macaroon", hex.EncodeToString(protectedMac),
		),
	)
	err = chain.enforceProtectorCaveats(dupCtx, uriOpenChannel, cleanReq)
	require.ErrorContains(t, err, "exactly 1 macaroon")

	// A macaroon value that isn't valid hex or isn't a parseable macaroon
	// fails closed as well.
	garbageCtx := metadata.NewIncomingContext(
		t.Context(), metadata.Pairs("macaroon", "not-hex"),
	)
	err = chain.enforceProtectorCaveats(
		garbageCtx, uriOpenChannel, cleanReq,
	)
	require.ErrorContains(t, err, "hex")

	// Whitelisted methods and noMacaroons mode skip enforcement entirely.
	err = chain.enforceProtectorCaveats(
		ctxWithMacaroon(protectedMac), "/lnrpc.State/GetState",
		deniedReq,
	)
	require.NoError(t, err)

	noMacChain := newProtectorTestChain(t, true)
	err = noMacChain.enforceProtectorCaveats(
		ctxWithMacaroon(protectedMac), uriOpenChannel, deniedReq,
	)
	require.NoError(t, err)
}

// acceptAllValidator simulates an external macaroon validator (like the one
// litd registers for super macaroons) that accepts any macaroon without
// knowing anything about protector caveats.
type acceptAllValidator struct{}

func (a *acceptAllValidator) ValidateMacaroon(_ context.Context,
	_ []bakery.Op, _ string) error {

	return nil
}

// setupMacaroonService creates a fully functional macaroon service backed by
// a temporary database, with the protector checker registered against the
// given interceptor chain.
func setupMacaroonService(t *testing.T,
	chain *InterceptorChain) *macaroons.Service {

	db, err := kvdb.Create(
		kvdb.BoltBackendName, path.Join(t.TempDir(), "macaroons.db"),
		true, kvdb.DefaultDBTimeout, false,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	store, err := macaroons.NewRootKeyStorage(db)
	require.NoError(t, err)

	service, err := macaroons.NewService(
		store, "lnd", false, macaroons.IPLockChecker,
		macaroons.CustomChecker(chain),
		macaroons.ProtectorChecker(chain),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, service.Close())
	})

	require.NoError(t, service.CreateUnlock(&testPassword))

	return service
}

// bakeProtectedMacaroon bakes a real macaroon with the given ops through the
// service and attaches the given protector profile caveat.
func bakeProtectedMacaroon(t *testing.T, service *macaroons.Service,
	profile string, ops ...bakery.Op) []byte {

	bakedMac, err := service.NewMacaroon(
		t.Context(), macaroons.DefaultRootKeyID, ops...,
	)
	require.NoError(t, err)

	constrainedMac, err := macaroons.AddConstraints(
		bakedMac.M(), macaroons.ProtectorConstraint(profile),
	)
	require.NoError(t, err)

	macBytes, err := constrainedMac.MarshalBinary()
	require.NoError(t, err)

	return macBytes
}

// runUnaryAuth composes macaroon validation and protector enforcement in the
// same order the real interceptor chain runs them for a unary RPC.
func runUnaryAuth(chain *InterceptorChain, ctx context.Context, uri string,
	req interface{}) error {

	if err := chain.checkMacaroon(ctx, uri); err != nil {
		return err
	}

	_, err := chain.ProtectorUnaryServerInterceptor()(
		ctx, req, &grpc.UnaryServerInfo{FullMethod: uri},
		func(_ context.Context, _ interface{}) (interface{}, error) {
			return nil, nil
		},
	)

	return err
}

// TestCheckMacaroonProtector tests macaroon validation plus protector
// enforcement with protector caveats, through both the internal bakery
// validator and, crucially, through an external validator that accepts
// everything: field enforcement must hold either way, so an external
// validator that doesn't know about protector caveats cannot be used to
// bypass them.
func TestCheckMacaroonProtector(t *testing.T) {
	t.Parallel()

	writeOp := bakery.Op{Entity: "offchain", Action: "write"}

	deniedReq := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 1_000_000,
		PushSat:            1,
	}
	cleanReq := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 1_000_000,
	}

	setup := func(t *testing.T) (*InterceptorChain, *macaroons.Service) {
		chain := newProtectorTestChain(t, false)
		service := setupMacaroonService(t, chain)
		chain.AddMacaroonService(service)
		require.NoError(t, chain.AddPermission(
			uriOpenChannel, []bakery.Op{writeOp},
		))

		return chain, service
	}

	// Internal validator path: the bakery recognizes the caveat and the
	// interceptor enforces the field rules.
	t.Run("internal validator", func(t *testing.T) {
		t.Parallel()

		chain, service := setup(t)
		macBytes := bakeProtectedMacaroon(
			t, service, ChannelManagementV1, writeOp,
		)

		err := runUnaryAuth(
			chain, ctxWithMacaroon(macBytes), uriOpenChannel,
			deniedReq,
		)
		require.ErrorContains(t, err, "push_sat")

		err = runUnaryAuth(
			chain, ctxWithMacaroon(macBytes), uriOpenChannel,
			cleanReq,
		)
		require.NoError(t, err)

		// A macaroon referencing an unknown profile is rejected by
		// the bakery itself.
		unknownProfileMac := bakeProtectedMacaroon(
			t, service, "future-v9", writeOp,
		)
		err = runUnaryAuth(
			chain, ctxWithMacaroon(unknownProfileMac),
			uriOpenChannel, cleanReq,
		)
		require.Error(t, err)
	})

	// External validator path: even if a registered external validator
	// blindly accepts the macaroon, protector enforcement must still run
	// and reject denied fields and unknown profiles.
	t.Run("external validator cannot bypass", func(t *testing.T) {
		t.Parallel()

		chain, service := setup(t)
		err := service.RegisterExternalValidator(
			uriOpenChannel, &acceptAllValidator{},
		)
		require.NoError(t, err)

		macBytes := bakeProtectedMacaroon(
			t, service, ChannelManagementV1, writeOp,
		)

		err = runUnaryAuth(
			chain, ctxWithMacaroon(macBytes), uriOpenChannel,
			deniedReq,
		)
		require.ErrorContains(t, err, "push_sat")

		err = runUnaryAuth(
			chain, ctxWithMacaroon(macBytes), uriOpenChannel,
			cleanReq,
		)
		require.NoError(t, err)

		// The external validator accepts the unknown profile (it does
		// not run the bakery), so the interceptor's own enforcement
		// must reject it.
		unknownProfileMac := bakeProtectedMacaroon(
			t, service, "future-v9", writeOp,
		)
		err = runUnaryAuth(
			chain, ctxWithMacaroon(unknownProfileMac),
			uriOpenChannel, cleanReq,
		)
		require.ErrorContains(t, err, "future-v9")
	})
}

// TestProtectorAfterMiddlewareRewrite makes sure protector enforcement holds
// for the FINAL request the handler executes: a registered RPC middleware is
// allowed to replace the request message, so the protector interceptor must
// run inside of (after) the middleware interceptor and judge the replacement,
// not the original request. This test simulates a middleware that rewrites a
// clean request into one carrying a denied field and asserts the rewritten
// request is rejected.
func TestProtectorAfterMiddlewareRewrite(t *testing.T) {
	t.Parallel()

	chain := newProtectorTestChain(t, false)
	protectedMac := dummyProtectorMacaroon(
		t, "protector "+ChannelManagementV1,
	)

	cleanReq := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 1_000_000,
	}
	rewrittenReq := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 1_000_000,
		PushSat:            10_000,
	}

	// A middleware-like interceptor that replaces the request before
	// passing it on, the same way middlewareUnaryServerInterceptor swaps
	// in a replacement from a registered middleware.
	rewritingMiddleware := func(ctx context.Context, _ interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		return handler(ctx, rewrittenReq)
	}

	handlerCalled := false
	chained := grpc_middleware.ChainUnaryServer(
		rewritingMiddleware, chain.ProtectorUnaryServerInterceptor(),
	)
	_, err := chained(
		ctxWithMacaroon(protectedMac), cleanReq,
		&grpc.UnaryServerInfo{FullMethod: uriOpenChannel},
		func(_ context.Context, _ interface{}) (interface{}, error) {
			handlerCalled = true
			return nil, nil
		},
	)
	require.ErrorContains(t, err, "push_sat")
	require.False(t, handlerCalled)
}

// fakeServerStream is a minimal grpc.ServerStream stub that returns a fixed
// request message on RecvMsg.
type fakeServerStream struct {
	grpc.ServerStream

	//nolint:containedctx
	ctx context.Context
	req *lnrpc.OpenChannelRequest
}

func (f *fakeServerStream) Context() context.Context {
	return f.ctx
}

func (f *fakeServerStream) RecvMsg(m interface{}) error {
	req, ok := m.(*lnrpc.OpenChannelRequest)
	if !ok {
		return nil
	}

	proto.Merge(req, f.req)

	return nil
}

// TestProtectorStreamWrapper makes sure streaming RPCs get per-message
// protector enforcement: the request message of a (streaming) RPC only
// becomes available on RecvMsg, after the macaroon check at stream open.
func TestProtectorStreamWrapper(t *testing.T) {
	t.Parallel()

	chain := newProtectorTestChain(t, false)
	protectedMac := dummyProtectorMacaroon(
		t, "protector "+ChannelManagementV1,
	)

	// A stream without protector caveats is passed through unwrapped.
	plainStream := &fakeServerStream{
		ctx: ctxWithMacaroon(dummyProtectorMacaroon(t)),
	}
	ss, err := chain.wrapStreamForProtector(plainStream, uriOpenChannel)
	require.NoError(t, err)
	require.Same(t, grpc.ServerStream(plainStream), ss)

	// A stream whose macaroon carries a protector caveat is wrapped and
	// rejects denied fields on RecvMsg.
	deniedStream := &fakeServerStream{
		ctx: ctxWithMacaroon(protectedMac),
		req: &lnrpc.OpenChannelRequest{
			LocalFundingAmount: 1_000_000,
			CloseAddress:       "bc1qattacker",
		},
	}
	ss, err = chain.wrapStreamForProtector(deniedStream, uriOpenChannel)
	require.NoError(t, err)
	require.NotSame(t, grpc.ServerStream(deniedStream), ss)

	err = ss.RecvMsg(&lnrpc.OpenChannelRequest{})
	require.ErrorContains(t, err, "close_address")

	// Clean requests pass through the wrapper.
	cleanStream := &fakeServerStream{
		ctx: ctxWithMacaroon(protectedMac),
		req: &lnrpc.OpenChannelRequest{
			LocalFundingAmount: 1_000_000,
		},
	}
	ss, err = chain.wrapStreamForProtector(cleanStream, uriOpenChannel)
	require.NoError(t, err)

	err = ss.RecvMsg(&lnrpc.OpenChannelRequest{})
	require.NoError(t, err)

	// A macaroon with an unknown profile fails closed at wrap time, i.e.
	// at stream open, before the handler ever runs.
	unknownStream := &fakeServerStream{
		ctx: ctxWithMacaroon(
			dummyProtectorMacaroon(t, "protector future-v9"),
		),
	}
	_, err = chain.wrapStreamForProtector(unknownStream, uriOpenChannel)
	require.ErrorContains(t, err, "future-v9")

	// A protected stream on a method the profile doesn't cover is passed
	// through unwrapped; the profile has no opinion on it.
	uncoveredStream := &fakeServerStream{
		ctx: ctxWithMacaroon(protectedMac),
	}
	ss, err = chain.wrapStreamForProtector(
		uncoveredStream, "/lnrpc.Lightning/SubscribeInvoices",
	)
	require.NoError(t, err)
	require.Same(t, grpc.ServerStream(uncoveredStream), ss)

	// A malformed protector caveat causes the wrap itself to fail closed.
	malformedStream := &fakeServerStream{
		ctx: ctxWithMacaroon(dummyProtectorMacaroon(t, "protector")),
	}
	_, err = chain.wrapStreamForProtector(malformedStream, uriOpenChannel)
	require.Error(t, err)
}

// fakeLightningServer implements just enough of the Lightning service to
// observe whether a request made it past the interceptor chain.
type fakeLightningServer struct {
	lnrpc.UnimplementedLightningServer
}

func (f *fakeLightningServer) OpenChannelSync(_ context.Context,
	_ *lnrpc.OpenChannelRequest) (*lnrpc.ChannelPoint, error) {

	return &lnrpc.ChannelPoint{OutputIndex: 42}, nil
}

func (f *fakeLightningServer) OpenChannel(_ *lnrpc.OpenChannelRequest,
	stream lnrpc.Lightning_OpenChannelServer) error {

	return stream.Send(&lnrpc.OpenStatusUpdate{
		PendingChanId: []byte{0x01},
	})
}

// TestProtectorEndToEnd spins up a real gRPC server with the full interceptor
// chain and makes sure protector enforcement holds over the wire, for both
// unary and (server) streaming RPCs. The streaming case is the important one:
// the request message of a streaming RPC only becomes available via RecvMsg
// inside the generated method handler, so this test proves the stream wrapper
// is actually in that path.
func TestProtectorEndToEnd(t *testing.T) {
	t.Parallel()

	writeOp := bakery.Op{Entity: "offchain", Action: "write"}

	// Set up the interceptor chain in an active RPC state, with a real
	// macaroon service attached.
	chain := newProtectorTestChain(t, false)
	chain.SetWalletUnlocked()
	chain.SetRPCActive()

	service := setupMacaroonService(t, chain)
	chain.AddMacaroonService(service)
	require.NoError(t, chain.AddPermission(
		uriOpenChannel, []bakery.Op{writeOp},
	))
	require.NoError(t, chain.AddPermission(
		uriOpenChannelSync, []bakery.Op{writeOp},
	))

	// Start a gRPC server over an in-memory connection, using the exact
	// server options lnd itself uses.
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(chain.CreateServerOpts()...)
	lnrpc.RegisterLightningServer(grpcServer, &fakeLightningServer{})

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- grpcServer.Serve(listener)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		require.NoError(t, <-serverErr)
	})

	conn, err := grpc.Dial(
		"bufconn",
		grpc.WithContextDialer(func(ctx context.Context,
			_ string) (net.Conn, error) {

			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})

	client := lnrpc.NewLightningClient(conn)

	protectedMac := bakeProtectedMacaroon(
		t, service, ChannelManagementV1, writeOp,
	)

	// Bake a second macaroon with the same permissions but without the
	// protector caveat, to prove the restriction comes from the caveat
	// and not from the endpoint itself.
	plainBaked, err := service.NewMacaroon(
		t.Context(), macaroons.DefaultRootKeyID, writeOp,
	)
	require.NoError(t, err)
	plainMac, err := plainBaked.M().MarshalBinary()
	require.NoError(t, err)

	callCtx := func(macBytes []byte) context.Context {
		return metadata.AppendToOutgoingContext(
			t.Context(), "macaroon",
			hex.EncodeToString(macBytes),
		)
	}

	deniedReq := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 1_000_000,
		PushSat:            1,
	}
	cleanReq := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 1_000_000,
	}

	// Unary: a denied field is rejected with the protected macaroon but
	// accepted with the plain one; clean requests always pass.
	_, err = client.OpenChannelSync(callCtx(protectedMac), deniedReq)
	require.ErrorContains(t, err, "push_sat")

	resp, err := client.OpenChannelSync(callCtx(protectedMac), cleanReq)
	require.NoError(t, err)
	require.EqualValues(t, 42, resp.OutputIndex)

	_, err = client.OpenChannelSync(callCtx(plainMac), deniedReq)
	require.NoError(t, err)

	// Streaming: same matrix over the server streaming OpenChannel RPC.
	// The error only surfaces on the first Recv.
	stream, err := client.OpenChannel(callCtx(protectedMac), deniedReq)
	require.NoError(t, err)
	_, err = stream.Recv()
	require.ErrorContains(t, err, "push_sat")

	stream, err = client.OpenChannel(callCtx(protectedMac), cleanReq)
	require.NoError(t, err)
	update, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, []byte{0x01}, update.PendingChanId)

	stream, err = client.OpenChannel(callCtx(plainMac), deniedReq)
	require.NoError(t, err)
	_, err = stream.Recv()
	require.NoError(t, err)

	// A macaroon with an unknown (future) profile is rejected outright,
	// even on a clean request.
	futureMac := bakeProtectedMacaroon(t, service, "future-v9", writeOp)
	_, err = client.OpenChannelSync(callCtx(futureMac), cleanReq)
	require.Error(t, err)

	// Finally, register an external validator for the unary method that
	// blindly accepts any macaroon (like litd's super macaroon validator
	// would) and make sure protector enforcement still holds over the
	// wire.
	err = service.RegisterExternalValidator(
		uriOpenChannelSync, &acceptAllValidator{},
	)
	require.NoError(t, err)

	_, err = client.OpenChannelSync(callCtx(protectedMac), deniedReq)
	require.ErrorContains(t, err, "push_sat")

	resp, err = client.OpenChannelSync(callCtx(protectedMac), cleanReq)
	require.NoError(t, err)
	require.EqualValues(t, 42, resp.OutputIndex)

	_, err = client.OpenChannelSync(callCtx(futureMac), cleanReq)
	require.ErrorContains(t, err, "future-v9")
}
