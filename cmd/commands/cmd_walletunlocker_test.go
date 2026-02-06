package commands

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeClientStream implements grpc.ClientStream for tests.
type fakeClientStream struct{}

// Header returns empty metadata for the fake client stream.
func (fakeClientStream) Header() (metadata.MD, error) {
	return nil, nil
}

// Trailer returns empty metadata for the fake client stream.
func (fakeClientStream) Trailer() metadata.MD {
	return nil
}

// CloseSend is a no-op for the fake client stream.
func (fakeClientStream) CloseSend() error {
	return nil
}

// Context returns a background context for the fake client stream.
func (fakeClientStream) Context() context.Context {
	return context.Background()
}

// SendMsg is a no-op for the fake client stream.
func (fakeClientStream) SendMsg(interface{}) error {
	return nil
}

// RecvMsg is a no-op for the fake client stream.
func (fakeClientStream) RecvMsg(interface{}) error {
	return nil
}

// stateStreamSpec describes the scripted responses for a state stream.
type stateStreamSpec struct {
	states []lnrpc.WalletState
	err    error
}

// fakeStateStream implements State_SubscribeStateClient with scripted states.
type fakeStateStream struct {
	fakeClientStream
	states []lnrpc.WalletState
	err    error
	idx    int
}

// Recv returns the next scripted wallet state or the configured error.
func (f *fakeStateStream) Recv() (*lnrpc.SubscribeStateResponse, error) {
	if f.idx < len(f.states) {
		state := f.states[f.idx]
		f.idx++

		return &lnrpc.SubscribeStateResponse{
			State: state,
		}, nil
	}

	if f.err != nil {
		return nil, f.err
	}

	return nil, io.EOF
}

// fakeStateClient implements lnrpc.StateClient with scripted streams.
type fakeStateClient struct {
	streams         []stateStreamSpec
	subscribeCalls  int
	subscribeInputs []*lnrpc.SubscribeStateRequest
}

// SubscribeState returns a scripted stream for the fake state client.
func (f *fakeStateClient) SubscribeState(_ context.Context,
	in *lnrpc.SubscribeStateRequest,
	_ ...grpc.CallOption) (lnrpc.State_SubscribeStateClient, error) {

	f.subscribeCalls++
	f.subscribeInputs = append(f.subscribeInputs, in)

	if f.subscribeCalls > len(f.streams) {
		return nil, errors.New("unexpected SubscribeState call")
	}

	streamSpec := f.streams[f.subscribeCalls-1]

	return &fakeStateStream{
		states: streamSpec.states,
		err:    streamSpec.err,
	}, nil
}

// GetState is unused in tests and returns a sentinel error.
func (f *fakeStateClient) GetState(_ context.Context,
	_ *lnrpc.GetStateRequest,
	_ ...grpc.CallOption) (*lnrpc.GetStateResponse, error) {

	return nil, errors.New("not implemented")
}

// Ensure fakeStateClient satisfies the lnrpc.StateClient interface.
var _ lnrpc.StateClient = (*fakeStateClient)(nil)

// errNotImplemented is returned by fake methods that are unused in tests.
var errNotImplemented = errors.New("not implemented")

// fakeUnlockerClient implements lnrpc.WalletUnlockerClient for tests.
type fakeUnlockerClient struct {
	unlockCalls int
	lastReq     *lnrpc.UnlockWalletRequest
	unlockErr   error
}

// GenSeed is unused in tests and returns a sentinel error.
func (f *fakeUnlockerClient) GenSeed(_ context.Context, _ *lnrpc.GenSeedRequest,
	_ ...grpc.CallOption) (*lnrpc.GenSeedResponse, error) {

	return nil, errNotImplemented
}

// InitWallet is unused in tests and returns a sentinel error.
func (f *fakeUnlockerClient) InitWallet(_ context.Context,
	_ *lnrpc.InitWalletRequest,
	_ ...grpc.CallOption) (*lnrpc.InitWalletResponse, error) {

	return nil, errNotImplemented
}

