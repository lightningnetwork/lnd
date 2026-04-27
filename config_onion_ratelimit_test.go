package lnd

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestValidateOnionMsgLimiter exercises every branch of
// validateOnionMsgLimiter: the happy-path cases (both zero, both positive
// with adequate burst) and every rejection branch (mismatched pair and
// undersized burst). Startup config validation is the first line of
// defense against a typo silently disabling the limiter, so every branch
// is exercised explicitly.
func TestValidateOnionMsgLimiter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		kbps       uint64
		burstBytes uint64
		wantErr    string
	}{
		{
			name:       "both zero disables",
			kbps:       0,
			burstBytes: 0,
		},
		{
			name:       "both positive enables",
			kbps:       512,
			burstBytes: 8 * 32 * 1024,
		},
		{
			name:       "large values pass",
			kbps:       1_000_000,
			burstBytes: 1_000_000,
		},
		{
			name:       "burst exactly at min allowed",
			kbps:       1,
			burstBytes: 2 + lnwire.MaxMsgBody,
		},
		{
			name: "burst one below min max-msg wire size " +
				"rejected",
			kbps:       1,
			burstBytes: 1 + lnwire.MaxMsgBody,
			wantErr:    "must be at least 65535",
		},
		{
			name:       "positive kbps zero burst rejected",
			kbps:       512,
			burstBytes: 0,
			wantErr: "kbps and burst-bytes must both be " +
				"positive or both be zero",
		},
		{
			name:       "zero kbps positive burst rejected",
			kbps:       0,
			burstBytes: 65_536,
			wantErr: "kbps and burst-bytes must both be " +
				"positive or both be zero",
		},
		{
			name: "burst below maxOnionMsgWireSize " +
				"rejected",
			kbps:       512,
			burstBytes: 1024,
			wantErr: "burst-bytes=1024 must be at least " +
				"65535",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateOnionMsgLimiter(
				"test", tc.kbps, tc.burstBytes,
			)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
