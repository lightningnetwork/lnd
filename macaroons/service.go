package macaroons

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnwire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	"gopkg.in/macaroon.v2"
)

var (
	// DBFilename is the filename within the data directory which contains
	// the macaroon stores.
	DBFilename = "macaroons.db"
)

// Service encapsulates bakery.Bakery and adds a Close() method that zeroes the
// root key service encryption keys, as well as utility methods to validate a
// macaroon against the bakery and gRPC middleware for macaroon-based auth.
// Additionally, there is an account storage for accounting based macaroon
// balances and utility methods to manage accounts.
type Service struct {
	bakery.Bakery

	rks *RootKeyStorage

	as *AccountStorage
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

	accountStore, err := NewAccountStorage(macaroonDB)
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

	macBakery := bakery.New(macaroonParams)
	service := &Service{*macBakery, rootKeyStore, accountStore}

	// Register all custom caveat checkers with the bakery's checker.
	// TODO(aakselrod): Add more checks as required.
	checker := macBakery.Checker.FirstPartyCaveatChecker.(*checkers.Checker)
	for _, check := range checks {
		cond, fun := check(service)
		if !isRegistered(checker, cond) {
			checker.Register(cond, "std", fun)
		}
	}

	return service, nil
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

// macaroonFromContext reads the macaroon content from a context's metadata and
// unmarshals it.
func macaroonFromContext(ctx context.Context) (*macaroon.Macaroon, error) {
	// Get macaroon bytes from context and unmarshal into macaroon.
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, fmt.Errorf("unable to get metadata from context")
	}
	if len(md["macaroon"]) != 1 {
		return nil, fmt.Errorf(
			"expected 1 macaroon, got %d", len(md["macaroon"]),
		)
	}

	// With the macaroon obtained, we'll now decode the hex-string
	// encoding, then unmarshal it from binary into its concrete struct
	// representation.
	macBytes, err := hex.DecodeString(md["macaroon"][0])
	if err != nil {
		return nil, err
	}
	mac := &macaroon.Macaroon{}
	err = mac.UnmarshalBinary(macBytes)
	if err != nil {
		return nil, err
	}
	return mac, nil
}

