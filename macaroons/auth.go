package macaroons

import (
	"encoding/hex"
	"fmt"
	"net"

	"golang.org/x/net/context"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"gopkg.in/macaroon-bakery.v1/bakery"
	"gopkg.in/macaroon-bakery.v1/bakery/checkers"
	macaroon "gopkg.in/macaroon.v1"
)

// MacaroonCredential wraps a macaroon to implement the
// credentials.PerRPCCredentials interface.
type MacaroonCredential struct {
	*macaroon.Macaroon
}

// RequireTransportSecurity implements the PerRPCCredentials interface.
func (m MacaroonCredential) RequireTransportSecurity() bool {
	return true
}

// GetRequestMetadata implements the PerRPCCredentials interface. This method
// is required in order to pass the wrapped macaroon into the gRPC context.
// With this, the macaroon will be available within the request handling scope
// of the ultimate gRPC server implementation.
func (m MacaroonCredential) GetRequestMetadata(ctx context.Context,
	uri ...string) (map[string]string, error) {

	macBytes, err := m.MarshalBinary()
	if err != nil {
		return nil, err
	}

	md := make(map[string]string)
	md["macaroon"] = hex.EncodeToString(macBytes)
	return md, nil
}

// NewMacaroonCredential returns a copy of the passed macaroon wrapped in a
// MacaroonCredential struct which implements PerRPCCredentials.
func NewMacaroonCredential(m *macaroon.Macaroon) MacaroonCredential {
	ms := MacaroonCredential{}
	ms.Macaroon = m.Clone()
	return ms
}

// ValidateMacaroon constructs checkers needed for request authentication and
// delegates validation to ValidateMacaroonWithCheckers.
func ValidateMacaroon(ctx context.Context, method string,
	svc *bakery.Service) (context.Context, error) {

	// Get peer info and extract IP address from it for macaroon check
	pr, ok := peer.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("unable to get peer info from context")
	}
	peerAddr, _, err := net.SplitHostPort(pr.Addr.String())
	if err != nil {
		return nil, fmt.Errorf("unable to parse peer address")
	}

	// Add checker context value to the context (no pun intended).
	// Currently only AllowConstraint and IPLockConsraint require one.
	for key, value := range map[interface{}]interface{}{
		AllowContextKey:  method,
		IPLockContextKey: peerAddr,
	} {
		ctx = context.WithValue(ctx, key, value)
	}

	// TODO(aakselrod): Add more checks as required.
	return ctx, ValidateMacaroonWithCheckers(ctx, svc, DefaultCheckers...)
}

// ValidateMacaroonWithCheckers validates the capabilities of a given request
// given a bakery service, context, and a list of checkers. Within the passed
// context.Context, we expect a macaroon to be encoded as request metadata
// using the key "macaroon".
func ValidateMacaroonWithCheckers(ctx context.Context,
	svc *bakery.Service, checkerList ...Checker) error {
	// Get macaroon bytes from context and unmarshal into macaroon.
	md, ok := metadata.FromContext(ctx)
	if !ok {
		return fmt.Errorf("unable to get metadata from context")
	}

	macMD := md["macaroon"]
	if len(macMD) != 1 {
		return fmt.Errorf("expected 1 macaroon, got %d", len(macMD))
	}

	// With the macaroon obtained, we'll now decode the hex-string
	// encoding, then unmarshal it from binary into its concrete struct
	// representation.
	macBytes, err := hex.DecodeString(macMD[0])
	if err != nil {
		return err
	}
	mac := &macaroon.Macaroon{}
	err = mac.UnmarshalBinary(macBytes)
	if err != nil {
		return err
	}

	// Instantiate checkers with context.
	cInstances := []checkers.Checker{}
	for _, gen := range checkerList {
		cInstances = append(cInstances, gen(ctx))
	}

	return svc.Check(macaroon.Slice{mac}, checkers.New(cInstances...))
}
