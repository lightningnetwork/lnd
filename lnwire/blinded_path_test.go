package lnwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// validPubkeyIntro returns an on-curve PubkeyIntro plus the matching
// *btcec.PublicKey for assertions.
func validPubkeyIntro(t *testing.T) (PubkeyIntro, *btcec.PublicKey) {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey()

	return PubkeyIntro{Pubkey: pub}, pub
}

// validBlindingPoint returns an on-curve pubkey suitable for use as a
// BlindingPoint or BlindedNodeID in tests.
func validBlindingPoint(t *testing.T) *btcec.PublicKey {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey()
}

// oversizeEncDataPaths returns a BlindedPaths with a single hop whose
// EncryptedData is one byte over the wire-format limit, used by the
// encode-rejects test.
func oversizeEncDataPaths(t *testing.T, intro IntroductionNode) *BlindedPaths {
	t.Helper()

	return &BlindedPaths{
		Paths: []BlindedPath{{
			IntroductionNode: intro,
			BlindingPoint:    validBlindingPoint(t),
			Hops: []BlindedHop{{
				BlindedNodeID: validBlindingPoint(t),
				EncryptedData: make(
					[]byte, maxEncryptedDataLen+1,
				),
			}},
		}},
	}
}

// TestBlindedPathRoundTrip pins encode→decode parity across both
// IntroductionNode variants and across single- and multi-path framings, so
// concrete variant types survive the round-trip with byte-identical output.
func TestBlindedPathRoundTrip(t *testing.T) {
	t.Parallel()

	pubkeyIntro, _ := validPubkeyIntro(t)
	sciddirIntro := SciddirIntro{
		Direction: 0x01,
		SCID: [8]byte{
			0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		},
	}

	hop := func(payload byte) BlindedHop {
		return BlindedHop{
			BlindedNodeID: validBlindingPoint(t),
			EncryptedData: []byte{payload, payload ^ 0xff},
		}
	}

	pubkeyPath := BlindedPath{
		IntroductionNode: pubkeyIntro,
		BlindingPoint:    validBlindingPoint(t),
		Hops: []BlindedHop{
			hop(0xde),
			hop(0xad),
		},
	}
	sciddirPath := BlindedPath{
		IntroductionNode: sciddirIntro,
		BlindingPoint:    validBlindingPoint(t),
		Hops:             []BlindedHop{hop(0xbe)},
	}

	tests := []struct {
		name  string
		paths []BlindedPath
	}{
		{
			name:  "single pubkey path",
			paths: []BlindedPath{pubkeyPath},
		},
		{
			name:  "single sciddir path",
			paths: []BlindedPath{sciddirPath},
		},
		{
			name:  "mixed multi-path",
			paths: []BlindedPath{pubkeyPath, sciddirPath},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bp := &BlindedPaths{Paths: tc.paths}

			var buf bytes.Buffer
			require.NoError(t, encodeBlindedPaths(
				&buf, bp, new([8]byte),
			))

			var decoded BlindedPaths
			err := decodeBlindedPaths(
				bytes.NewReader(buf.Bytes()), &decoded,
				new([8]byte), uint64(buf.Len()),
			)
			require.NoError(t, err)
			require.Equal(t, bp.Paths, decoded.Paths)

			// Single-path framing must round-trip too: the
			// reply_path TLV carries one BlindedPath, not a list.
			if len(tc.paths) == 1 {
				var single bytes.Buffer
				require.NoError(t, encodeBlindedPath(
					&single, &tc.paths[0], new([8]byte),
				))

				var decodedSingle BlindedPath
				err := decodeBlindedPath(
					bytes.NewReader(single.Bytes()),
					&decodedSingle, new([8]byte),
					uint64(single.Len()),
				)
				require.NoError(t, err)
				require.Equal(
					t, tc.paths[0], decodedSingle,
				)
			}
		})
	}
}

