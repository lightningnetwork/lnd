package macaroons

import (
	"fmt"
	"path"

	"google.golang.org/grpc"

	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"

	"golang.org/x/net/context"

	"github.com/boltdb/bolt"
)

var (
	// dbFileName is the filename within the data directory which contains
	// the macaroon stores.
	dbFilename = "macaroons.db"
)

// NewService returns a service backed by the macaroon Bolt DB stored in the
// passed directory. The `checks` argument can be any of the `Checker` type
// functions defined in this package, or a custom checker if desired. This
// constructor prevents double-registration of checkers to prevent panics, so
// listing the same checker more than once is not harmful. Default checkers,
// such as those for `allow`, `time-before`, `declared`, and `error` caveats
// are registered automatically and don't need to be added.
func NewService(dir string, checks ...Checker) (*bakery.Bakery, error) {
	// Open the database that we'll use to store the primary macaroon key,
	// and all generated macaroons+caveats.
	macaroonDB, err := bolt.Open(path.Join(dir, dbFilename), 0600,
		bolt.DefaultOptions)
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

	return svc, nil
}

// isRegistered checks to see if the required checker has already been
// registered in order to avoid a panic caused by double registration.
func isRegistered(c *checkers.Checker, name string) bool {
	if c == nil {
		return false
	}

	for _, info := range c.Info() {
		if info.Name == name && info.Prefix == "std" {
			return true
		}
	}

	return false
}

// UnaryServerInterceptor is a GRPC interceptor that checks whether the
// request is authorized by the included macaroons.
func UnaryServerInterceptor(svc *bakery.Bakery,
	permissionMap map[string][]bakery.Op) grpc.UnaryServerInterceptor {

	return func(ctx context.Context, req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		if _, ok := permissionMap[info.FullMethod]; !ok {
			return nil, fmt.Errorf("%s: unknown permissions "+
				"required for method", info.FullMethod)
		}

		err := ValidateMacaroon(ctx, permissionMap[info.FullMethod],
			svc)
		if err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// StreamServerInterceptor is a GRPC interceptor that checks whether the
// request is authorized by the included macaroons.
func StreamServerInterceptor(svc *bakery.Bakery,
	permissionMap map[string][]bakery.Op) grpc.StreamServerInterceptor {

	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		if _, ok := permissionMap[info.FullMethod]; !ok {
			return fmt.Errorf("%s: unknown permissions required "+
				"for method", info.FullMethod)
		}

		err := ValidateMacaroon(ss.Context(),
			permissionMap[info.FullMethod], svc)
		if err != nil {
			return err
		}

		return handler(srv, ss)
	}
}
