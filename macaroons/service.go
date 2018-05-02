package macaroons

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"

	"github.com/coreos/bbolt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	macaroon "gopkg.in/macaroon.v2"

	"golang.org/x/net/context"
)

var (
	// DBFilename is the filename within the data directory which contains
	// the macaroon stores.
	DBFilename = "macaroons.db"
)

// Service encapsulates bakery.Bakery and adds a Close() method that zeroes the
// root key service encryption keys, as well as utility methods to validate a
// macaroon against the bakery and gRPC middleware for macaroon-based auth.
type Service struct {
	bakery.Bakery

	rks *RootKeyStorage
}

// NewService returns a service backed by the macaroon Bolt DB stored in the
// passed directory. The `checks` argument can be any of the `Checker` type
// functions defined in this package, or a custom checker if desired. This
// constructor prevents double-registration of checkers to prevent panics, so
// listing the same checker more than once is not harmful. Default checkers,
// such as those for `allow`, `time-before`, `declared`, and `error` caveats
// are registered automatically and don't need to be added.
func NewService(dir string, checks ...Checker) (*Service, error) {
	// Ensure that the path to the directory exists.
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}

	// Open the database that we'll use to store the primary macaroon key,
	// and all generated macaroons+caveats.
	macaroonDB, err := bbolt.Open(
		path.Join(dir, DBFilename), 0600, bbolt.DefaultOptions,
	)
	if err != nil {
		return nil, err
	}

	rootKeyStore, err := NewRootKeyStorage(macaroonDB)
	if err != nil {
		return nil, err
	}

	macaroonParams := bakery.BakeryParams{
		Location:     "lnd",
		RootKeyStore: rootKeyStore,
		// No third-party caveat support for now.
		// TODO(aakselrod): Add third-party caveat support.
		Locator: nil,
		Key:     nil,
	}

	svc := bakery.New(macaroonParams)

	// Register all custom caveat checkers with the bakery's checker.
	// TODO(aakselrod): Add more checks as required.
	checker := svc.Checker.FirstPartyCaveatChecker.(*checkers.Checker)
	for _, check := range checks {
		cond, fun := check()
		if !isRegistered(checker, cond) {
			checker.Register(cond, "std", fun)
		}
	}

	return &Service{*svc, rootKeyStore}, nil
}

// isRegistered checks to see if the required checker has already been
// registered in order to avoid a panic caused by double registration.
func isRegistered(c *checkers.Checker, name string) bool {
	if c == nil {
		return false
	}

	for _, info := range c.Info() {
		if info.Name == name &&
			info.Prefix == "" &&
			info.Namespace == "std" {
			return true
		}
	}

	return false
}

// requestHash builds a SHA256 hash out of a request object (if available)
// and the request URI.
func requestHash(req interface{}, uri string) string {
	strToHash := ""
	reqJSON, err := json.Marshal(req)
	if err == nil {
		strToHash = string(reqJSON)
	}
	sum256 := sha256.Sum256([]byte(uri + strToHash))
	return hex.EncodeToString(sum256[:])
}

