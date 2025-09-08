package paymentsdb

const (
	// DefaultMaxPayments is the default maximum number of payments returned
	// in the payments query pagination.
	DefaultMaxPayments = 100
)

// Query represents a query to the payments database starting or ending
// at a certain offset index. The number of retrieved records can be limited.
type Query struct {
	// IndexOffset determines the starting point of the payments query and
	// is always exclusive. In normal order, the query starts at the next
	// higher (available) index compared to IndexOffset. In reversed order,
	// the query ends at the next lower (available) index compared to the
	// IndexOffset. In the case of a zero index_offset, the query will start
	// with the oldest payment when paginating forwards, or will end with
	// the most recent payment when paginating backwards.
	IndexOffset uint64

	// MaxPayments is the maximal number of payments returned in the
	// payments query.
	MaxPayments uint64

	// Reversed gives a meaning to the IndexOffset. If reversed is set to
	// true, the query will fetch payments with indices lower than the
	// IndexOffset, otherwise, it will return payments with indices greater
	// than the IndexOffset.
	Reversed bool

	// If IncludeIncomplete is true, then return payments that have not yet
	// fully completed. This means that pending payments, as well as failed
	// payments will show up if this field is set to true.
	IncludeIncomplete bool

	// CountTotal indicates that all payments currently present in the
	// payment index (complete and incomplete) should be counted.
	CountTotal bool

	// CreationDateStart, expressed in Unix seconds, if set, filters out
	// all payments with a creation date greater than or equal to it.
	CreationDateStart int64

	// CreationDateEnd, expressed in Unix seconds, if set, filters out all
	// payments with a creation date less than or equal to it.
	CreationDateEnd int64
}

// Response contains the result of a query to the payments database.
// It includes the set of payments that match the query and integers which
// represent the index of the first and last item returned in the series of
// payments. These integers allow callers to resume their query in the event
// that the query's response exceeds the max number of returnable events.
type Response struct {
	// Payments is the set of payments returned from the database for the
	// Query.
	Payments []*MPPayment

	// FirstIndexOffset is the index of the first element in the set of
	// returned MPPayments. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response. The offset can be used to continue reverse pagination.
	FirstIndexOffset uint64

	// LastIndexOffset is the index of the last element in the set of
	// returned MPPayments. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response. The offset can be used to continue forward pagination.
	LastIndexOffset uint64

	// TotalCount represents the total number of payments that are currently
	// stored in the payment database. This will only be set if the
	// CountTotal field in the query was set to true.
	TotalCount uint64
}
