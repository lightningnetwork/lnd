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

	CapturedWalletBalanceRequest         *lnrpc.WalletBalanceRequest
	CapturedChannelBalanceRequest        *lnrpc.ChannelBalanceRequest
	CapturedGetTransactionsRequest       *lnrpc.GetTransactionsRequest
	CapturedSendCoinsRequest             *lnrpc.SendCoinsRequest
	CapturedSubscribeTransactionsRequest *lnrpc.GetTransactionsRequest
	CapturedSendManyRequest              *lnrpc.SendManyRequest
	CapturedNewAddressRequest            *lnrpc.NewAddressRequest
	CapturedNewWitnessAddressRequest     *lnrpc.NewWitnessAddressRequest
	CapturedSignMessageRequest           *lnrpc.SignMessageRequest
	CapturedVerifyMessageRequest         *lnrpc.VerifyMessageRequest
	CapturedConnectPeerRequest           *lnrpc.ConnectPeerRequest
	CapturedDisconnectPeerRequest        *lnrpc.DisconnectPeerRequest
	CapturedListPeersRequest             *lnrpc.ListPeersRequest
	CapturedGetInfoRequest               *lnrpc.GetInfoRequest
	CapturedPendingChannelsRequest       *lnrpc.PendingChannelsRequest
	CapturedListChannelsRequest          *lnrpc.ListChannelsRequest
	CapturedOpenChannelSyncRequest       *lnrpc.OpenChannelRequest
	CapturedOpenChannelRequest           *lnrpc.OpenChannelRequest
	CapturedCloseChannelRequest          *lnrpc.CloseChannelRequest
	CapturedSendPaymentSyncRequest       *lnrpc.SendRequest
	CapturedAddInvoiceRequest            *lnrpc.Invoice
	CapturedListInvoiceRequest           *lnrpc.ListInvoiceRequest
	CapturedLookupInvoice                *lnrpc.PaymentHash
	CapturedSubscribeInvoicesRequest     *lnrpc.InvoiceSubscription
	CapturedDecodePayReqRequest          *lnrpc.PayReqString
	CapturedListPaymentsRequest          *lnrpc.ListPaymentsRequest
	CapturedDeleteAllPaymentsRequest     *lnrpc.DeleteAllPaymentsRequest
	CapturedDescribeGraphRequest         *lnrpc.ChannelGraphRequest
	CapturedChanInfoRequest              *lnrpc.ChanInfoRequest
	CapturedNodeInfoRequest              *lnrpc.NodeInfoRequest
	CapturedQueryRoutesRequest           *lnrpc.QueryRoutesRequest
	CapturedGetNetworkInfoRequest        *lnrpc.NetworkInfoRequest
	CapturedStopDaemonRequest            *lnrpc.StopRequest
	CapturedSubscribeChannelGraphRequest *lnrpc.GraphTopologySubscription
	CapturedDebugLevelRequest            *lnrpc.DebugLevelRequest
	CapturedFeeReportRequest             *lnrpc.FeeReportRequest
	CapturedUpdateChannelPolicyRequest   *lnrpc.PolicyUpdateRequest
}

func NewStubLightningClient() StubLightningClient {
	lightningClient := StubLightningClient{}
	clientStream := NewStubClientStream()
	lightningClient.SendPaymentClient = StubSendPaymentClient{&clientStream}
	return lightningClient
}

func (c *StubLightningClient) WalletBalance(ctx context.Context, in *lnrpc.WalletBalanceRequest, opts ...grpc.CallOption) (*lnrpc.WalletBalanceResponse, error) {
	c.CapturedWalletBalanceRequest = in
	return new(lnrpc.WalletBalanceResponse), nil
}

func (c *StubLightningClient) ChannelBalance(ctx context.Context, in *lnrpc.ChannelBalanceRequest, opts ...grpc.CallOption) (*lnrpc.ChannelBalanceResponse, error) {
	c.CapturedChannelBalanceRequest = in
	return new(lnrpc.ChannelBalanceResponse), nil
}

func (c *StubLightningClient) GetTransactions(ctx context.Context, in *lnrpc.GetTransactionsRequest, opts ...grpc.CallOption) (*lnrpc.TransactionDetails, error) {
	c.CapturedGetTransactionsRequest = in
	return new(lnrpc.TransactionDetails), nil
}

func (c *StubLightningClient) SendCoins(ctx context.Context, in *lnrpc.SendCoinsRequest, opts ...grpc.CallOption) (*lnrpc.SendCoinsResponse, error) {
	c.CapturedSendCoinsRequest = in
	return new(lnrpc.SendCoinsResponse), nil
}

func (c *StubLightningClient) SubscribeTransactions(ctx context.Context, in *lnrpc.GetTransactionsRequest, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeTransactionsClient, error) {
	c.CapturedSubscribeTransactionsRequest = in
	return new(StubLightningSubscribeTransactionsClient), nil
}