// TestDecodeBlindedPathsRejects covers every malformed-input branch the
// decoder must refuse: bad discriminators, allocation bombs, and short reads.
// The catch-all is that the decoder never allocates more memory than the
// remaining wire bytes can justify.
func TestDecodeBlindedPathsRejects(t *testing.T) {
	t.Parallel()

	// validKey is a 33-byte compressed SEC1 pubkey that the on-curve
	// decoder accepts; reused as both intro pubkey and blinding point so
	// the tests can exercise post-pubkey decode branches.
	validKey := validBlindingPoint(t).SerializeCompressed()

	// hopAllocOverflow declares num_hops=255 with no hop payload. Without
	// the remaining-bytes guard the decoder would make([]BlindedHop, 255)
	// before io.ReadFull notices the bytes are absent.
	hopAllocOverflow := func() []byte {
		out := make([]byte, 0, 67)
		out = append(out, validKey...)
		out = append(out, validKey...)
		out = append(out, 0xff)

		return out
	}

	// enclenOverflow declares enclen=65535 on a hop with no payload. The
	// guard against lr.N must reject before make([]byte, 65535).
	enclenOverflow := func() []byte {
		out := make([]byte, 0, 70)
		out = append(out, validKey...)
		out = append(out, validKey...)
		out = append(out, 0x01)
		out = append(out, validKey...)
		out = append(out, 0xff, 0xff)

		return out
	}

	// shortIntroPubkey truncates after the discriminator + 5 of 33 bytes
	// of intro pubkey, exercising io.ReadFull's short-read error.
	shortIntroPubkey := func() []byte {
		return append([]byte{0x02}, bytes.Repeat([]byte{0x00}, 5)...)
	}

	// shortBlindingPoint truncates after a full intro pubkey plus 5 of the
	// 33 blinding-point bytes, exercising io.ReadFull's short-read path
	// past the discriminator.
	shortBlindingPoint := func() []byte {
		out := make([]byte, 0, pubKeyLen+5)
		out = append(out, validKey...)
		out = append(out, bytes.Repeat([]byte{0x00}, 5)...)

		return out
	}

	tests := []struct {
		name    string
		data    []byte
		wantErr error
		wantMsg []string
	}{
		{
			name:    "invalid discriminator 0x04",
			data:    []byte{0x04},
			wantErr: ErrInvalidIntroNode,
		},
		{
			name:    "invalid discriminator 0x05",
			data:    []byte{0x05},
			wantErr: ErrInvalidIntroNode,
		},
		{
			name:    "invalid discriminator 0xff",
			data:    []byte{0xff},
			wantErr: ErrInvalidIntroNode,
		},
		{
			name:    "hop alloc overflow",
			data:    hopAllocOverflow(),
			wantMsg: []string{"num_hops", "exceeds remaining"},
		},
		{
			name:    "enclen alloc overflow",
			data:    enclenOverflow(),
			wantMsg: []string{"enclen", "exceeds remaining"},
		},
		{
			name:    "short intro pubkey",
			data:    shortIntroPubkey(),
			wantMsg: []string{"read intro pubkey"},
		},
		{
			name:    "short blinding point",
			data:    shortBlindingPoint(),
			wantMsg: []string{"read blinding point"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var bp BlindedPaths
			err := decodeBlindedPaths(
				bytes.NewReader(tc.data), &bp, new([8]byte),
				uint64(len(tc.data)),
			)
			require.Error(t, err)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			}
			for _, msg := range tc.wantMsg {
				require.Contains(t, err.Error(), msg)
			}
		})
	}
}

