package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/assert"
	"github.com/urfave/cli"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// openChannel should print the correct channel point when the bare minimum
// arguments are passed.
func TestOpenChannel(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanOpenUpdateBytes()},
		[]string{PubKey, LocalAmount, PushAmount},
		expectedRequest(),
		"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

// node_key can be passed as a flag instead of an argument.
func TestOpenChannel_NodeKeyFlag(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanOpenUpdateBytes()},
		[]string{"--node_key", PubKey, LocalAmount, PushAmount},
		expectedRequest(),
		"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

// peer_id can be specified instead of node_key.
func TestOpenChannel_PeerId(t *testing.T) {
	// The normal expected request has NodeKey set instead of PeerId. Override that.
	expectedReq := expectedRequest()
	expectedReq.TargetPeerId = PeerIdInt
	expectedReq.NodePubkey = nil

	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanOpenUpdateBytes()},
		[]string{"--peer_id", PeerId, LocalAmount, PushAmount},
		expectedReq,
		"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

// local_amt can be passed as a flag instead of an argument.
func TestOpenChannel_LocalAmtFlag(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanOpenUpdateBytes()},
		[]string{"--local_amt", LocalAmount, PubKey, PushAmount},
		expectedRequest(),
		"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

// push_amt can be passed as a flag instead of an argument.
func TestOpenChannel_PushAmtFlag(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanOpenUpdateBytes()},
		[]string{"--push_amt", PushAmount, PubKey, LocalAmount},
		expectedRequest(),
		"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

// push_amt doesn't have to be specified and will be defaulted if it isn't.
func TestOpenChannel_DefaultPushAmt(t *testing.T) {
	expectedReq := expectedRequest()
	expectedReq.PushSat = 0
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanOpenUpdateBytes()},
		[]string{PubKey, "--local_amt", LocalAmount},
		expectedReq,
		"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

// The funding txid can be passed as a string instead of bytes.
func TestOpenChannel_FundingTxidString(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanOpenUpdateString()},
		[]string{PubKey, LocalAmount, PushAmount},
		expectedRequest(),
		"{\n\t\"channel_point\": \"0000000000000000000000000000000000000000000000001234567890abcdef:6\"\n}\n")
}

// Specifying that a call should block has no effect if the first update
// that is received back confirms the channel.
func TestOpenChannel_UnnecessaryBlock(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanOpenUpdateBytes()},
		[]string{"--block", PubKey, LocalAmount, PushAmount},
		expectedRequest(),
		"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

// Verify that all pass through flags are passed through to the RPC call.
func TestOpenChannel_OverrideDefaults(t *testing.T) {
	expectedReq := expectedRequest()
	expectedReq.TargetConf = 54321
	expectedReq.SatPerByte = 1001
	expectedReq.MinHtlcMsat = 2000000
	expectedReq.Private = true
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanOpenUpdateBytes()},
		[]string{
			"--private",
			"--conf_target", "54321",
			"--sat_per_byte", "1001",
			"--min_htlc_msat", "2000000",
			PubKey, LocalAmount, PushAmount},
		expectedReq,
		"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

// openChannel endlessly loops if unrecognized or nil OpenStatusUpdates are returned.
// This probably isn't the correct behavior. This also happens with any infinitely
// repeating sequence of valid OpenStatusUpdates.
func TestOpenChannel_NoTerminationIfUnrecognizedUpdate(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testOpenChannel(t, &client, []string{PubKey, LocalAmount, PushAmount})
	assert.Equal(t, ErrTimeout, err)
}

// Help text should be printed if no arguments are passed.
func TestOpenChannel_Help(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, _ := testOpenChannel(t, &client, []string{})
	// Checking the whole usage text would result in too much test churn
	// so just verify that a portion of it is present.
	assert.True(t,
		strings.Contains(
			resp, "openchannel - Open a channel to an existing peer."),
		"Expected usage text to be printed but something else was.")
}

// Peer ID and pubkey can't both be specified.
func TestOpenChannel_PeerIdAndPubKey(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testOpenChannel(t, &client,
		[]string{"--peer_id", PeerId, "--node_key", PubKey})
	assert.Equal(t, ErrDuplicatePeerSpecifiers, err)
}

