package funding

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet"

	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestPublisherErrors ensures that the wrapped errors returned from the
// Publisher can correctly be matched on.
func TestPublisherErrors(t *testing.T) {
	errSanity := fmt.Errorf("sanity")
	errStandardness := fmt.Errorf("standardness")
	errPublish := fmt.Errorf("publish")

	tests := []struct {
		cfg                *PublisherCfg
		expectedPublishErr error
		expectedBackendErr error
	}{
		{
			cfg: &PublisherCfg{
				TxSanityCheck: func(tx *wire.MsgTx) error {
					return errSanity
				},
			},
			expectedPublishErr: &ErrSanity{},
			expectedBackendErr: errSanity,
		},
		{
			cfg: &PublisherCfg{
				TxStandardnessCheck: func(tx *wire.MsgTx) error {
					return errStandardness
				},
			},
			expectedPublishErr: &ErrStandardness{},
			expectedBackendErr: errStandardness,
		},
		{
			cfg: &PublisherCfg{
				TestMempoolAccept: func(tx *wire.MsgTx) error {
					return lnwallet.ErrDoubleSpend
				},
			},
			expectedPublishErr: &ErrMempoolTestAccept{},
			expectedBackendErr: lnwallet.ErrDoubleSpend,
		},
		{
			cfg: &PublisherCfg{
				Publish: func(tx *wire.MsgTx,
					label string) error {

					return errPublish
				},
			},
			expectedPublishErr: &ErrPublish{},
			expectedBackendErr: errPublish,
		},
	}

	for i, test := range tests {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			p := NewPublisher(test.cfg)
			err := p.CheckAndPublish(nil, "")

			var (
				e  error
				ok bool
			)
			switch test.expectedPublishErr.(type) {
			case *ErrSanity:
				e, ok = err.(*ErrSanity)
			case *ErrStandardness:
				e, ok = err.(*ErrStandardness)
			case *ErrMempoolTestAccept:
				e, ok = err.(*ErrMempoolTestAccept)
			case *ErrPublish:
				e, ok = err.(*ErrPublish)
			}

			if !ok {
				t.Fatalf("incorrect publish error")
			}

			require.True(
				t, errors.Is(
					e, test.expectedBackendErr,
				),
			)
		})
	}
}