// UnlockWallet records the request and returns the configured response.
func (f *fakeUnlockerClient) UnlockWallet(_ context.Context,
	in *lnrpc.UnlockWalletRequest,
	_ ...grpc.CallOption) (*lnrpc.UnlockWalletResponse, error) {

	f.unlockCalls++
	f.lastReq = in

	if f.unlockErr != nil {
		return nil, f.unlockErr
	}

	return &lnrpc.UnlockWalletResponse{}, nil
}

// ChangePassword is unused in tests and returns a sentinel error.
func (f *fakeUnlockerClient) ChangePassword(_ context.Context,
	_ *lnrpc.ChangePasswordRequest,
	_ ...grpc.CallOption) (*lnrpc.ChangePasswordResponse, error) {

	return nil, errNotImplemented
}

// Ensure fakeUnlockerClient satisfies the lnrpc.WalletUnlockerClient interface.
var _ lnrpc.WalletUnlockerClient = (*fakeUnlockerClient)(nil)

// newUnlockContext builds a cli.Context with unlock flags parsed.
func newUnlockContext(t *testing.T, args []string) *cli.Context {
	t.Helper()

	flagSet := flag.NewFlagSet("unlock", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)
	flagSet.Bool("stdin", false, "")
	flagSet.Int64("recovery_window", 0, "")
	flagSet.Bool("stateless_init", false, "")

	err := flagSet.Parse(args)
	require.NoError(t, err)

	app := cli.NewApp()

	return cli.NewContext(app, flagSet, nil)
}

