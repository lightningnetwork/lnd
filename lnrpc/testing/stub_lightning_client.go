package lnrpctesting

import (
	"github.com/lightningnetwork/lnd/lnrpc"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// A LightningClient that doesn't do any work,
// but instead just returns minimal, successful responses for all APIs.
type StubLightningClient struct {
	SendPaymentClient StubSendPaymentClient
	OpenChannelClient lnrpc.Lightning_OpenChannelClient

	CapturedRequest interface{}
}

func NewStubLightningClient() StubLightningClient {
	lightningClient := StubLightningClient{}
	clientStream := NewStubClientStream()
	lightningClient.SendPaymentClient = StubSendPaymentClient{&clientStream}

	openChannelClient := StubLightningOpenChannelClient{&clientStream}
	lightningClient.OpenChannelClient = &openChannelClient
	return lightningClient
}

func (c *StubLightningClient) WalletBalance(ctx context.Context, in *lnrpc.WalletBalanceRequest, opts ...grpc.CallOption) (*lnrpc.WalletBalanceResponse, error) {
	c.CapturedRequest = in
	response := lnrpc.WalletBalanceResponse{
		TotalBalance: 100, ConfirmedBalance: 70, UnconfirmedBalance: 30}
	return &response, nil
}

func (c *StubLightningClient) ChannelBalance(ctx context.Context, in *lnrpc.ChannelBalanceRequest, opts ...grpc.CallOption) (*lnrpc.ChannelBalanceResponse, error) {
	c.CapturedRequest = in
	return &lnrpc.ChannelBalanceResponse{Balance: 42}, nil
}

func (c *StubLightningClient) GetTransactions(ctx context.Context, in *lnrpc.GetTransactionsRequest, opts ...grpc.CallOption) (*lnrpc.TransactionDetails, error) {
	c.CapturedRequest = in

	transaction := lnrpc.Transaction{
		TxHash: "TxHash",
		Amount: 42}
	return &lnrpc.TransactionDetails{[]*lnrpc.Transaction{&transaction}}, nil
}

func (c *StubLightningClient) SendCoins(ctx context.Context, in *lnrpc.SendCoinsRequest, opts ...grpc.CallOption) (*lnrpc.SendCoinsResponse, error) {
	c.CapturedRequest = in
	response := lnrpc.SendCoinsResponse{"BitcoinTxid"}
	return &response, nil
}

func (c *StubLightningClient) SubscribeTransactions(ctx context.Context, in *lnrpc.GetTransactionsRequest, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeTransactionsClient, error) {
	c.CapturedRequest = in
	return new(StubLightningSubscribeTransactionsClient), nil
}

func (c *StubLightningClient) SendMany(ctx context.Context, in *lnrpc.SendManyRequest, opts ...grpc.CallOption) (*lnrpc.SendManyResponse, error) {
	c.CapturedRequest = in
	return &lnrpc.SendManyResponse{"BitcoinTxid"}, nil
}

func (c *StubLightningClient) NewAddress(ctx context.Context, in *lnrpc.NewAddressRequest, opts ...grpc.CallOption) (*lnrpc.NewAddressResponse, error) {
	c.CapturedRequest = in
	return &lnrpc.NewAddressResponse{"Address"}, nil
}

func (c *StubLightningClient) NewWitnessAddress(ctx context.Context, in *lnrpc.NewWitnessAddressRequest, opts ...grpc.CallOption) (*lnrpc.NewAddressResponse, error) {
	c.CapturedRequest = in
	return new(lnrpc.NewAddressResponse), nil
}

func (c *StubLightningClient) SignMessage(ctx context.Context, in *lnrpc.SignMessageRequest, opts ...grpc.CallOption) (*lnrpc.SignMessageResponse, error) {
	c.CapturedRequest = in
	return &lnrpc.SignMessageResponse{Signature: "Signature"}, nil
}

func (c *StubLightningClient) VerifyMessage(ctx context.Context, in *lnrpc.VerifyMessageRequest, opts ...grpc.CallOption) (*lnrpc.VerifyMessageResponse, error) {
	c.CapturedRequest = in
	return new(lnrpc.VerifyMessageResponse), nil
}

func (c *StubLightningClient) ConnectPeer(ctx context.Context, in *lnrpc.ConnectPeerRequest, opts ...grpc.CallOption) (*lnrpc.ConnectPeerResponse, error) {
	c.CapturedRequest = in
	return new(lnrpc.ConnectPeerResponse), nil
}

func (c *StubLightningClient) DisconnectPeer(ctx context.Context, in *lnrpc.DisconnectPeerRequest, opts ...grpc.CallOption) (*lnrpc.DisconnectPeerResponse, error) {
	c.CapturedRequest = in
	return new(lnrpc.DisconnectPeerResponse), nil
}

func (c *StubLightningClient) ListPeers(ctx context.Context, in *lnrpc.ListPeersRequest, opts ...grpc.CallOption) (*lnrpc.ListPeersResponse, error) {
	c.CapturedRequest = in
	peer := createPeer()
	peers := []*lnrpc.Peer{&peer}
	return &lnrpc.ListPeersResponse{peers}, nil
}

func (c *StubLightningClient) GetInfo(ctx context.Context, in *lnrpc.GetInfoRequest, opts ...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {
	c.CapturedRequest = in

	response := lnrpc.GetInfoResponse{
		IdentityPubkey:     "Pubkey",
		Alias:              "Alias",
		NumPendingChannels: 55,
		NumActiveChannels:  21,
		NumPeers:           77,
		BlockHeight:        123456,
		BlockHash:          "BlockHash",
		SyncedToChain:      true,
		Testnet:            true,
		Chains:             []string{"Bitcoin", "Litecoin"},
		Uris:               []string{"URI0", "URI1"}}

	return &response, nil
}

func (c *StubLightningClient) PendingChannels(ctx context.Context, in *lnrpc.PendingChannelsRequest, opts ...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error) {
	c.CapturedRequest = in
	return new(lnrpc.PendingChannelsResponse), nil
}

func (c *StubLightningClient) ListChannels(ctx context.Context, in *lnrpc.ListChannelsRequest, opts ...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {
	c.CapturedRequest = in

	channel := lnrpc.Channel{
		Active:        true,
		RemotePubkey:  "RemotePubkey",
		LocalBalance:  1234,
		RemoteBalance: 5678}

	return &lnrpc.ListChannelsResponse{[]*lnrpc.Channel{&channel}}, nil
}

func (c *StubLightningClient) OpenChannelSync(ctx context.Context, in *lnrpc.OpenChannelRequest, opts ...grpc.CallOption) (*lnrpc.ChannelPoint, error) {
	c.CapturedRequest = in
	return new(lnrpc.ChannelPoint), nil
}

func (c *StubLightningClient) OpenChannel(ctx context.Context, in *lnrpc.OpenChannelRequest, opts ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient, error) {
	c.CapturedRequest = in
	return c.OpenChannelClient, nil
}

func (c *StubLightningClient) CloseChannel(ctx context.Context, in *lnrpc.CloseChannelRequest, opts ...grpc.CallOption) (lnrpc.Lightning_CloseChannelClient, error) {
	c.CapturedRequest = in
	return &StubLightningCloseChannelClient{}, nil
}

func (c *StubLightningClient) SendPayment(ctx context.Context, opts ...grpc.CallOption) (lnrpc.Lightning_SendPaymentClient, error) {
	return &c.SendPaymentClient, nil
}

func (c *StubLightningClient) SendPaymentSync(ctx context.Context, in *lnrpc.SendRequest, opts ...grpc.CallOption) (*lnrpc.SendResponse, error) {
	c.CapturedRequest = in
	return new(lnrpc.SendResponse), nil
}

func (c *StubLightningClient) AddInvoice(ctx context.Context, in *lnrpc.Invoice, opts ...grpc.CallOption) (*lnrpc.AddInvoiceResponse, error) {
	c.CapturedRequest = in
	return &lnrpc.AddInvoiceResponse{RHash: []byte{123}, PaymentRequest: "PaymentRequest"}, nil
}

func (c *StubLightningClient) ListInvoices(ctx context.Context, in *lnrpc.ListInvoiceRequest, opts ...grpc.CallOption) (*lnrpc.ListInvoiceResponse, error) {
	c.CapturedRequest = in
	invoice := lnrpc.Invoice{
		Value:           5000,
		Receipt:         []uint8{0x78, 0x9a, 0xaa},
		DescriptionHash: []uint8{0x45, 0x6d, 0xef},
		RPreimage:       []uint8{0x12, 0x3a, 0xbc}}
	return &lnrpc.ListInvoiceResponse{[]*lnrpc.Invoice{&invoice}}, nil
}

func (c *StubLightningClient) LookupInvoice(ctx context.Context, in *lnrpc.PaymentHash, opts ...grpc.CallOption) (*lnrpc.Invoice, error) {
	c.CapturedRequest = in
	return new(lnrpc.Invoice), nil
}

func (c *StubLightningClient) SubscribeInvoices(ctx context.Context, in *lnrpc.InvoiceSubscription, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeInvoicesClient, error) {
	c.CapturedRequest = in
	return new(StubLightningSubscribeInvoicesClient), nil
}

func (c *StubLightningClient) DecodePayReq(ctx context.Context, in *lnrpc.PayReqString, opts ...grpc.CallOption) (*lnrpc.PayReq, error) {
	c.CapturedRequest = in
	return new(lnrpc.PayReq), nil
}

func (c *StubLightningClient) ListPayments(ctx context.Context, in *lnrpc.ListPaymentsRequest, opts ...grpc.CallOption) (*lnrpc.ListPaymentsResponse, error) {
	c.CapturedRequest = in

	payment := lnrpc.Payment{
		PaymentHash: "PaymentHash",
		Value:       765,
	}
	return &lnrpc.ListPaymentsResponse{[]*lnrpc.Payment{&payment}}, nil
}

func (c *StubLightningClient) DeleteAllPayments(ctx context.Context, in *lnrpc.DeleteAllPaymentsRequest, opts ...grpc.CallOption) (*lnrpc.DeleteAllPaymentsResponse, error) {
	return new(lnrpc.DeleteAllPaymentsResponse), nil
}

func (c *StubLightningClient) DescribeGraph(ctx context.Context, in *lnrpc.ChannelGraphRequest, opts ...grpc.CallOption) (*lnrpc.ChannelGraph, error) {
	c.CapturedRequest = in
	return new(lnrpc.ChannelGraph), nil
}

func (c *StubLightningClient) GetChanInfo(ctx context.Context, in *lnrpc.ChanInfoRequest, opts ...grpc.CallOption) (*lnrpc.ChannelEdge, error) {
	c.CapturedRequest = in
	return new(lnrpc.ChannelEdge), nil
}

func (c *StubLightningClient) GetNodeInfo(ctx context.Context, in *lnrpc.NodeInfoRequest, opts ...grpc.CallOption) (*lnrpc.NodeInfo, error) {
	c.CapturedRequest = in
	return new(lnrpc.NodeInfo), nil
}

func (c *StubLightningClient) QueryRoutes(ctx context.Context, in *lnrpc.QueryRoutesRequest, opts ...grpc.CallOption) (*lnrpc.QueryRoutesResponse, error) {
	c.CapturedRequest = in

	route := lnrpc.Route{
		TotalTimeLock: 123,
		TotalFees:     456,
		TotalAmt:      789}
	return &lnrpc.QueryRoutesResponse{[]*lnrpc.Route{&route}}, nil
}

func (c *StubLightningClient) GetNetworkInfo(ctx context.Context, in *lnrpc.NetworkInfoRequest, opts ...grpc.CallOption) (*lnrpc.NetworkInfo, error) {
	c.CapturedRequest = in
	return new(lnrpc.NetworkInfo), nil
}

func (c *StubLightningClient) StopDaemon(ctx context.Context, in *lnrpc.StopRequest, opts ...grpc.CallOption) (*lnrpc.StopResponse, error) {
	c.CapturedRequest = in
	return new(lnrpc.StopResponse), nil
}

func (c *StubLightningClient) SubscribeChannelGraph(ctx context.Context, in *lnrpc.GraphTopologySubscription, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeChannelGraphClient, error) {
	c.CapturedRequest = in
	return new(StubLightningSubscribeChannelGraphClient), nil
}

func (c *StubLightningClient) DebugLevel(ctx context.Context, in *lnrpc.DebugLevelRequest, opts ...grpc.CallOption) (*lnrpc.DebugLevelResponse, error) {
	c.CapturedRequest = in
	return new(lnrpc.DebugLevelResponse), nil
}

func (c *StubLightningClient) FeeReport(ctx context.Context, in *lnrpc.FeeReportRequest, opts ...grpc.CallOption) (*lnrpc.FeeReportResponse, error) {
	c.CapturedRequest = in

	channelFeeReport := lnrpc.ChannelFeeReport{
		ChanPoint:   "ChanPoint",
		BaseFeeMsat: 789,
		FeePerMil:   456,
		FeeRate:     123}

	return &lnrpc.FeeReportResponse{
		[]*lnrpc.ChannelFeeReport{&channelFeeReport},
		0,
		0,
		0}, nil
}

func (c *StubLightningClient) UpdateChannelPolicy(ctx context.Context, in *lnrpc.PolicyUpdateRequest, opts ...grpc.CallOption) (*lnrpc.PolicyUpdateResponse, error) {
	c.CapturedRequest = in
	return new(lnrpc.PolicyUpdateResponse), nil
}

func (c *StubLightningClient) ForwardingHistory(ctx context.Context, in *lnrpc.ForwardingHistoryRequest, opts ...grpc.CallOption) (*lnrpc.ForwardingHistoryResponse, error) {
	c.CapturedRequest = in
	return new(lnrpc.ForwardingHistoryResponse), nil
}

// A duplicate of lightningSendPaymentClient, but that is private so it
// must be duplicated here. The only other option is using the lnrpc
// namespace so that private access can be obtained, cluttering a non-test
// namespace with test objects in the process.
type StubSendPaymentClient struct {
	grpc.ClientStream
}

func (x *StubSendPaymentClient) Send(m *lnrpc.SendRequest) error {
	return x.ClientStream.SendMsg(m)
}

func (x *StubSendPaymentClient) Recv() (*lnrpc.SendResponse, error) {
	m := new(lnrpc.SendResponse)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func createPeer() lnrpc.Peer {
	return lnrpc.Peer{
		PubKey:    "PubKey",
		Address:   "Address",
		BytesSent: 78,
		BytesRecv: 89,
		SatSent:   123,
		SatRecv:   456,
		Inbound:   true,
		PingTime:  7890,
	}
}
