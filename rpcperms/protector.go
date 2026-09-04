package rpcperms

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	macaroon "gopkg.in/macaroon.v2"
)

// protectorFieldRules describes, for a single request message type, which
// fields a protector profile denies and which it has vetted as safe. Fields
// that hold nested messages can carry their own rule set that is applied
// recursively to every populated element.
type protectorFieldRules struct {
	// msgName is the fully qualified proto name of the request message
	// these rules apply to. Enforcement rejects any message of a different
	// type, so a rule set can never silently be applied to (or skipped
	// for) the wrong message.
	msgName protoreflect.FullName

	// denied contains the fields that must not be set on the request. For
	// proto3 scalar fields without explicit presence, "set" means carrying
	// a non-default value.
	denied map[protoreflect.Name]struct{}

	// allowed contains the fields that were vetted as safe to set. Any
	// populated field that is neither allowed, denied nor nested is
	// rejected at enforcement time (fail closed by construction). The
	// exhaustiveness unit test additionally asserts at build time that
	// every field of the message is explicitly classified, so a newly
	// added proto field surfaces as a test failure instead of as runtime
	// denials for users.
	allowed map[protoreflect.Name]struct{}

	// nested maps message (or repeated message) fields to the rules that
	// are applied recursively to each populated element.
	nested map[protoreflect.Name]*protectorFieldRules
}

// check walks all populated fields of the given message and returns an error
// if any denied field is set. Nested rules are applied recursively. Any
// populated field that is not explicitly classified as allowed, denied or
// nested is rejected as well: unclassified fields fail closed by
// construction, so even if the exhaustiveness unit test were ever skipped, a
// newly added proto field could never slip through enforcement.
func (r *protectorFieldRules) check(msg protoreflect.Message) error {
	if name := msg.Descriptor().FullName(); name != r.msgName {
		return fmt.Errorf("unexpected request message type %v, "+
			"expected %v", name, r.msgName)
	}

	var checkErr error
	msg.Range(func(fd protoreflect.FieldDescriptor,
		v protoreflect.Value) bool {

		name := fd.Name()
		if _, ok := r.denied[name]; ok {
			checkErr = fmt.Errorf("field %q must not be set", name)
			return false
		}

		if sub, ok := r.nested[name]; ok {
			switch {
			case fd.IsList():
				list := v.List()
				for i := 0; i < list.Len(); i++ {
					checkErr = sub.check(
						list.Get(i).Message(),
					)
					if checkErr != nil {
						return false
					}
				}

			case fd.Message() != nil && !fd.IsMap():
				checkErr = sub.check(v.Message())
				if checkErr != nil {
					return false
				}
			}

			return true
		}

		if _, ok := r.allowed[name]; ok {
			return true
		}

		checkErr = fmt.Errorf("field %q is not classified by this "+
			"profile and fails closed; it must not be set", name)

		return false
	})

	return checkErr
}

// protectorProfile is a named, compiled-in set of per-method field rules. A
// profile only constrains the methods listed in its rule map; any other method
// passes unchanged. Restricting which methods a macaroon can call at all
// remains the responsibility of the baker (via the macaroon's permission
// ops), which also allows several protector profiles to be combined on one
// macaroon without conflicting with each other.
type protectorProfile struct {
	// name is the profile name referenced by the macaroon caveat
	// "protector <name>". Once released, a name and the guarantee it
	// stands for are frozen: rules under an existing name may only ever be
	// tightened, never loosened. Changed or extended semantics require a
	// new profile name.
	name string

	// description is a short human readable summary of the guarantee the
	// profile provides.
	description string

	// methods maps full RPC URIs to the field rules enforced on their
	// request messages.
	methods map[string]*protectorFieldRules
}

// checkRequest enforces the profile's field rules against a single request
// message of the given method. Methods the profile does not cover pass.
func (p *protectorProfile) checkRequest(fullMethod string,
	req proto.Message) error {

	rules, ok := p.methods[fullMethod]
	if !ok {
		return nil
	}

	if err := rules.check(req.ProtoReflect()); err != nil {
		return status.Errorf(codes.PermissionDenied,
			"protector profile %q violation on %s: %v",
			p.name, fullMethod, err)
	}

	return nil
}

// ChannelManagementV1 is the name of the channel management protector profile,
// version 1.
const ChannelManagementV1 = "channel-management-v1"

