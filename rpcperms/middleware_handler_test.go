package rpcperms

import (
	"encoding/json"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

// TestReplaceProtoMsg makes sure the proto message replacement works as
// expected.
func TestReplaceProtoMsg(t *testing.T) {
	testCases := []struct {
		name        string
		original    interface{}
		replacement interface{}
		expectedErr string
	}{{
		name: "simple content replacement",
		original: &lnrpc.Invoice{
			Memo:  "This is a memo string",
			Value: 123456,
		},
		replacement: &lnrpc.Invoice{
			Memo:  "This is the replaced string",
			Value: 654321,
		},
	}, {
		name: "replace with empty message",
		original: &lnrpc.Invoice{
			Memo:  "This is a memo string",
			Value: 123456,
		},
		replacement: &lnrpc.Invoice{},
	}, {
		name: "replace with fewer fields",
		original: &lnrpc.Invoice{
			Memo:  "This is a memo string",
			Value: 123456,
		},
		replacement: &lnrpc.Invoice{
			Value: 654321,
		},
	}, {
		name: "wrong replacement type",
		original: &lnrpc.Invoice{
			Memo:  "This is a memo string",
			Value: 123456,
		},
		replacement: &lnrpc.AddInvoiceResponse{},
		expectedErr: "replacement message is of wrong type",
	}, {
		name:     "wrong original type",
		original: &interceptRequest{},
		replacement: &lnrpc.Invoice{
			Memo:  "This is the replaced string",
			Value: 654321,
		},
		expectedErr: "target is not a proto message",
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(tt *testing.T) {
			err := replaceProtoMsg(tc.original, tc.replacement)

			if tc.expectedErr != "" {
				require.Error(tt, err)
				require.Contains(
					tt, err.Error(), tc.expectedErr,
				)

				return
			}

			require.NoError(tt, err)
			jsonEqual(tt, tc.replacement, tc.original)
		})
	}
}

func jsonEqual(t *testing.T, expected, actual interface{}) {
	expectedJSON, err := json.Marshal(expected)
	require.NoError(t, err)

	actualJSON, err := json.Marshal(actual)
	require.NoError(t, err)

	require.JSONEq(t, string(expectedJSON), string(actualJSON))
}
