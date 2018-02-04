package lnrpctesting

import (
	"github.com/lightningnetwork/lnd/lnrpc"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// A LightningClient that doesn't do any work,
// but instead just returns minimal, successful responses for all APIs.
type StubLightningClient struct {
}

func NewStubLightningClient() StubLightningClient {
	return StubLightningClient{}
}

func (c *StubLightningClient) WalletBalance(ctx context.Context, in *lnrpc.WalletBalanceRequest, opts ...grpc.CallOption) (*lnrpc.WalletBalanceResponse, error) {
	return new(lnrpc.WalletBalanceResponse), nil
}

func (c *StubLightningClient) ChannelBalance(ctx context.Context, in *lnrpc.ChannelBalanceRequest, opts ...grpc.CallOption) (*lnrpc.ChannelBalanceResponse, error) {
	return new(lnrpc.ChannelBalanceResponse), nil
}

func (c *StubLightningClient) GetTransactions(ctx context.Context, in *lnrpc.GetTransactionsRequest, opts ...grpc.CallOption) (*lnrpc.TransactionDetails, error) {
	return new(lnrpc.TransactionDetails), nil
}

func (c *StubLightningClient) SendCoins(ctx context.Context, in *lnrpc.SendCoinsRequest, opts ...grpc.CallOption) (*lnrpc.SendCoinsResponse, error) {
	return new(lnrpc.SendCoinsResponse), nil
}

func (c *StubLightningClient) SubscribeTransactions(ctx context.Context, in *lnrpc.GetTransactionsRequest, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeTransactionsClient, error) {
	return new(StubLightningSubscribeTransactionsClient), nil
}

func (c *StubLightningClient) SendMany(ctx context.Context, in *lnrpc.SendManyRequest, opts ...grpc.CallOption) (*lnrpc.SendManyResponse, error) {
	return new(lnrpc.SendManyResponse), nil
}

func (c *StubLightningClient) NewAddress(ctx context.Context, in *lnrpc.NewAddressRequest, opts ...grpc.CallOption) (*lnrpc.NewAddressResponse, error) {
	return new(lnrpc.NewAddressResponse), nil
}

func (c *StubLightningClient) NewWitnessAddress(ctx context.Context, in *lnrpc.NewWitnessAddressRequest, opts ...grpc.CallOption) (*lnrpc.NewAddressResponse, error) {
	return new(lnrpc.NewAddressResponse), nil
}

func (c *StubLightningClient) SignMessage(ctx context.Context, in *lnrpc.SignMessageRequest, opts ...grpc.CallOption) (*lnrpc.SignMessageResponse, error) {
	return new(lnrpc.SignMessageResponse), nil
}

func (c *StubLightningClient) VerifyMessage(ctx context.Context, in *lnrpc.VerifyMessageRequest, opts ...grpc.CallOption) (*lnrpc.VerifyMessageResponse, error) {
	return new(lnrpc.VerifyMessageResponse), nil
}

func (c *StubLightningClient) ConnectPeer(ctx context.Context, in *lnrpc.ConnectPeerRequest, opts ...grpc.CallOption) (*lnrpc.ConnectPeerResponse, error) {
	return new(lnrpc.ConnectPeerResponse), nil
}

func (c *StubLightningClient) DisconnectPeer(ctx context.Context, in *lnrpc.DisconnectPeerRequest, opts ...grpc.CallOption) (*lnrpc.DisconnectPeerResponse, error) {
	return new(lnrpc.DisconnectPeerResponse), nil
}

func (c *StubLightningClient) ListPeers(ctx context.Context, in *lnrpc.ListPeersRequest, opts ...grpc.CallOption) (*lnrpc.ListPeersResponse, error) {
	return new(lnrpc.ListPeersResponse), nil
}

func (c *StubLightningClient) GetInfo(ctx context.Context, in *lnrpc.GetInfoRequest, opts ...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {
	return new(lnrpc.GetInfoResponse), nil
}

func (c *StubLightningClient) PendingChannels(ctx context.Context, in *lnrpc.PendingChannelsRequest, opts ...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error) {
	return new(lnrpc.PendingChannelsResponse), nil
}

func (c *StubLightningClient) ListChannels(ctx context.Context, in *lnrpc.ListChannelsRequest, opts ...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {
	return new(lnrpc.ListChannelsResponse), nil
}

func (c *StubLightningClient) OpenChannelSync(ctx context.Context, in *lnrpc.OpenChannelRequest, opts ...grpc.CallOption) (*lnrpc.ChannelPoint, error) {
	return new(lnrpc.ChannelPoint), nil
}

func (c *StubLightningClient) OpenChannel(ctx context.Context, in *lnrpc.OpenChannelRequest, opts ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient, error) {
	return new(StubLightningOpenChannelClient), nil
}

func (c *StubLightningClient) CloseChannel(ctx context.Context, in *lnrpc.CloseChannelRequest, opts ...grpc.CallOption) (lnrpc.Lightning_CloseChannelClient, error) {
	return &StubLightningCloseChannelClient{}, nil
}

func (c *StubLightningClient) SendPayment(ctx context.Context, opts ...grpc.CallOption) (lnrpc.Lightning_SendPaymentClient, error) {
	return new(StubLightningSendPaymentClient), nil
}

func (c *StubLightningClient) SendPaymentSync(ctx context.Context, in *lnrpc.SendRequest, opts ...grpc.CallOption) (*lnrpc.SendResponse, error) {
	return new(lnrpc.SendResponse), nil
}

func (c *StubLightningClient) AddInvoice(ctx context.Context, in *lnrpc.Invoice, opts ...grpc.CallOption) (*lnrpc.AddInvoiceResponse, error) {
	return new(lnrpc.AddInvoiceResponse), nil
}

func (c *StubLightningClient) ListInvoices(ctx context.Context, in *lnrpc.ListInvoiceRequest, opts ...grpc.CallOption) (*lnrpc.ListInvoiceResponse, error) {
	return new(lnrpc.ListInvoiceResponse), nil
}

func (c *StubLightningClient) LookupInvoice(ctx context.Context, in *lnrpc.PaymentHash, opts ...grpc.CallOption) (*lnrpc.Invoice, error) {
	return new(lnrpc.Invoice), nil
}

func (c *StubLightningClient) SubscribeInvoices(ctx context.Context, in *lnrpc.InvoiceSubscription, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeInvoicesClient, error) {
	return new(StubLightningSubscribeInvoicesClient), nil
}

func (c *StubLightningClient) DecodePayReq(ctx context.Context, in *lnrpc.PayReqString, opts ...grpc.CallOption) (*lnrpc.PayReq, error) {
	return new(lnrpc.PayReq), nil
}

func (c *StubLightningClient) ListPayments(ctx context.Context, in *lnrpc.ListPaymentsRequest, opts ...grpc.CallOption) (*lnrpc.ListPaymentsResponse, error) {
	return new(lnrpc.ListPaymentsResponse), nil
}

func (c *StubLightningClient) DeleteAllPayments(ctx context.Context, in *lnrpc.DeleteAllPaymentsRequest, opts ...grpc.CallOption) (*lnrpc.DeleteAllPaymentsResponse, error) {
	return new(lnrpc.DeleteAllPaymentsResponse), nil
}

func (c *StubLightningClient) DescribeGraph(ctx context.Context, in *lnrpc.ChannelGraphRequest, opts ...grpc.CallOption) (*lnrpc.ChannelGraph, error) {
	return new(lnrpc.ChannelGraph), nil
}

func (c *StubLightningClient) GetChanInfo(ctx context.Context, in *lnrpc.ChanInfoRequest, opts ...grpc.CallOption) (*lnrpc.ChannelEdge, error) {
	return new(lnrpc.ChannelEdge), nil
}

func (c *StubLightningClient) GetNodeInfo(ctx context.Context, in *lnrpc.NodeInfoRequest, opts ...grpc.CallOption) (*lnrpc.NodeInfo, error) {
	return new(lnrpc.NodeInfo), nil
}

func (c *StubLightningClient) QueryRoutes(ctx context.Context, in *lnrpc.QueryRoutesRequest, opts ...grpc.CallOption) (*lnrpc.QueryRoutesResponse, error) {
	return new(lnrpc.QueryRoutesResponse), nil
}

func (c *StubLightningClient) GetNetworkInfo(ctx context.Context, in *lnrpc.NetworkInfoRequest, opts ...grpc.CallOption) (*lnrpc.NetworkInfo, error) {
	return new(lnrpc.NetworkInfo), nil
}

func (c *StubLightningClient) StopDaemon(ctx context.Context, in *lnrpc.StopRequest, opts ...grpc.CallOption) (*lnrpc.StopResponse, error) {
	return new(lnrpc.StopResponse), nil
}

func (c *StubLightningClient) SubscribeChannelGraph(ctx context.Context, in *lnrpc.GraphTopologySubscription, opts ...grpc.CallOption) (lnrpc.Lightning_SubscribeChannelGraphClient, error) {
	return new(StubLightningSubscribeChannelGraphClient), nil
}

func (c *StubLightningClient) DebugLevel(ctx context.Context, in *lnrpc.DebugLevelRequest, opts ...grpc.CallOption) (*lnrpc.DebugLevelResponse, error) {
	return new(lnrpc.DebugLevelResponse), nil
}

func (c *StubLightningClient) FeeReport(ctx context.Context, in *lnrpc.FeeReportRequest, opts ...grpc.CallOption) (*lnrpc.FeeReportResponse, error) {
	return new(lnrpc.FeeReportResponse), nil
}

func (c *StubLightningClient) UpdateChannelPolicy(ctx context.Context, in *lnrpc.PolicyUpdateRequest, opts ...grpc.CallOption) (*lnrpc.PolicyUpdateResponse, error) {
	return new(lnrpc.PolicyUpdateResponse), nil
}