// Reject invalid pubkeys.
func TestOpenChannel_BadPubKeyFromFlag(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testOpenChannel(t, &client, []string{"--node_key", "BadPubKey"})
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "unable to decode node public key"))
}

// Reject invalid pubkeys.
func TestOpenChannel_BadPubKeyFromArg(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testOpenChannel(t, &client, []string{"BadPubKey"})
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "unable to decode node public key"))
}

// Either pubkey or peer ID must be specified.
func TestOpenChannel_NoNodeId(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testOpenChannel(t, &client, []string{"--local_amt", LocalAmount})
	assert.Equal(t, ErrMissingPeerSpecifiers, err)
}

// Reject bad local amounts.
func TestOpenChannel_BadLocalAmtFromArg(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testOpenChannel(t, &client,
		[]string{PubKey, "InvalidLocalAmount", PushAmount})
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "unable to decode local amt"))
}

// Reject bad local amounts.
// Bug: This is supposed to fail.
func TestOpenChannel_BadLocalAmtFromFlag(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testOpenChannel(t, &client,
		[]string{"--local_amt", "InvalidLocalAmount", PubKey, PushAmount})
	//TODO: Remove this the following line and uncomment the ones after upon bug fix.
	assert.NoError(t, err)
	assert.True(t, strings.Contains(
		resp,
		"invalid value \"InvalidLocalAmount\" for flag -local_amt"))
	//assert.Error(t, err)
	//assert.True(t, strings.Contains(err.Error(), "unable to decode local amt"))
}

// Local amount must be specified.
func TestOpenChannel_NoLocalAmt(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testOpenChannel(t, &client, []string{PubKey, "--push_amt", PushAmount})
	assert.Equal(t, ErrMissingLocalAmount, err)
}

// Reject bad push amounts.
func TestOpenChannel_BadPushAmtFromArg(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testOpenChannel(t, &client,
		[]string{PubKey, LocalAmount, "InvalidPushAmount"})
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "unable to decode push amt"))
}

// Reject bad push amounts.
// Bug: This is supposed to fail.
func TestOpenChannel_BadPushAmtFromFlag(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testOpenChannel(t, &client,
		[]string{"--push_amt", "InvalidPushAmount", PubKey, LocalAmount})
	//TODO: Remove this the following line and uncomment the ones after upon bug fix.
	assert.NoError(t, err)
	assert.True(t, strings.Contains(
		resp,
		"invalid value \"InvalidPushAmount\" for flag -push_amt"))
	//assert.Error(t, err)
	//assert.True(t, strings.Contains(err.Error(), "unable to decode push amt"))
}

// Most errors that occur during opening a channel should be propagated up unmodified.
func TestOpenChannel_Failed(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := testOpenChannel(t, &client, []string{PubKey, LocalAmount, PushAmount})
	assert.Error(t, err)
	assert.Equal(t, io.ErrClosedPipe, err, "Incorrect error returned.")
}

// EOF errors are currently propagated up unmodified,
// but should be given a friendlier form.
func TestOpenChannel_FailedWithEOF(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.EOF)
	_, err := testOpenChannel(t, &client, []string{PubKey, LocalAmount, PushAmount})
	assert.Error(t, err)
	assert.Equal(t, io.EOF, err, "Incorrect error returned.")
}

// Errors when receiving updates are propagated up unmodified.
// it's likely that these errors should be distinguished from errors in the
// initial connection, but they currently are represented identically.
func TestOpenChannel_RecvFailed(t *testing.T) {
	client := NewStubClient([]lnrpc.OpenStatusUpdate{}, io.ErrClosedPipe)
	_, err := testOpenChannel(t, &client, []string{PubKey, LocalAmount, PushAmount})
	assert.Error(t, err)
	assert.Equal(t, io.ErrClosedPipe, err, "Incorrect error returned.")
}