// protectorProfiles is the registry of all protector profiles known to this
// version of lnd, keyed by profile name.
var protectorProfiles = map[string]*protectorProfile{
	ChannelManagementV1: newChannelManagementV1Profile(),
}

// fieldSet is a small helper to build field name sets for rule tables.
func fieldSet(names ...protoreflect.Name) map[protoreflect.Name]struct{} {
	set := make(map[protoreflect.Name]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}

	return set
}

// newChannelManagementV1Profile builds the channel-management-v1 profile. The
// guarantee of this profile is: the channel management methods it covers
// cannot redirect value to a third party. Concretely it denies pushing funds
// to the peer at open time (push_sat), pre-committing channel funds to an
// arbitrary address at close time (close_address, delivery_address) and
// non-standard funding flows whose outputs cannot be vetted here
// (funding_shim).
//
// Deliberately out of scope for v1: fee based value burn (fee rate fields
// remain settable) and any restriction on which peers channels may be opened
// with. Restricting the callable method set itself is the baker's job via the
// macaroon's permission ops.
func newChannelManagementV1Profile() *protectorProfile {
	// The rules for OpenChannelRequest, shared by the streaming and the
	// sync open methods.
	openRules := &protectorFieldRules{
		msgName: proto.MessageName(&lnrpc.OpenChannelRequest{}),
		denied: fieldSet(
			"push_sat",
			"close_address",
			"funding_shim",
		),
		allowed: fieldSet(
			"sat_per_vbyte",
			"node_pubkey",
			"node_pubkey_string",
			"local_funding_amount",
			"target_conf",
			"sat_per_byte",
			"private",
			"min_htlc_msat",
			"remote_csv_delay",
			"min_confs",
			"spend_unconfirmed",
			"remote_max_value_in_flight_msat",
			"remote_max_htlcs",
			"max_local_csv",
			"commitment_type",
			"zero_conf",
			"scid_alias",
			"base_fee",
			"fee_rate",
			"use_base_fee",
			"use_fee_rate",
			"remote_chan_reserve_sat",
			"fund_max",
			"memo",
			"outpoints",
		),
	}

	// The per-channel rules for the batch open method. The inner
	// BatchOpenChannel message has its own (smaller) field set but shares
	// the same two redirection vectors.
	batchChannelRules := &protectorFieldRules{
		msgName: proto.MessageName(&lnrpc.BatchOpenChannel{}),
		denied: fieldSet(
			"push_sat",
			"close_address",
		),
		allowed: fieldSet(
			"node_pubkey",
			"local_funding_amount",
			"private",
			"min_htlc_msat",
			"remote_csv_delay",
			"pending_chan_id",
			"commitment_type",
			"remote_max_value_in_flight_msat",
			"remote_max_htlcs",
			"max_local_csv",
			"zero_conf",
			"scid_alias",
			"base_fee",
			"fee_rate",
			"use_base_fee",
			"use_fee_rate",
			"remote_chan_reserve_sat",
			"memo",
		),
	}
	batchOpenRules := &protectorFieldRules{
		msgName: proto.MessageName(&lnrpc.BatchOpenChannelRequest{}),
		allowed: fieldSet(
			"target_conf",
			"sat_per_vbyte",
			"min_confs",
			"spend_unconfirmed",
			"label",
			"coin_selection_strategy",
		),
		nested: map[protoreflect.Name]*protectorFieldRules{
			"channels": batchChannelRules,
		},
	}

	closeRules := &protectorFieldRules{
		msgName: proto.MessageName(&lnrpc.CloseChannelRequest{}),
		denied: fieldSet(
			"delivery_address",
		),
		allowed: fieldSet(
			"channel_point",
			"force",
			"target_conf",
			"sat_per_byte",
			"sat_per_vbyte",
			"max_fee_per_vbyte",
			"no_wait",
		),
	}

	// UpdateChannelPolicy has no value redirection vector; all its fields
	// are vetted as safe. The entry exists to document that the method was
	// reviewed and to reserve the slot for future tightening.
	policyRules := &protectorFieldRules{
		msgName: proto.MessageName(&lnrpc.PolicyUpdateRequest{}),
		allowed: fieldSet(
			"global",
			"chan_point",
			"base_fee_msat",
			"fee_rate",
			"fee_rate_ppm",
			"time_lock_delta",
			"max_htlc_msat",
			"min_htlc_msat",
			"min_htlc_msat_specified",
			"inbound_fee",
			"create_missing_edge",
		),
	}

	return &protectorProfile{
		name: ChannelManagementV1,
		description: "channel management without the ability to " +
			"redirect value to a third party",
		methods: map[string]*protectorFieldRules{
			"/lnrpc.Lightning/OpenChannel":         openRules,
			"/lnrpc.Lightning/OpenChannelSync":     openRules,
			"/lnrpc.Lightning/BatchOpenChannel":    batchOpenRules,
			"/lnrpc.Lightning/CloseChannel":        closeRules,
			"/lnrpc.Lightning/UpdateChannelPolicy": policyRules,
		},
	}
}

