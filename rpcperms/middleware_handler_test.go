package rpcperms

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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

// TestParseErrorReplacement tests that parseErrorReplacement correctly parses
// both plain error strings and rich gRPC status errors.
func TestParseErrorReplacement(t *testing.T) {
	testCases := []struct {
		name           string
		typeName       string
		serialized     []byte
		expectedErrMsg string
		expectParseErr bool
	}{{
		name:           "plain error string",
		typeName:       StatusTypeNameError,
		serialized:     []byte("this is a plain error"),
		expectedErrMsg: "this is a plain error",
	}, {
		name:           "empty error string",
		typeName:       StatusTypeNameError,
		serialized:     []byte(""),
		expectedErrMsg: "",
	}, {
		name:           "invalid status proto",
		typeName:       StatusTypeNameStatus,
		serialized:     []byte("not a valid proto"),
		expectParseErr: true,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(tt *testing.T) {
			resultErr, parseErr := parseErrorReplacement(
				tc.typeName, tc.serialized,
			)

			if tc.expectParseErr {
				require.Error(tt, parseErr)
				return
			}

			require.NoError(tt, parseErr)
			require.Equal(tt, tc.expectedErrMsg, resultErr.Error())
		})
	}
}

// TestParseErrorReplacementWithStatus tests that parseErrorReplacement
// correctly handles gRPC status errors with proper error codes.
func TestParseErrorReplacementWithStatus(t *testing.T) {
	// Create a gRPC status error with a specific code and message.
	st := status.New(codes.NotFound, "resource not found")
	statusProto := st.Proto()

	// Serialize the status proto.
	serialized, err := proto.Marshal(statusProto)
	require.NoError(t, err)

	// Parse it back.
	resultErr, parseErr := parseErrorReplacement(
		StatusTypeNameStatus, serialized,
	)
	require.NoError(t, parseErr)
	require.Error(t, resultErr)

	// Verify we can extract the status back.
	resultStatus, ok := status.FromError(resultErr)
	require.True(t, ok)
	require.Equal(t, codes.NotFound, resultStatus.Code())
	require.Equal(t, "resource not found", resultStatus.Message())
}

// TestNewMessageInterceptionRequestWithStatusError tests that
// NewMessageInterceptionRequest correctly serializes gRPC status errors
// as google.rpc.Status protos instead of plain error strings.
func TestNewMessageInterceptionRequestWithStatusError(t *testing.T) {
	testCases := []struct {
		name             string
		err              error
		expectedTypeName string
		isStatusError    bool
	}{{
		name:             "plain error",
		err:              errors.New("this is a plain error"),
		expectedTypeName: StatusTypeNameError,
		isStatusError:    false,
	}, {
		name:             "gRPC status error",
		err:              status.Error(codes.NotFound, "resource not found"),
		expectedTypeName: StatusTypeNameStatus,
		isStatusError:    true,
	}, {
		name:             "gRPC status error with different code",
		err:              status.Error(codes.PermissionDenied, "access denied"),
		expectedTypeName: StatusTypeNameStatus,
		isStatusError:    true,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(tt *testing.T) {
			ctx := context.Background()
			req, err := NewMessageInterceptionRequest(
				ctx, TypeResponse, false, "/test/Method",
				tc.err,
			)
			require.NoError(tt, err)
			require.True(tt, req.IsError)
			require.Equal(tt, tc.expectedTypeName, req.ProtoTypeName)

			if tc.isStatusError {
				// Verify we can parse the status back.
				resultErr, parseErr := parseErrorReplacement(
					req.ProtoTypeName, req.ProtoSerialized,
				)
				require.NoError(tt, parseErr)

				// Verify the error code is preserved.
				resultStatus, ok := status.FromError(resultErr)
				require.True(tt, ok)

				originalStatus, _ := status.FromError(tc.err)
				require.Equal(
					tt, originalStatus.Code(),
					resultStatus.Code(),
				)
				require.Equal(
					tt, originalStatus.Message(),
					resultStatus.Message(),
				)
			}
		})
	}
}
