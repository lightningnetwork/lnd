package main

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// TestCloseChannel verifies that closeChannel prints the correct
// closing_txid when the bare minimum arguments are passed.
func TestCloseChannel(t *testing.T) {
	expectedReq := expectedCloseChannelRequest()
	expectedReq.ChannelPoint.OutputIndex = 0
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{chanCloseUpdate()},
		[]string{FundingTxidString},
		expectedReq,
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_FundingTxidFlag verifies that FundingTxid can be passed
// as a flag rather than argument.
func TestCloseChannel_FundingTxidFlag(t *testing.T) {
	expectedReq := expectedCloseChannelRequest()
	expectedReq.ChannelPoint.OutputIndex = 0
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{chanCloseUpdate()},
		[]string{"--funding_txid", FundingTxidString},
		expectedReq,
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_OutputIndexArg verified that outputIndex can be passed
// as an argument rather than defaulted
func TestCloseChannel_OutputIndexArg(t *testing.T) {
	expectedReq := expectedCloseChannelRequest()
	expectedReq.ChannelPoint.OutputIndex = OutputIndexInt
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{chanCloseUpdate()},
		[]string{FundingTxidString, OutputIndex},
		expectedReq,
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_OutputIndexFlag verifies that OutputIndex can be passed
// as a flag rather than defaulted
func TestCloseChannel_OutputIndexFlag(t *testing.T) {
	expectedReq := expectedCloseChannelRequest()
	expectedReq.ChannelPoint.OutputIndex = OutputIndexInt
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{chanCloseUpdate()},
		[]string{"--output_index", OutputIndex, FundingTxidString},
		expectedReq,
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_TimeLimitArg verifies that TimeLimit can be passed
// as an argument, but currently does nothing.
func TestCloseChannel_TimeLimitArg(t *testing.T) {
	expectedReq := expectedCloseChannelRequest()
	expectedReq.ChannelPoint.OutputIndex = OutputIndexInt
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{chanCloseUpdate()},
		[]string{FundingTxidString, OutputIndex, TimeLimit},
		expectedReq,
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_TimeLimitFlag verifies that TimeLimit can be passed
// as a flag, but currently does nothing.
func TestCloseChannel_TimeLimitFlag(t *testing.T) {
	expectedReq := expectedCloseChannelRequest()
	expectedReq.ChannelPoint.OutputIndex = OutputIndexInt
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{chanCloseUpdate()},
		[]string{"--time_limit", TimeLimit, FundingTxidString, OutputIndex},
		expectedReq,
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_NoFundingTxid verifies that FundingTxid must be specified.
func TestCloseChannel_NoFundingTxid(t *testing.T) {
	client := NewStubCloseClient([]lnrpc.CloseStatusUpdate{}, io.EOF)
	_, err := runCloseChannel(&client, []string{"--output_index", OutputIndex})
	require.Error(t, err)
	require.Equal(t, ErrMissingFundingTxid, err, "Incorrect error returned.")
}

// TestCloseChannel_BadOutputIndex verifies that outputIndex must be an integer.
func TestCloseChannel_BadOutputIndex(t *testing.T) {
	client := NewStubCloseClient([]lnrpc.CloseStatusUpdate{}, io.EOF)
	_, err := runCloseChannel(&client, []string{FundingTxidString, "BadOutputIndex"})
	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "unable to decode output index:"),
		"Incorrect error message returned.")
}

// TestCloseChannel_UnnecessaryBlock verifies that specifying that a call should
// block has no effect if the first update that is received back confirms
// channel closure.
func TestCloseChannel_UnnecessaryBlock(t *testing.T) {
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{chanCloseUpdate()},
		[]string{"--block", FundingTxidString},
		expectedCloseChannelRequest(),
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_OverrideDefaults verifiest that all pass through flags
// are passed through to the RPC call.
func TestCloseChannel_OverrideDefaults(t *testing.T) {
	expectedReq := expectedCloseChannelRequest()
	expectedReq.Force = true
	expectedReq.TargetConf = 54321
	expectedReq.SatPerByte = 1001
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{chanCloseUpdate()},
		[]string{
			"--force",
			"--conf_target", "54321",
			"--sat_per_byte", "1001",
			FundingTxidString},
		expectedReq,
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_NoTerminationIfUnrecognizedUpdate verifies that
// closeChannel endlessly loops if unrecognized or nil CloseStatusUpdates
// are returned. This probably isn't the correct behavior. This also
// happens with any infinitely repeating sequence of valid CloseStatusUpdates.
func TestCloseChannel_NoTerminationIfUnrecognizedUpdate(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := runCloseChannel(&client, []string{FundingTxidString})
	require.Equal(t, ErrTimeout, err)
}

// TestCloseChannel_Help verifies that help text should be printed
// if no arguments are passed.
func TestCloseChannel_Help(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, _ := runCloseChannel(&client, []string{})
	// Checking the whole usage text would result in too much test churn
	// so just verify that a portion of it is present.
	require.True(t,
		strings.Contains(resp, "closechannel - Close an existing channel."),
		"Expected usage text to be printed but something else was.")
}

// TestCloseChannel_Failed verifies that most errors that occur during closing
// a channel should be propagated up unmodified.
func TestCloseChannel_Failed(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := runCloseChannel(&client, []string{FundingTxidString})
	require.Error(t, err)
	require.Equal(t, io.ErrClosedPipe, err, "Incorrect error returned.")
}

// TestCloseChannel_RecvFailed verifies that errors when receiving updates are
// propagated up unmodified. It's likely that these errors should be
// distinguished from errors in the initial connection, but they currently are
// represented identically.
func TestCloseChannel_RecvFailed(t *testing.T) {
	client := NewStubCloseClient([]lnrpc.CloseStatusUpdate{}, io.ErrClosedPipe)
	_, err := runCloseChannel(&client, []string{FundingTxidString})
	require.Error(t, err)
	require.Equal(t, io.ErrClosedPipe, err, "Incorrect error returned.")
}

// TestCloseChannel_NonBlockingChanClose verifies that non-blocking calls that
// retrieve a ChanPending are successes.
func TestCloseChannel_NonBlockingChanClose(t *testing.T) {
	client := NewStubCloseClient(
		[]lnrpc.CloseStatusUpdate{chanPendingCloseUpdate()}, io.EOF)
	resp, err := runCloseChannel(&client, []string{FundingTxidString})
	require.NoError(t, err)
	require.Equal(t,
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n",
		resp,
		"Incorrect response from closeChannel.")
}

// TestCloseChannel_ChanPendingThenEOF verifies that a terminated connection
// after a ChanPending currently does not result in an error.
func TestCloseChannel_ChanPendingThenEOF(t *testing.T) {
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{chanPendingCloseUpdate()},
		[]string{"--block", FundingTxidString},
		expectedCloseChannelRequest(),
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_ChanPendingThenChanClose verifies that a ChanPending
// followed by a ChanClose should print the same txid twice.
func TestCloseChannel_ChanPendingThenChanClose(t *testing.T) {
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{chanPendingCloseUpdate(), chanCloseUpdate()},
		[]string{"--block", FundingTxidString},
		expectedCloseChannelRequest(),
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_ChanCloseThenChanPending verifies that a ChanClose
// followed by a ChanPending prints the txid twice. It's not clear that this
// order of events is valid.
func TestCloseChannel_ChanCloseThenChanPending(t *testing.T) {
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{chanCloseUpdate(), chanPendingCloseUpdate()},
		[]string{"--block", FundingTxidString},
		expectedCloseChannelRequest(),
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_ChanPendingBadTxHash verifies that a bad tx hash should
// result in an error being propagated up.
func TestCloseChannel_ChanPendingBadTxHash(t *testing.T) {
	bytes := make([]byte, chainhash.HashSize-1)
	client := NewStubCloseClient(
		[]lnrpc.CloseStatusUpdate{chanPendingCloseUpdateWithTxid(bytes)},
		io.EOF)
	_, err := runCloseChannel(&client, []string{FundingTxidString})
	require.Error(t, err)
	require.Equal(t,
		"invalid hash length of 31, want 32", err.Error(), "Incorrect error returned.")
}

// TestCloseChannel_ChanCloseBadTxHash verifies that a bad tx hash should
// result in an error being propagated up.
func TestCloseChannel_ChanCloseBadTxHash(t *testing.T) {
	client := NewStubCloseClient([]lnrpc.CloseStatusUpdate{chanCloseUpdateBadTxid()}, io.EOF)
	_, err := runCloseChannel(&client, []string{FundingTxidString})
	require.Error(t, err)
	require.Equal(t,
		"invalid hash length of 31, want 32", err.Error(), "Incorrect error returned.")
}

// TestCloseChannel_MultipleChanPendingThenChanClose verifies that regardless
// of how many ChanPendings are received, a ChanClose at the end should succeed.
func TestCloseChannel_MultipleChanPendingThenChanClose(t *testing.T) {
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{
			chanPendingCloseUpdate(), chanPendingCloseUpdate(), chanPendingCloseUpdate(), chanCloseUpdate()},
		[]string{"--block", FundingTxidString},
		expectedCloseChannelRequest(),
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_MultipleChanClose verifies that currently it doesn't
// matter if multiple ChanCloses are received.
func TestCloseChannel_MultipleChanClose(t *testing.T) {
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{
			chanCloseUpdate(), chanCloseUpdate(), chanCloseUpdate(), chanCloseUpdate()},
		[]string{"--block", FundingTxidString},
		expectedCloseChannelRequest(),
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// TestCloseChannel_MultipleAlternatingChanPendingAndChanClose verifies that
// currently it doesn't matter what sequence of ChanPendings and ChanCloses are received.
func TestCloseChannel_MultipleAlternatingChanPendingAndChanClose(t *testing.T) {
	testErrorlessCloseChannel(t,
		[]lnrpc.CloseStatusUpdate{
			chanPendingCloseUpdate(), chanCloseUpdate(), chanPendingCloseUpdate(), chanCloseUpdate()},
		[]string{"--block", FundingTxidString},
		expectedCloseChannelRequest(),
		"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"closing_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

func runCloseChannel(client lnrpc.LightningClient, args []string) (string, error) {
	return RunCommandWithTimeout(
		client, closeChannelCommand, closeChannel, "closechannel", args)
}

// Test closeChannel validating that no error occurs and that the output
// and the arguments passed to the CloseChannel RPC are correct.
func testErrorlessCloseChannel(
	t *testing.T,
	updates []lnrpc.CloseStatusUpdate,
	args []string,
	expectedRequest lnrpc.CloseChannelRequest,
	expectedResult string) {

	client := NewStubCloseClient(updates, io.EOF)
	resp, err := runCloseChannel(&client, args)
	require.NoError(t, err)
	errorMessage := fmt.Sprintf(
		"Values passed to closeChannel were incorrect. Expected\n%+v\n but found\n%+v\n",
		expectedRequest,
		client.capturedRequest)
	// Check that the values passed to the CloseChannel RPC were correct.
	require.Equal(t,
		expectedRequest,
		client.capturedRequest,
		errorMessage)

	require.Equal(t,
		expectedResult,
		resp,
		"Incorrect response from closeChannel.")
}

// A LightningClient that has a configurable CloseChannelClient
// and stores the last CloseChannelRequest it sees.
type StubCloseChannelLightningClient struct {
	lnrpctesting.StubLightningClient
	closeChannelClient lnrpc.Lightning_CloseChannelClient
	capturedRequest    lnrpc.CloseChannelRequest
}

func (c *StubCloseChannelLightningClient) CloseChannel(
	ctx context.Context, in *lnrpc.CloseChannelRequest, opts ...grpc.CallOption) (lnrpc.Lightning_CloseChannelClient, error) {
	c.capturedRequest = *in
	return c.closeChannelClient, nil
}

// An CloseChannelClient that terminates with an error after all of its updates
// are provided. Using the io.EOF error results in a successful ending despite
// being technically an error.
type TerminatingStubLightningCloseChannelClient struct {
	grpc.ClientStream
	updates          []lnrpc.CloseStatusUpdate
	terminatingError error
}

// A LightningClient that returns updates followed by the specified error.
func NewStubCloseClient(
	updates []lnrpc.CloseStatusUpdate,
	terminatingError error) StubCloseChannelLightningClient {

	stream := lnrpctesting.NewStubClientStream()
	closeChannelClient := TerminatingStubLightningCloseChannelClient{
		&stream, updates, terminatingError}

	return StubCloseChannelLightningClient{
		lnrpctesting.StubLightningClient{}, &closeChannelClient, lnrpc.CloseChannelRequest{}}
}

// Iterates through the list of updates, finally returning an error when no updates remain.
func (client *TerminatingStubLightningCloseChannelClient) Recv() (*lnrpc.CloseStatusUpdate, error) {
	if len(client.updates) < 1 {
		return nil, client.terminatingError
	}

	update := client.updates[0]
	client.updates = client.updates[1:]

	return &update, nil
}

func chanCloseUpdate() lnrpc.CloseStatusUpdate {
	return chanCloseUpdateWithTxid(
		[]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 137, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
}

func chanCloseUpdateBadTxid() lnrpc.CloseStatusUpdate {
	return chanCloseUpdateWithTxid(
		[]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
}

func chanCloseUpdateWithTxid(txid []byte) lnrpc.CloseStatusUpdate {
	return lnrpc.CloseStatusUpdate{
		Update: &lnrpc.CloseStatusUpdate_ChanClose{
			ChanClose: &lnrpc.ChannelCloseUpdate{
				ClosingTxid: txid,
				Success:     true,
			},
		},
	}
}

func chanPendingCloseUpdate() lnrpc.CloseStatusUpdate {
	hash := chainhash.Hash{}
	bytes := make([]byte, chainhash.HashSize)
	bytes[15] = 0x89
	hash.SetBytes(bytes)
	return chanPendingCloseUpdateWithTxid(hash[:])
}

func chanPendingCloseUpdateWithTxid(txid []byte) lnrpc.CloseStatusUpdate {
	return lnrpc.CloseStatusUpdate{
		Update: &lnrpc.CloseStatusUpdate_ClosePending{
			ClosePending: &lnrpc.PendingUpdate{
				Txid:        txid,
				OutputIndex: 4,
			},
		},
	}
}

// The standard CloseChannelRequest that tests should result in being passed to
// the LightningClient. Some tests that need different values will override them.
func expectedCloseChannelRequest() lnrpc.CloseChannelRequest {
	return lnrpc.CloseChannelRequest{
		ChannelPoint: &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidStr{
				FundingTxidStr: FundingTxidString,
			},
			OutputIndex: 0,
		},
		Force:      false,
		TargetConf: 0,
		SatPerByte: 0,
	}
}