// UnaryServerInterceptor is a GRPC interceptor that checks whether the
// request is authorized by the included macaroons.
func (svc *Service) UnaryServerInterceptor(
	permissionMap map[string][]bakery.Op) grpc.UnaryServerInterceptor {

	return func(ctx context.Context, req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		if _, ok := permissionMap[info.FullMethod]; !ok {
			return nil, fmt.Errorf("%s: unknown permissions "+
				"required for method", info.FullMethod)
		}

		err := svc.ValidateMacaroon(ctx, permissionMap[info.FullMethod])
		if err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// StreamServerInterceptor is a GRPC interceptor that checks whether the
// request is authorized by the included macaroons.
func (svc *Service) StreamServerInterceptor(
	permissionMap map[string][]bakery.Op) grpc.StreamServerInterceptor {

	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		if _, ok := permissionMap[info.FullMethod]; !ok {
			return fmt.Errorf("%s: unknown permissions required "+
				"for method", info.FullMethod)
		}

		err := svc.ValidateMacaroon(
			ss.Context(), permissionMap[info.FullMethod],
		)
		if err != nil {
			return err
		}

		return handler(srv, ss)
	}
}

// ValidateMacaroon validates the capabilities of a given request given a
// bakery service, context, and uri. Within the passed context.Context, we
// expect a macaroon to be encoded as request metadata using the key
// "macaroon".
func (svc *Service) ValidateMacaroon(ctx context.Context,
	requiredPermissions []bakery.Op) error {

	mac, err := macaroonFromContext(ctx)
	if err != nil {
		return err
	}

	// Check the method being called against the permitted operation and
	// the expiration time and IP address and return the result.
	authChecker := svc.Checker.Auth(macaroon.Slice{mac})
	_, err = authChecker.Allow(ctx, requiredPermissions...)
	return err
}

// ValidateMacaroonAccountBalance reads the macaroon from the context and checks
// if it is locked to an account. If it is locked, the account's balance is
// checked for sufficient balance.
func (svc *Service) ValidateMacaroonAccountBalance(ctx context.Context,
	requiredBalance lnwire.MilliSatoshi) error {

	// Get the macaroon from the context and see if it is locked to an
	// account.
	mac, err := macaroonFromContext(ctx)
	if err != nil {
		return err
	}
	macaroonAccount := GetCaveatArgOfCondition(mac, CondAccount)
	if len(macaroonAccount) == 0 {
		// There is no condition that locks the macaroon to an account,
		// so there is nothing to check.
		return nil
	}

	// The macaroon is indeed locked to an account. Fetch the account and
	// validate its balance.
	accountIDBytes, err := hex.DecodeString(macaroonAccount)
	if err != nil {
		return err
	}
	var accountID AccountID
	copy(accountID[:], accountIDBytes)
	return svc.CheckAccountBalance(accountID, requiredBalance)
}

// ChargeMacaroonAccountBalance reads the macaroon from the context and checks
// if it is locked to an account. If it is locked, the account is charged with
// the specified amount, reducing its balance.
func (svc *Service) ChargeMacaroonAccountBalance(ctx context.Context,
	amount lnwire.MilliSatoshi) error {

	// Get the macaroon from the context and see if it is locked to an
	// account.
	mac, err := macaroonFromContext(ctx)
	if err != nil {
		return err
	}
	macaroonAccount := GetCaveatArgOfCondition(mac, CondAccount)
	if len(macaroonAccount) == 0 {
		// There is no condition that locks the macaroon to an account,
		// so there is nothing to check.
		return nil
	}

	// The macaroon is indeed locked to an account. Fetch the account and
	// validate its balance.
	accountIDBytes, err := hex.DecodeString(macaroonAccount)
	if err != nil {
		return err
	}
	var accountID AccountID
	copy(accountID[:], accountIDBytes)
	return svc.ChargeAccount(accountID, amount)
}

// Close closes the database that underlies the RootKeyStore and AccountStore
// and zeroes the encryption keys.
func (svc *Service) Close() error {
	// We need to make sure both stores/DBs are closed. If both return an
	// error, it doesn't really matter which one we return.
	err := svc.rks.Close()
	err2 := svc.as.Close()
	if err != nil {
		return err
	}
	if err2 != nil {
		return err
	}
	return nil
}

// CreateUnlock calls the underlying root key store's CreateUnlock and returns
// the result.
func (svc *Service) CreateUnlock(password *[]byte) error {
	return svc.rks.CreateUnlock(password)
}

// NewAccount calls the underlying account store's NewAccount and returns the
// result.
func (svc *Service) NewAccount(balance lnwire.MilliSatoshi,
	expirationDate time.Time) (*OffChainBalanceAccount, error) {

	return svc.as.NewAccount(balance, expirationDate)
}

// GetAccount calls the underlying account store's GetAccount and returns the
// result.
func (svc *Service) GetAccount(id AccountID) (*OffChainBalanceAccount, error) {
	return svc.as.GetAccount(id)
}

// GetAccounts calls the underlying account store's GetAccounts and returns the
// result.
func (svc *Service) GetAccounts() ([]*OffChainBalanceAccount, error) {
	return svc.as.GetAccounts()
}

// RemoveAccount calls the underlying account store's RemoveAccount and returns
// the result.
func (svc *Service) RemoveAccount(id AccountID) error {
	return svc.as.RemoveAccount(id)
}

// CheckAccountBalance calls the underlying account store's CheckAccountBalance
// and returns the result.
func (svc *Service) CheckAccountBalance(id AccountID,
	requiredBalance lnwire.MilliSatoshi) error {

	return svc.as.CheckAccountBalance(id, requiredBalance)
}

// ChargeAccount calls the underlying account store's ChargeAccount and returns
// the result.
func (svc *Service) ChargeAccount(id AccountID,
	amount lnwire.MilliSatoshi) error {

	return svc.as.ChargeAccount(id, amount)
}
