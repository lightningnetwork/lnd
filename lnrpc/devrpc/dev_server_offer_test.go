//go:build dev
// +build dev

package devrpc

import (
	"context"
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/offers"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeOfferHash is the offer hash that fakeOfferCreator returns.
var fakeOfferHash = [32]byte{1}

// fakeOfferCreator returns a fixed result and records the quantity limit.
type fakeOfferCreator struct {
	err         error
	quantityMax fn.Option[uint64]
}

// CreateOffer returns err, or a fixed offer when err is nil.
func (f *fakeOfferCreator) CreateOffer(_ context.Context, _ string,
	_, _ uint64,
	quantityMax fn.Option[uint64]) (*offers.CreateOfferResult, error) {

	f.quantityMax = quantityMax
	if f.err != nil {
		return nil, f.err
	}

	return &offers.CreateOfferResult{
		Encoded: "lno1test",
		Hash:    fakeOfferHash,
	}, nil
}

// TestCreateOffer checks the gRPC code of each CreateOffer result.
func TestCreateOffer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		creator *fakeOfferCreator
		req     *CreateOfferRequest
		code    codes.Code
	}{
		{
			name: "offers disabled",
			req:  &CreateOfferRequest{Description: "coffee"},
			code: codes.FailedPrecondition,
		},
		{
			name:    "amount without description",
			creator: &fakeOfferCreator{},
			req:     &CreateOfferRequest{AmountMsat: 1},
			code:    codes.InvalidArgument,
		},
		{
			name: "expiry in the past",
			creator: &fakeOfferCreator{
				err: offers.ErrExpiryNotInFuture,
			},
			req:  &CreateOfferRequest{Description: "coffee"},
			code: codes.InvalidArgument,
		},
		{
			name: "offer exists",
			creator: &fakeOfferCreator{
				err: offers.ErrOfferExists,
			},
			req:  &CreateOfferRequest{Description: "coffee"},
			code: codes.AlreadyExists,
		},
		{
			name: "store failure",
			creator: &fakeOfferCreator{
				err: errors.New("disk full"),
			},
			req:  &CreateOfferRequest{Description: "coffee"},
			code: codes.Unknown,
		},
		{
			name:    "success",
			creator: &fakeOfferCreator{},
			req:     &CreateOfferRequest{Description: "coffee"},
			code:    codes.OK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{}
			if tc.creator != nil {
				cfg.OfferCreator = tc.creator
			}
			server, _, err := New(cfg)
			require.NoError(t, err)

			resp, err := server.CreateOffer(t.Context(), tc.req)
			require.Equal(t, tc.code, status.Code(err))
			if tc.code != codes.OK {
				return
			}

			require.Equal(t, "lno1test", resp.Offer)
			require.Equal(t, fakeOfferHash[:], resp.OfferHash)
		})
	}
}

// TestCreateOfferQuantityMax checks that an unset quantity_max reaches the
// creator as None, and a set one as Some.
func TestCreateOfferQuantityMax(t *testing.T) {
	t.Parallel()

	creator := &fakeOfferCreator{}
	server, _, err := New(&Config{OfferCreator: creator})
	require.NoError(t, err)

	_, err = server.CreateOffer(t.Context(), &CreateOfferRequest{})
	require.NoError(t, err)
	require.True(t, creator.quantityMax.IsNone())

	qty := uint64(1)
	_, err = server.CreateOffer(t.Context(), &CreateOfferRequest{
		QuantityMax: &qty,
	})
	require.NoError(t, err)
	require.Equal(t, fn.Some(qty), creator.quantityMax)
}
