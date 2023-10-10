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
