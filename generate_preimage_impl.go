package lnd

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

// GeneratePreimage generates a new preimage and returns it along with its hash.
// If a preimage is provided in the request, it is validated and its hash is
// returned.
func (r *rpcServer) GeneratePreimage(ctx context.Context,
	req *lnrpc.GeneratePreimageRequest) (*lnrpc.GeneratePreimageResponse, error) {

	var preimage lntypes.Preimage
	var err error

	// If the user provided a preimage to use, we'll parse it to ensure it's
	// valid.
	if len(req.Preimage) > 0 {
		preimage, err = lntypes.MakePreimage(req.Preimage)
		if err != nil {
			return nil, err
		}
	} else {
		// Otherwise, we'll generate a random preimage.
		if _, err := io.ReadFull(rand.Reader, preimage[:]); err != nil {
			return nil, fmt.Errorf("unable to generate preimage: %w",
				err)
		}
	}

	// With the preimage obtained, we can now compute the hash and return
	// both to the caller.
	hash := preimage.Hash()

	return &lnrpc.GeneratePreimageResponse{
		Preimage: preimage[:],
		Hash:     hash[:],
	}, nil
}