// It is currently not an error for EOF to occur immediately after a
// successful OpenChannel call.
func TestOpenChannel_RecvEOF(t *testing.T) {
	client := NewStubClient([]lnrpc.OpenStatusUpdate{}, io.EOF)
	resp, err := testOpenChannel(t, &client, []string{PubKey, LocalAmount, PushAmount})
	assert.NoError(t, err)
	assert.Equal(t, "", resp, "Incorrect response from openChannel.")
}

// Non-blocking calls that retrieve a ChanPending are successes.
func TestOpenChannel_NonBlockingChanPending(t *testing.T) {
	client := NewStubClient(
		[]lnrpc.OpenStatusUpdate{chanPendingUpdate()}, io.EOF)
	resp, err := testOpenChannel(t, &client, []string{PubKey, LocalAmount, PushAmount})
	assert.NoError(t, err)
	assert.Equal(t,
		"{\n\t\"funding_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n",
		resp,
		"Incorrect response from openChannel.")
}

// A terminated connection after a ChanPending currently does not result in an error.
func TestOpenChannel_ChanPendingThenEOF(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanPendingUpdate()},
		[]string{"--block", PubKey, LocalAmount, PushAmount},
		expectedRequest(),
		"{\n\t\"funding_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// A ChanPending followed by a ChanOpen should print a txid followed by a channel point.
func TestOpenChannel_ChanPendingThenChanOpen(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanPendingUpdate(), chanOpenUpdateBytes()},
		[]string{"--block", PubKey, LocalAmount, PushAmount},
		expectedRequest(),
		"{\n\t\"funding_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

// A ChanOpen followed by a ChanPending currently prints a channel point
// followed by txid.
// It's not clear that this order of events is valid.
func TestOpenChannel_ChanOpenThenChanPending(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{chanOpenUpdateBytes(), chanPendingUpdate()},
		[]string{"--block", PubKey, LocalAmount, PushAmount},
		expectedRequest(),
		"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n\n"+
			"{\n\t\"funding_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n")
}

// A bad tx hash should result in an error being propagated up.
func TestOpenChannel_ChanPendingBadTxHash(t *testing.T) {
	bytes := make([]byte, chainhash.HashSize-1)
	client := NewStubClient(
		[]lnrpc.OpenStatusUpdate{chanPendingUpdateWithTxid(bytes)},
		io.EOF)
	_, err := testOpenChannel(t, &client, []string{PubKey, LocalAmount, PushAmount})
	assert.Error(t, err)
	assert.Equal(t,
		"invalid hash length of 31, want 32", err.Error(), "Incorrect error returned.")
}

// A bad tx hash should result in an error being propagated up.
func TestOpenChannel_ChanOpenBadTxHash(t *testing.T) {
	client := NewStubClient([]lnrpc.OpenStatusUpdate{chanOpenUpdateBadBytes()}, io.EOF)
	_, err := testOpenChannel(t, &client, []string{PubKey, LocalAmount, PushAmount})
	assert.Error(t, err)
	assert.Equal(t,
		"invalid hash length of 31, want 32", err.Error(), "Incorrect error returned.")
}

// A bad txid stringshould result in an error being propagated up.
func TestOpenChannel_ChanOpenBadFundingTxidString(t *testing.T) {
	update := chanOpenUpdateWithChannelPoint(lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidStr{
			FundingTxidStr: "BadFundingTxidStr",
		},
		OutputIndex: 6,
	})

	client := NewStubClient([]lnrpc.OpenStatusUpdate{update}, io.EOF)
	_, err := testOpenChannel(t, &client, []string{PubKey, LocalAmount, PushAmount})
	assert.Error(t, err)
	assert.Equal(t,
		"encoding/hex: invalid byte: U+0075 'u'", err.Error(), "Incorrect error returned.")
}