// TestEncodeBlindedPathsRejects pins the encoder's fail-closed guards. Any
// case here must not emit bytes — invalid input cannot be retracted from the
// wire once flushed.
func TestEncodeBlindedPathsRejects(t *testing.T) {
	t.Parallel()

	validIntro, _ := validPubkeyIntro(t)
	validHop := BlindedHop{BlindedNodeID: validBlindingPoint(t)}

	tests := []struct {
		name        string
		paths       *BlindedPaths
		wantErr     error
		wantMsg     []string
		wantNoWrite bool
	}{
		{
			name: "nil intro",
			paths: &BlindedPaths{
				Paths: []BlindedPath{{
					BlindingPoint: validBlindingPoint(t),
					Hops:          []BlindedHop{validHop},
				}},
			},
			wantMsg:     []string{"nil intro node"},
			wantNoWrite: true,
		},
		{
			name: "nil pubkey in PubkeyIntro",
			paths: &BlindedPaths{
				Paths: []BlindedPath{{
					IntroductionNode: PubkeyIntro{},
					BlindingPoint:    validBlindingPoint(t),
					Hops: []BlindedHop{
						validHop,
					},
				}},
			},
			wantErr:     ErrInvalidIntroNode,
			wantNoWrite: true,
		},
		{
			name: "invalid sciddir direction 0x02",
			paths: &BlindedPaths{
				Paths: []BlindedPath{{
					IntroductionNode: SciddirIntro{
						Direction: 0x02,
					},
					BlindingPoint: validBlindingPoint(t),
					Hops:          []BlindedHop{validHop},
				}},
			},
			wantErr:     ErrInvalidIntroNode,
			wantNoWrite: true,
		},
		{
			name: "invalid sciddir direction 0xff",
			paths: &BlindedPaths{
				Paths: []BlindedPath{{
					IntroductionNode: SciddirIntro{
						Direction: 0xff,
					},
					BlindingPoint: validBlindingPoint(t),
					Hops:          []BlindedHop{validHop},
				}},
			},
			wantErr:     ErrInvalidIntroNode,
			wantNoWrite: true,
		},
		{
			name: "nil blinding point",
			paths: &BlindedPaths{
				Paths: []BlindedPath{{
					IntroductionNode: validIntro,
					Hops: []BlindedHop{
						validHop,
					},
				}},
			},
			wantMsg:     []string{"nil blinding point"},
			wantNoWrite: true,
		},
		{
			name: "zero hops",
			paths: &BlindedPaths{
				Paths: []BlindedPath{{
					IntroductionNode: validIntro,
					BlindingPoint:    validBlindingPoint(t),
					Hops:             nil,
				}},
			},
			wantErr:     ErrEmptyBlindedPath,
			wantNoWrite: true,
		},
		{
			name: "hop overflow",
			paths: &BlindedPaths{
				Paths: []BlindedPath{{
					IntroductionNode: validIntro,
					BlindingPoint:    validBlindingPoint(t),
					Hops: func() []BlindedHop {
						hops := make([]BlindedHop,
							maxBlindedPathHops+1)
						pub := validBlindingPoint(t)
						for i := range hops {
							// Write to hop.
							h := &hops[i]
							h.BlindedNodeID = pub
						}

						return hops
					}(),
				}},
			},
			wantMsg:     []string{"exceeds limit"},
			wantNoWrite: true,
		},
		{
			name:    "oversize encrypted data",
			paths:   oversizeEncDataPaths(t, validIntro),
			wantMsg: []string{"exceeds limit"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			err := encodeBlindedPaths(
				&buf, tc.paths, new([8]byte),
			)
			require.Error(t, err)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			}
			for _, msg := range tc.wantMsg {
				require.Contains(t, err.Error(), msg)
			}
			if tc.wantNoWrite {
				require.Equal(t, 0, buf.Len(),
					"encoder wrote bytes on fail-closed "+
						"path")
			}
		})
	}
}

// TestBlindedPathSphinxRoundTrip checks that NewBlindedPathFromSphinx inverts
// ToSphinx for a path with a pubkey-form introduction node.
func TestBlindedPathSphinxRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		path := RandBlindedPath(rt)
		intro, err := NewPubkeyIntro(RandPubKey(rt))
		require.NoError(rt, err)
		path.IntroductionNode = intro

		sphinxPath, err := path.ToSphinx()
		require.NoError(rt, err)

		got, err := NewBlindedPathFromSphinx(sphinxPath)
		require.NoError(rt, err)
		require.Equal(rt, path, got)
	})
}

// TestToSphinxRejects checks that ToSphinx rejects a path that the routing
// layer cannot use.
func TestToSphinxRejects(t *testing.T) {
	t.Parallel()

	pubIntro, _ := validPubkeyIntro(t)
	sciddirIntro, err := NewSciddirIntro(
		0x00, [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
	)
	require.NoError(t, err)

	hop := BlindedHop{
		BlindedNodeID: validBlindingPoint(t),
		EncryptedData: []byte{0xaa, 0xbb},
	}

	tests := []struct {
		name  string
		intro IntroductionNode
		point *btcec.PublicKey
		hops  []BlindedHop
		err   string
	}{
		{
			name:  "sciddir intro",
			intro: sciddirIntro,
			point: validBlindingPoint(t),
			hops:  []BlindedHop{hop},
			err:   "not supported",
		},
		{
			name:  "nil intro",
			point: validBlindingPoint(t),
			hops:  []BlindedHop{hop},
			err:   "nil intro node",
		},
		{
			name:  "nil intro pubkey",
			intro: PubkeyIntro{},
			point: validBlindingPoint(t),
			hops:  []BlindedHop{hop},
			err:   "nil pubkey",
		},
		{
			name:  "nil blinding point",
			intro: pubIntro,
			hops:  []BlindedHop{hop},
			err:   "nil blinding point",
		},
		{
			name:  "nil blinded hop",
			intro: pubIntro,
			point: validBlindingPoint(t),
			hops:  []BlindedHop{hop, {}},
			err:   "nil blinded hop 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := &BlindedPath{
				IntroductionNode: tc.intro,
				BlindingPoint:    tc.point,
				Hops:             tc.hops,
			}

			_, err := path.ToSphinx()
			require.ErrorContains(t, err, tc.err)
		})
	}
}
