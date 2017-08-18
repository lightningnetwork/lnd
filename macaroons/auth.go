package macaroons

import (
	"encoding/hex"
	"fmt"

	"golang.org/x/net/context"
	"google.golang.org/grpc/metadata"

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

// GetRequestMetadata implements the PerRPCCredentials interface.
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

// ValidateMacaroon validates auth given a bakery service, context, and uri.
func ValidateMacaroon(ctx context.Context, method string,
	svc *bakery.Service) error {
	// Get macaroon bytes from context and unmarshal into macaroon.
	// TODO(aakselrod): use FromIncomingContext after grpc update in glide.
	md, ok := metadata.FromContext(ctx)
	if !ok {
		return fmt.Errorf("unable to get metadata from context")
	}
	if len(md["macaroon"]) != 1 {
		return fmt.Errorf("expected 1 macaroon, got %d",
			len(md["macaroon"]))
	}
	macBytes, err := hex.DecodeString(md["macaroon"][0])
	if err != nil {
		return err
	}
	mac := &macaroon.Macaroon{}
	err = mac.UnmarshalBinary(macBytes)
	if err != nil {
		return err
	}

	// Check the method being called against the permitted operation and the
	// expiration time and return the result.
	// TODO(aakselrod): Add more checks as required.
	return svc.Check(macaroon.Slice{mac}, checkers.New(
		checkers.OperationChecker(method),
		checkers.TimeBefore,
	))
}