// KnownProtectorProfile returns nil if a protector profile with the given
// name is compiled into this lnd instance.
//
// NOTE: This is part of the macaroons.ProtectorProfileChecker interface.
func (r *InterceptorChain) KnownProtectorProfile(profile string) error {
	if _, ok := protectorProfiles[profile]; !ok {
		return fmt.Errorf("unknown protector profile %q", profile)
	}

	return nil
}

// ProtectorUnaryServerInterceptor is a gRPC interceptor that enforces the
// protector caveats of the request's macaroon against the request message.
// It must be placed after (inside of) both the macaroon and the middleware
// interceptors: after the macaroon interceptor because enforcement assumes an
// already validated macaroon, and after the middleware interceptor because a
// registered middleware may replace the request message and the field rules
// must hold for the final request the handler will actually execute.
//
//nolint:ll
func (r *InterceptorChain) ProtectorUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		err := r.enforceProtectorCaveats(ctx, info.FullMethod, req)
		if err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// ProtectorStreamServerInterceptor is a gRPC interceptor that enforces the
// protector caveats of the stream's macaroon against every request message
// received from the client. Like the unary variant, it must be placed after
// (inside of) the macaroon and middleware interceptors, so the field rules
// are enforced on the final message after any middleware replacement.
//
//nolint:ll
func (r *InterceptorChain) ProtectorStreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		// If the macaroon carries protector caveats, wrap the stream
		// so the caveats' field rules are enforced on every request
		// message received from the client. This fails closed right at
		// stream open if the macaroon references an unknown profile or
		// its protector caveats cannot be resolved.
		wrappedSS, err := r.wrapStreamForProtector(ss, info.FullMethod)
		if err != nil {
			return err
		}

		return handler(srv, wrappedSS)
	}
}

// macaroonExempt returns true if the given method is exempt from macaroon
// authentication (and therefore also from protector enforcement): either
// macaroons are disabled entirely or the method is on the macaroon whitelist.
func (r *InterceptorChain) macaroonExempt(fullMethod string) bool {
	if r.noMacaroons {
		return true
	}
	_, ok := macaroonWhitelist[fullMethod]

	return ok
}

// checkCoveringProfiles enforces the field rules of the given (already
// coverage filtered) profiles against a single request message. It is the
// shared enforcement core of the unary interceptor and the stream wrapper.
func checkCoveringProfiles(covering []*protectorProfile, fullMethod string,
	m interface{}) error {

	// Fail closed: if a protector caveat covers this method but the
	// request cannot be inspected, the request must not proceed.
	msg, ok := m.(proto.Message)
	if !ok {
		return status.Errorf(codes.PermissionDenied,
			"protector caveat present but request of type %T "+
				"cannot be inspected", m)
	}

	for _, profile := range covering {
		if err := profile.checkRequest(fullMethod, msg); err != nil {
			return err
		}
	}

	return nil
}

// enforceProtectorCaveats resolves all protector caveats from the macaroon in
// the given context and enforces their profiles' field rules against the
// request message. It runs after macaroon validation succeeded and runs
// regardless of whether the internal bakery or an external validator accepted
// the macaroon, so an external validator that doesn't know about protector
// caveats cannot bypass their enforcement.
func (r *InterceptorChain) enforceProtectorCaveats(ctx context.Context,
	fullMethod string, req interface{}) error {

	// Requests that skip macaroon validation entirely also skip protector
	// enforcement: without macaroons there is no caveat to enforce.
	if r.macaroonExempt(fullMethod) {
		return nil
	}

	profiles, err := protectorProfilesFromContext(ctx)
	if err != nil {
		return err
	}

	// Methods not covered by any of the macaroon's profiles pass by
	// definition, without inspecting the request. This matters for
	// externally registered gRPC services (whose request types lnd cannot
	// inspect) that are reached with a protector caveated macaroon.
	covering := coveringProfiles(profiles, fullMethod)
	if len(covering) == 0 {
		return nil
	}

	return checkCoveringProfiles(covering, fullMethod, req)
}

