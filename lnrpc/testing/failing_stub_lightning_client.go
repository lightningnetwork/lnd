package lnrpctesting

import (
	"github.com/lightningnetwork/lnd/lnrpc"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// A LightningClient that fails with the specified error when any API is called.
type FailingStubLightningClient struct {
	err error
}

func NewFailingStubLightningClient(err error) FailingStubLightningClient {
	return FailingStubLightningClient{err}
}

func (c *FailingStubLightningClient) WalletBalance(ctx context.Context, in *lnrpc.WalletBalanceRequest, opts ...grpc.CallOption) (*lnrpc.WalletBalanceResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) ChannelBalance(ctx context.Context, in *lnrpc.ChannelBalanceRequest, opts ...grpc.CallOption) (*lnrpc.ChannelBalanceResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) GetTransactions(ctx context.Context, in *lnrpc.GetTransactionsRequest, opts ...grpc.CallOption) (*lnrpc.TransactionDetails, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) SendCoins(ctx context.Context, in *lnrpc.SendCoinsRequest, opts ...grpc.CallOption) (*lnrpc.SendCoinsResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) SubscribeTransactions(ctx context.Context, in *lnrpc.GetTransactionsRequest, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeTransactionsClient, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) SendMany(ctx context.Context, in *lnrpc.SendManyRequest, opts ...grpc.CallOption) (*lnrpc.SendManyResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) NewAddress(ctx context.Context, in *lnrpc.NewAddressRequest, opts ...grpc.CallOption) (*lnrpc.NewAddressResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) NewWitnessAddress(ctx context.Context, in *lnrpc.NewWitnessAddressRequest, opts ...grpc.CallOption) (*lnrpc.NewAddressResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) SignMessage(ctx context.Context, in *lnrpc.SignMessageRequest, opts ...grpc.CallOption) (*lnrpc.SignMessageResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) VerifyMessage(ctx context.Context, in *lnrpc.VerifyMessageRequest, opts ...grpc.CallOption) (*lnrpc.VerifyMessageResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) ConnectPeer(ctx context.Context, in *lnrpc.ConnectPeerRequest, opts ...grpc.CallOption) (*lnrpc.ConnectPeerResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) DisconnectPeer(ctx context.Context, in *lnrpc.DisconnectPeerRequest, opts ...grpc.CallOption) (*lnrpc.DisconnectPeerResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) ListPeers(ctx context.Context, in *lnrpc.ListPeersRequest, opts ...grpc.CallOption) (*lnrpc.ListPeersResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) GetInfo(ctx context.Context, in *lnrpc.GetInfoRequest, opts ...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) PendingChannels(ctx context.Context, in *lnrpc.PendingChannelsRequest, opts ...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) ListChannels(ctx context.Context, in *lnrpc.ListChannelsRequest, opts ...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) OpenChannelSync(ctx context.Context, in *lnrpc.OpenChannelRequest, opts ...grpc.CallOption) (*lnrpc.ChannelPoint, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) OpenChannel(ctx context.Context, in *lnrpc.OpenChannelRequest, opts ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) CloseChannel(ctx context.Context, in *lnrpc.CloseChannelRequest, opts ...grpc.CallOption) (lnrpc.Lightning_CloseChannelClient, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) SendPayment(ctx context.Context, opts ...grpc.CallOption) (lnrpc.Lightning_SendPaymentClient, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) SendPaymentSync(ctx context.Context, in *lnrpc.SendRequest, opts ...grpc.CallOption) (*lnrpc.SendResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) AddInvoice(ctx context.Context, in *lnrpc.Invoice, opts ...grpc.CallOption) (*lnrpc.AddInvoiceResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) ListInvoices(ctx context.Context, in *lnrpc.ListInvoiceRequest, opts ...grpc.CallOption) (*lnrpc.ListInvoiceResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) LookupInvoice(ctx context.Context, in *lnrpc.PaymentHash, opts ...grpc.CallOption) (*lnrpc.Invoice, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) SubscribeInvoices(ctx context.Context, in *lnrpc.InvoiceSubscription, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeInvoicesClient, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) DecodePayReq(ctx context.Context, in *lnrpc.PayReqString, opts ...grpc.CallOption) (*lnrpc.PayReq, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) ListPayments(ctx context.Context, in *lnrpc.ListPaymentsRequest, opts ...grpc.CallOption) (*lnrpc.ListPaymentsResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) DeleteAllPayments(ctx context.Context, in *lnrpc.DeleteAllPaymentsRequest, opts ...grpc.CallOption) (*lnrpc.DeleteAllPaymentsResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) DescribeGraph(ctx context.Context, in *lnrpc.ChannelGraphRequest, opts ...grpc.CallOption) (*lnrpc.ChannelGraph, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) GetChanInfo(ctx context.Context, in *lnrpc.ChanInfoRequest, opts ...grpc.CallOption) (*lnrpc.ChannelEdge, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) GetNodeInfo(ctx context.Context, in *lnrpc.NodeInfoRequest, opts ...grpc.CallOption) (*lnrpc.NodeInfo, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) QueryRoutes(ctx context.Context, in *lnrpc.QueryRoutesRequest, opts ...grpc.CallOption) (*lnrpc.QueryRoutesResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) GetNetworkInfo(ctx context.Context, in *lnrpc.NetworkInfoRequest, opts ...grpc.CallOption) (*lnrpc.NetworkInfo, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) StopDaemon(ctx context.Context, in *lnrpc.StopRequest, opts ...grpc.CallOption) (*lnrpc.StopResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) SubscribeChannelGraph(ctx context.Context, in *lnrpc.GraphTopologySubscription, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeChannelGraphClient, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) DebugLevel(ctx context.Context, in *lnrpc.DebugLevelRequest, opts ...grpc.CallOption) (*lnrpc.DebugLevelResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) FeeReport(ctx context.Context, in *lnrpc.FeeReportRequest, opts ...grpc.CallOption) (*lnrpc.FeeReportResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) UpdateChannelPolicy(ctx context.Context, in *lnrpc.PolicyUpdateRequest, opts ...grpc.CallOption) (*lnrpc.PolicyUpdateResponse, error) {
	return nil, c.err
}

func (c *FailingStubLightningClient) ForwardingHistory(ctx context.Context, in *lnrpc.ForwardingHistoryRequest, opts ...grpc.CallOption) (*lnrpc.ForwardingHistoryResponse, error) {
	return nil, c.err
}