func (c *StubLightningClient) SendMany(ctx context.Context, in *lnrpc.SendManyRequest, opts ...grpc.CallOption) (*lnrpc.SendManyResponse, error) {
	c.CapturedSendManyRequest = in
	return new(lnrpc.SendManyResponse), nil
}

func (c *StubLightningClient) NewAddress(ctx context.Context, in *lnrpc.NewAddressRequest, opts ...grpc.CallOption) (*lnrpc.NewAddressResponse, error) {
	c.CapturedNewAddressRequest = in
	response := lnrpc.NewAddressResponse{"Address"}
	return &response, nil
}

func (c *StubLightningClient) NewWitnessAddress(ctx context.Context, in *lnrpc.NewWitnessAddressRequest, opts ...grpc.CallOption) (*lnrpc.NewAddressResponse, error) {
	c.CapturedNewWitnessAddressRequest = in
	return new(lnrpc.NewAddressResponse), nil
}

func (c *StubLightningClient) SignMessage(ctx context.Context, in *lnrpc.SignMessageRequest, opts ...grpc.CallOption) (*lnrpc.SignMessageResponse, error) {
	c.CapturedSignMessageRequest = in
	return new(lnrpc.SignMessageResponse), nil
}

func (c *StubLightningClient) VerifyMessage(ctx context.Context, in *lnrpc.VerifyMessageRequest, opts ...grpc.CallOption) (*lnrpc.VerifyMessageResponse, error) {
	c.CapturedVerifyMessageRequest = in
	return new(lnrpc.VerifyMessageResponse), nil
}

func (c *StubLightningClient) ConnectPeer(ctx context.Context, in *lnrpc.ConnectPeerRequest, opts ...grpc.CallOption) (*lnrpc.ConnectPeerResponse, error) {
	c.CapturedConnectPeerRequest = in
	return new(lnrpc.ConnectPeerResponse), nil
}

func (c *StubLightningClient) DisconnectPeer(ctx context.Context, in *lnrpc.DisconnectPeerRequest, opts ...grpc.CallOption) (*lnrpc.DisconnectPeerResponse, error) {
	c.CapturedDisconnectPeerRequest = in
	return new(lnrpc.DisconnectPeerResponse), nil
}

func (c *StubLightningClient) ListPeers(ctx context.Context, in *lnrpc.ListPeersRequest, opts ...grpc.CallOption) (*lnrpc.ListPeersResponse, error) {
	c.CapturedListPeersRequest = in
	return new(lnrpc.ListPeersResponse), nil
}