// UnaryServerInterceptor is a gRPC interceptor for synchronous incoming
// (server side) requests that checks whether the request is authorized by the
// included macaroons. It also calculates the SHA256 hash of the incoming
// request object and stores it in the context for further macaroon caveat
// checkers to verify.
func (svc *Service) UnaryServerInterceptor(
	permissionMap map[string][]bakery.Op) grpc.UnaryServerInterceptor {

	return func(ctx context.Context, req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		// Check that there is a permission defined for the full method
		// of the incoming request. Then validate every caveat of the
		// macaroon.
		if _, ok := permissionMap[info.FullMethod]; !ok {
			return nil, fmt.Errorf("%s: unknown permissions "+
				"required for method", info.FullMethod)
		}
		err := svc.ValidateMacaroon(
			ctx, req, info.FullMethod,
			permissionMap[info.FullMethod],
		)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// UnaryClientInterceptor is a gRPC interceptor for synchronous outgoing
// (client side) requests that calculates the SHA256 sum of the request
// object and adds that as a first-party caveat to the macaroon for
// better replay protection.
func UnaryClientInterceptor(
	mac *macaroon.Macaroon) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{},
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption) error {

		// Add a new constraint to the macaroon that checks the hash of
		// the request object and method. Since the whole interceptor is
		// only added if the request hash check is enabled, we don't
		// need to check that here again.
		mac, err := AddConstraints(
			mac, RequestHashConstraint(requestHash(req, method)),
		)
		if err != nil {
			return fmt.Errorf("error adding constraint: %v", err)
		}
		cred := NewMacaroonCredential(mac)
		opts = append(opts, grpc.PerRPCCredentials(cred))
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// StreamServerInterceptor is a gRPC interceptor for streaming incoming
// (server side) requests that checks whether the request is authorized by the
// included macaroons. It also calculates the SHA256 hash of the incoming
// method name and stores it in the context for further macaroon caveat
// checkers to verify.
func (svc *Service) StreamServerInterceptor(
	permissionMap map[string][]bakery.Op) grpc.StreamServerInterceptor {

	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		// Check that there is a permission defined for the full method
		// of the incoming request. Then validate every caveat of the
		// macaroon.
		if _, ok := permissionMap[info.FullMethod]; !ok {
			return fmt.Errorf(
				"%s: unknown permissions required for method",
				info.FullMethod,
			)
		}
		err := svc.ValidateMacaroon(
			ss.Context(), nil, info.FullMethod,
			permissionMap[info.FullMethod],
		)
		if err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// StreamClientInterceptor is a gRPC interceptor for streaming outgoing
// (client side) requests that calculates the SHA256 sum of the method name
// and adds that as a first-party caveat to the macaroon for better replay
// protection.
func StreamClientInterceptor(
	mac *macaroon.Macaroon) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc,
		cc *grpc.ClientConn, method string, streamer grpc.Streamer,
		opts ...grpc.CallOption) (grpc.ClientStream, error) {

		// Add a new constraint to the macaroon that checks the hash of
		// the request object and method. Since the whole interceptor is
		// only added if the request hash check is enabled, we don't
		// need to check that here again.
		mac, err := AddConstraints(
			mac, RequestHashConstraint(requestHash(nil, method)),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"error adding constraint: %v", err,
			)
		}
		cred := NewMacaroonCredential(mac)
		opts = append(opts, grpc.PerRPCCredentials(cred))
		return streamer(ctx, desc, cc, method, opts...)
	}
}

// ValidateMacaroon validates the capabilities of a given request given a
// bakery service, context, request object uri and required permissions.
// Within the passed context.Context, we expect a macaroon to be encoded as
// request metadata using the key "macaroon".
func (svc *Service) ValidateMacaroon(ctx context.Context,
	req interface{}, uri string, requiredPermissions []bakery.Op) error {

	// Since this is the only place where we get the full request object
	// in a generic way, we need to calculate the hash of it here. We do
	// this, even if the macaroon might not have a condition set for it.
	// But the performance impact should be negligible.
	ctx = context.WithValue(ctx, KeyRequestHash, requestHash(req, uri))

	// Get macaroon bytes from context and unmarshal into macaroon.
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return fmt.Errorf("unable to get metadata from context")
	}
	if len(md["macaroon"]) != 1 {
		return fmt.Errorf("expected 1 macaroon, got %d",
			len(md["macaroon"]))
	}

	// With the macaroon obtained, we'll now decode the hex-string
	// encoding, then unmarshal it from binary into its concrete struct
	// representation.
	macBytes, err := hex.DecodeString(md["macaroon"][0])
	if err != nil {
		return err
	}
	mac := &macaroon.Macaroon{}
	err = mac.UnmarshalBinary(macBytes)
	if err != nil {
		return err
	}

	// Check the method being called against the permitted operation and
	// the expiration time and IP address and return the result.
	authChecker := svc.Checker.Auth(macaroon.Slice{mac})
	_, err = authChecker.Allow(ctx, requiredPermissions...)
	return err
}

// Close closes the database that underlies the RootKeyStore and zeroes the
// encryption keys.
func (svc *Service) Close() error {
	return svc.rks.Close()
}

// CreateUnlock calls the underlying root key store's CreateUnlock and returns
// the result.
func (svc *Service) CreateUnlock(password *[]byte) error {
	return svc.rks.CreateUnlock(password)
}