// TestUnlock exercises wallet unlock command across success and error paths.
func TestUnlock(t *testing.T) {
	// Shortcut for a long name.
	const waitingToString = lnrpc.WalletState_WAITING_TO_START

	// Define table-driven cases for unlockWithDeps behavior and inputs.
	testCases := []struct {
		name                    string
		args                    []string
		stdinInput              string
		readPasswordRet         []byte
		readPasswordErr         error
		stateStreams            []stateStreamSpec
		unlockerErr             error
		expectErr               string
		expectReadPasswordCalls int
		expectUnlockCalls       int
		expectSubscribeCalls    int
		expectReq               *lnrpc.UnlockWalletRequest
	}{
		// Succeeds by waiting for locked then RPC active.
		{
			name:            "success_default",
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					states: []lnrpc.WalletState{
						waitingToString,
						lnrpc.WalletState_LOCKED,
					},
				},
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_RPC_ACTIVE,
					},
				},
			},
			expectReadPasswordCalls: 1,
			expectUnlockCalls:       1,
			expectSubscribeCalls:    2,
			expectReq: &lnrpc.UnlockWalletRequest{
				WalletPassword: []byte("pw"),
				RecoveryWindow: 0,
				StatelessInit:  false,
			},
		},

		// Uses stdin, stateless init, and recovery window flag.
		{
			name: "success_stdin_flag_recovery_stateless",
			args: []string{
				"--stdin", "--stateless_init",
				"--recovery_window=50",
			},
			stdinInput: "secret\n",
			stateStreams: []stateStreamSpec{
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_LOCKED,
					},
				},
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_UNLOCKED,
					},
				},
			},
			expectReadPasswordCalls: 0,
			expectUnlockCalls:       1,
			expectSubscribeCalls:    2,
			expectReq: &lnrpc.UnlockWalletRequest{
				WalletPassword: []byte("secret"),
				RecoveryWindow: 50,
				StatelessInit:  true,
			},
		},

		// Uses positional recovery window argument.
		{
			name:            "success_arg_recovery_window",
			args:            []string{"25"},
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_LOCKED,
					},
				},
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_SERVER_ACTIVE,
					},
				},
			},
			expectReadPasswordCalls: 1,
			expectUnlockCalls:       1,
			expectSubscribeCalls:    2,
			expectReq: &lnrpc.UnlockWalletRequest{
				WalletPassword: []byte("pw"),
				RecoveryWindow: 25,
				StatelessInit:  false,
			},
		},

		// Propagates password read errors.
		{
			name:                    "read_password_error",
			readPasswordErr:         errors.New("read fail"),
			expectErr:               "read fail",
			expectReadPasswordCalls: 1,
		},

		// Fails when positional recovery window is not an int.
		{
			name:                    "bad_recovery_arg",
			args:                    []string{"not-int"},
			readPasswordRet:         []byte("pw"),
			expectErr:               "invalid syntax",
			expectReadPasswordCalls: 1,
		},

		// EOF while waiting for locked state returns a descriptive
		// error.
		{
			name:            "wait_locked_eof",
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					err: io.EOF,
				},
			},
			expectErr: "lnd shut down before reaching expected " +
				"wallet state",
			expectReadPasswordCalls: 1,
			expectSubscribeCalls:    1,
		},

		// Unimplemented StateService skips lock wait then succeeds.
		{
			name:            "wait_locked_unimplemented",
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					err: status.Error(
						codes.Unimplemented, "no state",
					),
				},
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_UNLOCKED,
					},
				},
			},
			expectReadPasswordCalls: 1,
			expectUnlockCalls:       1,
			expectSubscribeCalls:    2,
			expectReq: &lnrpc.UnlockWalletRequest{
				WalletPassword: []byte("pw"),
				RecoveryWindow: 0,
				StatelessInit:  false,
			},
		},

		// Unavailable StateService skips lock wait then succeeds.
		{
			name:            "wait_locked_unavailable",
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					err: status.Error(
						codes.Unavailable, "no state",
					),
				},
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_RPC_ACTIVE,
					},
				},
			},
			expectReadPasswordCalls: 1,
			expectUnlockCalls:       1,
			expectSubscribeCalls:    2,
			expectReq: &lnrpc.UnlockWalletRequest{
				WalletPassword: []byte("pw"),
				RecoveryWindow: 0,
				StatelessInit:  false,
			},
		},

		// NON_EXISTING during lock wait fails before unlock.
		{
			name:            "wait_locked_non_existing",
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_NON_EXISTING,
					},
				},
			},
			expectErr: "wallet is not initialized - please run " +
				"'lncli create'",
			expectReadPasswordCalls: 1,
			expectSubscribeCalls:    1,
		},

		// Already unlocked during lock wait fails before unlock.
		{
			name:            "wait_locked_already_unlocked",
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_UNLOCKED,
					},
				},
			},
			expectErr:               "wallet is already unlocked",
			expectReadPasswordCalls: 1,
			expectSubscribeCalls:    1,
		},

		// Unlock RPC error is returned after lock wait succeeds.
		{
			name:            "unlocker_error",
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_LOCKED,
					},
				},
			},
			unlockerErr:             errors.New("unlock failed"),
			expectErr:               "unlock failed",
			expectReadPasswordCalls: 1,
			expectUnlockCalls:       1,
			expectSubscribeCalls:    1,
			expectReq: &lnrpc.UnlockWalletRequest{
				WalletPassword: []byte("pw"),
				RecoveryWindow: 0,
				StatelessInit:  false,
			},
		},

		// EOF while waiting for unlocked state returns a descriptive
		// error.
		{
			name:            "wait_unlocked_eof",
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_LOCKED,
					},
				},
				{
					err: io.EOF,
				},
			},
			expectErr: "lnd shut down before reaching expected " +
				"wallet state",
			expectReadPasswordCalls: 1,
			expectUnlockCalls:       1,
			expectSubscribeCalls:    2,
			expectReq: &lnrpc.UnlockWalletRequest{
				WalletPassword: []byte("pw"),
				RecoveryWindow: 0,
				StatelessInit:  false,
			},
		},

		// NON_EXISTING during unlocked wait fails after unlock attempt.
		{
			name:            "wait_unlocked_non_existing",
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_LOCKED,
					},
				},
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_NON_EXISTING,
					},
				},
			},
			expectErr: "wallet is not initialized - please run " +
				"'lncli create'",
			expectReadPasswordCalls: 1,
			expectUnlockCalls:       1,
			expectSubscribeCalls:    2,
			expectReq: &lnrpc.UnlockWalletRequest{
				WalletPassword: []byte("pw"),
				RecoveryWindow: 0,
				StatelessInit:  false,
			},
		},

		// Unimplemented StateService skips unlock wait.
		{
			name:            "wait_unlocked_unimplemented",
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_LOCKED,
					},
				},
				{
					err: status.Error(
						codes.Unimplemented, "no state",
					),
				},
			},
			expectReadPasswordCalls: 1,
			expectUnlockCalls:       1,
			expectSubscribeCalls:    2,
			expectReq: &lnrpc.UnlockWalletRequest{
				WalletPassword: []byte("pw"),
				RecoveryWindow: 0,
				StatelessInit:  false,
			},
		},

		// Unavailable StateService skips unlock wait.
		{
			name:            "wait_unlocked_unavailable",
			readPasswordRet: []byte("pw"),
			stateStreams: []stateStreamSpec{
				{
					states: []lnrpc.WalletState{
						lnrpc.WalletState_LOCKED,
					},
				},
				{
					err: status.Error(
						codes.Unavailable, "no state",
					),
				},
			},
			expectReadPasswordCalls: 1,
			expectUnlockCalls:       1,
			expectSubscribeCalls:    2,
			expectReq: &lnrpc.UnlockWalletRequest{
				WalletPassword: []byte("pw"),
				RecoveryWindow: 0,
				StatelessInit:  false,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Build the CLI context with unlock flags and args.
			ctx := newUnlockContext(t, tc.args)

			// Create fake clients with scripted responses.
			unlocker := &fakeUnlockerClient{
				unlockErr: tc.unlockerErr,
			}
			stateClient := &fakeStateClient{
				streams: tc.stateStreams,
			}

			// Track cleanup for the unlocker client.
			unlockerCleaned := false
			getUnlockerClient := func(
				*cli.Context) (lnrpc.WalletUnlockerClient,
				func()) {

				return unlocker, func() {
					unlockerCleaned = true
				}
			}

			// Track cleanup for the state client.
			stateCleaned := false
			getStateClient := func(*cli.Context) (lnrpc.StateClient,
				func()) {

				return stateClient, func() {
					stateCleaned = true
				}
			}

			// Capture password prompt and inject return values.
			readPrompt := ""
			readCalls := 0
			readPasswordFn := func(prompt string) ([]byte, error) {
				readPrompt = prompt
				readCalls++

				return tc.readPasswordRet, tc.readPasswordErr
			}

			// Provide a deterministic context without signal
			// handling.
			contextCalls := 0
			getContextFn := func() context.Context {
				contextCalls++

				return t.Context()
			}

			// Provide stdin input via injected reader.
			stdin := strings.NewReader(tc.stdinInput)

			// Execute unlockWithDeps with injected dependencies.
			err := unlockWithDeps(
				ctx, readPasswordFn, getUnlockerClient,
				getStateClient, getContextFn, stdin,
			)

			// Assert error behavior.
			if tc.expectErr != "" {
				require.ErrorContains(t, err, tc.expectErr)
			} else {
				require.NoError(t, err)
			}

			// Verify password prompt usage.
			require.Equal(t, tc.expectReadPasswordCalls, readCalls)
			if readCalls > 0 {
				require.Equal(
					t, "Input wallet password: ",
					readPrompt,
				)
			}

			// Verify client usage and cleanup behavior.
			require.Equal(
				t, tc.expectUnlockCalls, unlocker.unlockCalls,
			)
			require.Equal(
				t, tc.expectSubscribeCalls,
				stateClient.subscribeCalls,
			)
			require.True(t, unlockerCleaned)
			require.True(t, stateCleaned)
			require.Equal(t, 1, contextCalls)

			// Verify the unlock request fields when applicable.
			if tc.expectReq != nil {
				require.NotNil(t, unlocker.lastReq)
				require.Equal(t, tc.expectReq.WalletPassword,
					unlocker.lastReq.WalletPassword)
				require.Equal(t, tc.expectReq.RecoveryWindow,
					unlocker.lastReq.RecoveryWindow)
				require.Equal(t, tc.expectReq.StatelessInit,
					unlocker.lastReq.StatelessInit)
			} else {
				require.Nil(t, unlocker.lastReq)
			}

			// Verify SubscribeState requests were well-formed.
			require.Len(
				t, stateClient.subscribeInputs,
				stateClient.subscribeCalls,
			)
			for _, req := range stateClient.subscribeInputs {
				require.NotNil(t, req)
				require.Equal(
					t, &lnrpc.SubscribeStateRequest{}, req,
				)
			}
		})
	}
}