func (c *StubLightningClient) GetInfo(ctx context.Context, in *lnrpc.GetInfoRequest, opts ...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {
	c.CapturedGetInfoRequest = in
	return new(lnrpc.GetInfoResponse), nil
}

func (c *StubLightningClient) PendingChannels(ctx context.Context, in *lnrpc.PendingChannelsRequest, opts ...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error) {
	c.CapturedPendingChannelsRequest = in
	return new(lnrpc.PendingChannelsResponse), nil
}

func (c *StubLightningClient) ListChannels(ctx context.Context, in *lnrpc.ListChannelsRequest, opts ...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {
	c.CapturedListChannelsRequest = in
	return new(lnrpc.ListChannelsResponse), nil
}

func (c *StubLightningClient) OpenChannelSync(ctx context.Context, in *lnrpc.OpenChannelRequest, opts ...grpc.CallOption) (*lnrpc.ChannelPoint, error) {
	c.CapturedOpenChannelSyncRequest = in
	return new(lnrpc.ChannelPoint), nil
}

func (c *StubLightningClient) OpenChannel(ctx context.Context, in *lnrpc.OpenChannelRequest, opts ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient, error) {
	c.CapturedOpenChannelRequest = in
	return new(StubLightningOpenChannelClient), nil
}

func (c *StubLightningClient) CloseChannel(ctx context.Context, in *lnrpc.CloseChannelRequest, opts ...grpc.CallOption) (lnrpc.Lightning_CloseChannelClient, error) {
	c.CapturedCloseChannelRequest = in
	return &StubLightningCloseChannelClient{}, nil
}

func (c *StubLightningClient) SendPayment(ctx context.Context, opts ...grpc.CallOption) (lnrpc.Lightning_SendPaymentClient, error) {
	return &c.SendPaymentClient, nil
}

func (c *StubLightningClient) SendPaymentSync(ctx context.Context, in *lnrpc.SendRequest, opts ...grpc.CallOption) (*lnrpc.SendResponse, error) {
	c.CapturedSendPaymentSyncRequest = in
	return new(lnrpc.SendResponse), nil
}

func (c *StubLightningClient) AddInvoice(ctx context.Context, in *lnrpc.Invoice, opts ...grpc.CallOption) (*lnrpc.AddInvoiceResponse, error) {
	c.CapturedAddInvoiceRequest = in
	return new(lnrpc.AddInvoiceResponse), nil
}

func (c *StubLightningClient) ListInvoices(ctx context.Context, in *lnrpc.ListInvoiceRequest, opts ...grpc.CallOption) (*lnrpc.ListInvoiceResponse, error) {
	c.CapturedListInvoiceRequest = in
	return new(lnrpc.ListInvoiceResponse), nil
}

func (c *StubLightningClient) LookupInvoice(ctx context.Context, in *lnrpc.PaymentHash, opts ...grpc.CallOption) (*lnrpc.Invoice, error) {
	c.CapturedLookupInvoice = in
	return new(lnrpc.Invoice), nil
}

func (c *StubLightningClient) SubscribeInvoices(ctx context.Context, in *lnrpc.InvoiceSubscription, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeInvoicesClient, error) {
	c.CapturedSubscribeInvoicesRequest = in
	return new(StubLightningSubscribeInvoicesClient), nil
}

func (c *StubLightningClient) DecodePayReq(ctx context.Context, in *lnrpc.PayReqString, opts ...grpc.CallOption) (*lnrpc.PayReq, error) {
	c.CapturedDecodePayReqRequest = in
	return new(lnrpc.PayReq), nil
}

func (c *StubLightningClient) ListPayments(ctx context.Context, in *lnrpc.ListPaymentsRequest, opts ...grpc.CallOption) (*lnrpc.ListPaymentsResponse, error) {
	c.CapturedListPaymentsRequest = in
	return new(lnrpc.ListPaymentsResponse), nil
}

func (c *StubLightningClient) DeleteAllPayments(ctx context.Context, in *lnrpc.DeleteAllPaymentsRequest, opts ...grpc.CallOption) (*lnrpc.DeleteAllPaymentsResponse, error) {
	c.CapturedDeleteAllPaymentsRequest = in
	return new(lnrpc.DeleteAllPaymentsResponse), nil
}

func (c *StubLightningClient) DescribeGraph(ctx context.Context, in *lnrpc.ChannelGraphRequest, opts ...grpc.CallOption) (*lnrpc.ChannelGraph, error) {
	c.CapturedDescribeGraphRequest = in
	return new(lnrpc.ChannelGraph), nil
}

func (c *StubLightningClient) GetChanInfo(ctx context.Context, in *lnrpc.ChanInfoRequest, opts ...grpc.CallOption) (*lnrpc.ChannelEdge, error) {
	c.CapturedChanInfoRequest = in
	return new(lnrpc.ChannelEdge), nil
}

func (c *StubLightningClient) GetNodeInfo(ctx context.Context, in *lnrpc.NodeInfoRequest, opts ...grpc.CallOption) (*lnrpc.NodeInfo, error) {
	c.CapturedNodeInfoRequest = in
	return new(lnrpc.NodeInfo), nil
}

func (c *StubLightningClient) QueryRoutes(ctx context.Context, in *lnrpc.QueryRoutesRequest, opts ...grpc.CallOption) (*lnrpc.QueryRoutesResponse, error) {
	c.CapturedQueryRoutesRequest = in
	return new(lnrpc.QueryRoutesResponse), nil
}

func (c *StubLightningClient) GetNetworkInfo(ctx context.Context, in *lnrpc.NetworkInfoRequest, opts ...grpc.CallOption) (*lnrpc.NetworkInfo, error) {
	c.CapturedGetNetworkInfoRequest = in
	return new(lnrpc.NetworkInfo), nil
}

func (c *StubLightningClient) StopDaemon(ctx context.Context, in *lnrpc.StopRequest, opts ...grpc.CallOption) (*lnrpc.StopResponse, error) {
	c.CapturedStopDaemonRequest = in
	return new(lnrpc.StopResponse), nil
}

func (c *StubLightningClient) SubscribeChannelGraph(ctx context.Context, in *lnrpc.GraphTopologySubscription, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeChannelGraphClient, error) {
	c.CapturedSubscribeChannelGraphRequest = in
	return new(StubLightningSubscribeChannelGraphClient), nil
}

func (c *StubLightningClient) DebugLevel(ctx context.Context, in *lnrpc.DebugLevelRequest, opts ...grpc.CallOption) (*lnrpc.DebugLevelResponse, error) {
	c.CapturedDebugLevelRequest = in
	return new(lnrpc.DebugLevelResponse), nil
}

func (c *StubLightningClient) FeeReport(ctx context.Context, in *lnrpc.FeeReportRequest, opts ...grpc.CallOption) (*lnrpc.FeeReportResponse, error) {
	c.CapturedFeeReportRequest = in
	return new(lnrpc.FeeReportResponse), nil
}

func (c *StubLightningClient) UpdateChannelPolicy(ctx context.Context, in *lnrpc.PolicyUpdateRequest, opts ...grpc.CallOption) (*lnrpc.PolicyUpdateResponse, error) {
	c.CapturedUpdateChannelPolicyRequest = in
	return new(lnrpc.PolicyUpdateResponse), nil
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