// Regardless of how many ChanPendings are received, a ChanOpen at the end should succeed.
func TestOpenChannel_MultipleChanPendingThenChanOpen(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{
			chanPendingUpdate(), chanPendingUpdate(), chanPendingUpdate(), chanOpenUpdateBytes()},
		[]string{"--block", PubKey, LocalAmount, PushAmount},
		expectedRequest(),
		"{\n\t\"funding_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"funding_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"funding_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

// Currently it doesn't matter if multiple ChanOpens are received.
func TestOpenChannel_MultipleChanOpen(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{
			chanOpenUpdateBytes(), chanOpenUpdateBytes(), chanOpenUpdateBytes(), chanOpenUpdateBytes()},
		[]string{"--block", PubKey, LocalAmount, PushAmount},
		expectedRequest(),
		"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n\n"+
			"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n\n"+
			"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n\n"+
			"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

// Currently it doesn't matter what sequence of ChanPendings and ChanOpens are received.
func TestOpenChannel_MultipleAlternatingChanPendingAndChanOpen(t *testing.T) {
	testErrorlessOpenChannel(t,
		[]lnrpc.OpenStatusUpdate{
			chanPendingUpdate(), chanOpenUpdateBytes(), chanPendingUpdate(), chanOpenUpdateBytes()},
		[]string{"--block", PubKey, LocalAmount, PushAmount},
		expectedRequest(),
		"{\n\t\"funding_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n\n"+
			"{\n\t\"funding_txid\": \"0000000000000000000000000000000089000000000000000000000000000000\"\n}\n\n"+
			"{\n\t\"channel_point\": \"0000000000000000000000000000000012000000000000000000000000000000:5\"\n}\n")
}

var OpenChannelTimeout = 50 * time.Millisecond
var ErrTimeout = fmt.Errorf("Timed out waiting for the command to complete.")

// Calls openChannel with the specified LightningClient and args.
// Replaces stdout as the writer so that the output can be unit tested (without IO).
// Applies a timeout to the openChannel call since openChannel can loop infinitely,
// and unit tests should terminate, even if non-termination would indicate a bug.
func testOpenChannel(
	t *testing.T, client lnrpc.LightningClient, args []string) (string, error) {

	app := cli.NewApp()
	command := openChannelCommand
	writer := StringWriter{}
	// Redirect the command output from stdout to a writer we can test.
	app.Writer = &writer

	// The actual openChannelCommand causes real network events and
	// prints to the console. For testing purposes we need to override
	// this functionality to stub out the network events and write to
	// a Writer that we can validate.
	var err error
	command.Action = func(context *cli.Context) {
		err = openChannel(context, client, &writer)
	}

	app.Commands = []cli.Command{command}
	args = append([]string{"lncli", "openchannel"}, args...)
	// A channel is needed to tell when openChannel has ended.
	// openChannel can infinitely loop, so it needs to be run on a
	// separate thread with a timeout.
	channel := make(chan string, 1)
	go func() {
		app.Run(args)
		channel <- "goroutine terminated"
	}()

	select {
	case <-channel:
		// openChannel completed within the timeout
		if err != nil {
			return "", err
		}

		return writer.Join(), nil
	case <-time.After(OpenChannelTimeout):
		// openChannel was blocking (probably) indefinitely,
		// which it is currently intended only if no EOF nor error occurred.
		return "", ErrTimeout
	}
}

// Test openChannel validating that no error occurs and that the output
// and the arguments passed to the OpenChannel RPC are correct.
func testErrorlessOpenChannel(
	t *testing.T,
	updates []lnrpc.OpenStatusUpdate,
	args []string,
	expectedRequest lnrpc.OpenChannelRequest,
	expectedResult string) {

	client := NewStubClient(updates, io.EOF)
	resp, err := testOpenChannel(t, &client, args)
	assert.NoError(t, err)
	errorMessage := fmt.Sprintf(
		"Values passed to openChannel were incorrect. Expected\n%+v\n but found\n%+v\n",
		expectedRequest,
		client.capturedRequest)
	// Check that the values passed to the OpenChannel RPC were correct.
	assert.True(t,
		reflect.DeepEqual(expectedRequest, client.capturedRequest),
		errorMessage)

	assert.Equal(t,
		expectedResult,
		resp,
		"Incorrect response from openChannel.")
}

// A LightningClient that has a configurable OpenChannelClient
// and stores the last OpenChannelRequest it sees.
type StubOpenChannelLightningClient struct {
	lnrpctesting.StubLightningClient
	openChannelClient lnrpc.Lightning_OpenChannelClient
	capturedRequest   lnrpc.OpenChannelRequest
}

func (c *StubOpenChannelLightningClient) OpenChannel(
	ctx context.Context, in *lnrpc.OpenChannelRequest, opts ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient, error) {
	c.capturedRequest = *in
	return c.openChannelClient, nil
}

// An OpenChannelClient that terminates with an error after all of its updates
// are provided. Using the io.EOF error results in a successful ending despite
// being technically an error.
type TerminatingStubLightningOpenChannelClient struct {
	grpc.ClientStream
	updates          []lnrpc.OpenStatusUpdate
	terminatingError error
}

// A LightningClient that returns updates followed by the specified error.
func NewStubClient(
	updates []lnrpc.OpenStatusUpdate,
	terminatingError error) StubOpenChannelLightningClient {

	stream := lnrpctesting.NewStubClientStream()
	openChannelClient := TerminatingStubLightningOpenChannelClient{
		&stream, updates, terminatingError}

	return StubOpenChannelLightningClient{
		lnrpctesting.StubLightningClient{}, &openChannelClient, lnrpc.OpenChannelRequest{}}
}

// Iterates through the list of updates, finally returning an error when no updates remain.
func (client *TerminatingStubLightningOpenChannelClient) Recv() (*lnrpc.OpenStatusUpdate, error) {
	if len(client.updates) < 1 {
		return nil, client.terminatingError
	}

	update := client.updates[0]
	client.updates = client.updates[1:]

	return &update, nil
}

func chanOpenUpdateBytes() lnrpc.OpenStatusUpdate {
	hash := chainhash.Hash{}
	bytes := make([]byte, chainhash.HashSize)
	bytes[15] = 0x12
	hash.SetBytes(bytes)
	return chanOpenUpdateWithChannelPoint(lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: hash[:],
		},
		OutputIndex: 5,
	})
}

