package invoices

import (
	"context"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/mock"
)

type MockInvoiceDB struct {
	mock.Mock
}

func NewInvoicesDBMock() *MockInvoiceDB {
	return &MockInvoiceDB{}
}

func (m *MockInvoiceDB) AddInvoice(invoice *Invoice,
	paymentHash lntypes.Hash) (uint64, error) {

	args := m.Called(invoice, paymentHash)

	addIndex, _ := args.Get(0).(uint64)

	// NOTE: this is a side effect of the AddInvoice method.
	invoice.AddIndex = addIndex

	return addIndex, args.Error(1)
}

func (m *MockInvoiceDB) InvoicesAddedSince(idx uint64) ([]Invoice, error) {
	args := m.Called(idx)
	invoices, _ := args.Get(0).([]Invoice)

	return invoices, args.Error(1)
}

func (m *MockInvoiceDB) InvoicesSettledSince(idx uint64) ([]Invoice, error) {
	args := m.Called(idx)
	invoices, _ := args.Get(0).([]Invoice)

	return invoices, args.Error(1)
}

func (m *MockInvoiceDB) LookupInvoice(ref InvoiceRef) (Invoice, error) {
	args := m.Called(ref)
	invoice, _ := args.Get(0).(Invoice)

	return invoice, args.Error(1)
}

func (m *MockInvoiceDB) FetchPendingInvoices(ctx context.Context) (
	map[lntypes.Hash]Invoice, error) {

	args := m.Called(ctx)
	return args.Get(0).(map[lntypes.Hash]Invoice), args.Error(1)
}

func (m *MockInvoiceDB) QueryInvoices(q InvoiceQuery) (InvoiceSlice, error) {
	args := m.Called(q)
	invoiceSlice, _ := args.Get(0).(InvoiceSlice)

	return invoiceSlice, args.Error(1)
}

func (m *MockInvoiceDB) UpdateInvoice(ref InvoiceRef, setIDHint *SetID,
	callback InvoiceUpdateCallback) (*Invoice, error) {

	args := m.Called(ref, setIDHint, callback)
	invoice, _ := args.Get(0).(*Invoice)

	return invoice, args.Error(1)
}

func (m *MockInvoiceDB) DeleteInvoice(invoices []InvoiceDeleteRef) error {
	args := m.Called(invoices)

	return args.Error(0)
}

func (m *MockInvoiceDB) DeleteCanceledInvoices(ctx context.Context) error {
	args := m.Called(ctx)

	return args.Error(0)
}

// MockHtlcModifier is a mock implementation of the HtlcModifier interface.
type MockHtlcModifier struct {
	mock.Mock
}

// Intercept generates a new intercept session for the given invoice.
// The call blocks until the client has responded to the request or an
// error occurs. The response callback is only called if a session was
// created in the first place, which is only the case if a client is
// registered.
func (m *MockHtlcModifier) Intercept(
	req HtlcModifyRequest, callback func(HtlcModifyResponse)) error {

	// If no expectations are set, return nil by default.
	if len(m.ExpectedCalls) == 0 {
		return nil
	}

	args := m.Called(req, callback)

	// If a response was provided to the mock, execute the callback with it.
	if response, ok := args.Get(1).(HtlcModifyResponse); ok &&
		callback != nil {

		callback(response)
	}

	return args.Error(0)
}

// RegisterInterceptor sets the client callback function that will be
// called when an invoice is intercepted. If a callback is already set,
// an error is returned. The returned function must be used to reset the
// callback to nil once the client is done or disconnects. The read-only channel
// closes when the server stops.
func (m *MockHtlcModifier) RegisterInterceptor(HtlcModifyCallback) (func(),
	<-chan struct{}, error) {

	return func() {}, make(chan struct{}), nil
}

// Ensure that MockHtlcModifier implements the HtlcInterceptor and HtlcModifier
// interfaces.
var _ HtlcInterceptor = (*MockHtlcModifier)(nil)
var _ HtlcModifier = (*MockHtlcModifier)(nil)