// coveringProfiles filters the given profiles down to the ones that have
// field rules for the given method.
func coveringProfiles(profiles []*protectorProfile,
	fullMethod string) []*protectorProfile {

	var covering []*protectorProfile
	for _, profile := range profiles {
		if _, ok := profile.methods[fullMethod]; ok {
			covering = append(covering, profile)
		}
	}

	return covering
}

// protectorProfilesFromContext parses the macaroon from the given context and
// resolves the profiles of all protector caveats it carries. As opposed to
// the more tolerant macaroon parsing of the middleware handler, any ambiguity
// fails closed here: a request that carries more than one macaroon value, an
// unparseable macaroon, a malformed protector caveat or an unknown profile
// name all result in an error. Only the complete absence of a macaroon (a
// request that could never have been authenticated by one) yields no
// profiles.
func protectorProfilesFromContext(
	ctx context.Context) ([]*protectorProfile, error) {

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, nil
	}

	macValues := md.Get("macaroon")
	switch {
	case len(macValues) == 0:
		return nil, nil

	case len(macValues) > 1:
		return nil, status.Errorf(codes.PermissionDenied,
			"protector enforcement requires exactly 1 macaroon "+
				"metadata value, got %d", len(macValues))
	}

	macBytes, err := hex.DecodeString(macValues[0])
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied,
			"error hex decoding macaroon for protector "+
				"enforcement: %v", err)
	}

	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, status.Errorf(codes.PermissionDenied,
			"error parsing macaroon for protector "+
				"enforcement: %v", err)
	}

	names, err := macaroons.GetProtectorProfiles(mac)
	if err != nil {
		return nil, err
	}

	profiles := make([]*protectorProfile, 0, len(names))
	for _, name := range names {
		profile, ok := protectorProfiles[name]
		if !ok {
			return nil, status.Errorf(codes.PermissionDenied,
				"unknown protector profile %q", name)
		}

		profiles = append(profiles, profile)
	}

	return profiles, nil
}

// protectorStreamWrapper wraps a grpc.ServerStream to enforce protector
// caveats on every request message received from the client. This is required
// because the interceptors for streaming RPCs run at stream open, before any
// request message exists. The profiles are resolved once at wrap time, so the
// per-message cost is just the field rule check.
type protectorStreamWrapper struct {
	grpc.ServerStream

	profiles   []*protectorProfile
	fullMethod string
}

// RecvMsg receives a message from the underlying stream and enforces the
// protector caveats of the stream's macaroon against it before handing it to
// the handler.
func (w *protectorStreamWrapper) RecvMsg(m interface{}) error {
	if err := w.ServerStream.RecvMsg(m); err != nil {
		return err
	}

	return checkCoveringProfiles(w.profiles, w.fullMethod, m)
}

// wrapStreamForProtector wraps the given server stream with per-message
// protector enforcement if (and only if) the macaroon of the stream carries at
// least one protector caveat whose profile covers the streamed method.
// Streams without such caveats are returned unchanged, so the wrapper adds no
// cost to normal traffic. Resolution failures (ambiguous macaroon metadata,
// malformed caveats, unknown profiles) fail closed.
func (r *InterceptorChain) wrapStreamForProtector(ss grpc.ServerStream,
	fullMethod string) (grpc.ServerStream, error) {

	if r.macaroonExempt(fullMethod) {
		return ss, nil
	}

	profiles, err := protectorProfilesFromContext(ss.Context())
	if err != nil {
		return nil, err
	}

	// Only wrap if at least one profile actually covers this method; the
	// field rules of non-covering profiles pass any message by
	// definition.
	covering := coveringProfiles(profiles, fullMethod)
	if len(covering) == 0 {
		return ss, nil
	}

	return &protectorStreamWrapper{
		ServerStream: ss,
		profiles:     covering,
		fullMethod:   fullMethod,
	}, nil
}