func chanOpenUpdateString() lnrpc.OpenStatusUpdate {
	return chanOpenUpdateWithChannelPoint(lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidStr{
			FundingTxidStr: "1234567890abcdef",
		},
		OutputIndex: 6,
	})
}

func chanOpenUpdateBadBytes() lnrpc.OpenStatusUpdate {
	bytes := make([]byte, chainhash.HashSize-1)
	bytes[15] = 0x12

	return chanOpenUpdateWithChannelPoint(lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: bytes,
		},
		OutputIndex: 5,
	})
}

func chanOpenUpdateWithChannelPoint(channelPoint lnrpc.ChannelPoint) lnrpc.OpenStatusUpdate {
	return lnrpc.OpenStatusUpdate{
		Update: &lnrpc.OpenStatusUpdate_ChanOpen{
			ChanOpen: &lnrpc.ChannelOpenUpdate{
				ChannelPoint: &channelPoint,
			},
		},
	}
}

func chanPendingUpdate() lnrpc.OpenStatusUpdate {
	hash := chainhash.Hash{}
	bytes := make([]byte, chainhash.HashSize)
	bytes[15] = 0x89
	hash.SetBytes(bytes)
	return chanPendingUpdateWithTxid(hash[:])
}

func chanPendingUpdateWithTxid(txid []byte) lnrpc.OpenStatusUpdate {
	return lnrpc.OpenStatusUpdate{
		Update: &lnrpc.OpenStatusUpdate_ChanPending{
			ChanPending: &lnrpc.PendingUpdate{
				Txid:        txid,
				OutputIndex: 4,
			},
		},
	}
}

// The standard OpenChannelRequest that tests should result in being passed to
// the LightningClient. Some tests that need different values will override them.
func expectedRequest() lnrpc.OpenChannelRequest {
	hexPubKey, _ := hex.DecodeString(PubKey)
	return lnrpc.OpenChannelRequest{
		TargetPeerId:       0,
		NodePubkey:         hexPubKey,
		NodePubkeyString:   "",
		LocalFundingAmount: LocalAmountInt,
		PushSat:            PushAmountInt,
		TargetConf:         0,
		SatPerByte:         0,
		Private:            false,
		MinHtlcMsat:        0,
	}
}
