package macaroons

import (
	"encoding/hex"
	"fmt"

	"google.golang.org/grpc/metadata"

	"golang.org/x/net/context"

	"gopkg.in/macaroon-bakery.v2/bakery"
	macaroon "gopkg.in/macaroon.v2"
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

// ValidateMacaroon validates the capabilities of a given request given a
// bakery service, context, and uri. Within the passed context.Context, we
// expect a macaroon to be encoded as request metadata using the key
// "macaroon".
func ValidateMacaroon(ctx context.Context, requiredPermissions []bakery.Op,
	svc *bakery.Bakery) error {

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
