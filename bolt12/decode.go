package bolt12

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/tlv"
)

// decodeStream runs a single typed-stream pass over data and returns the
// canonical TypeMap. Records may be passed in any order; NewStream requires
// them sorted, so SortRecords runs first.
func decodeStream(data []byte, records ...tlv.Record) (tlv.TypeMap, error) {
	tlv.SortRecords(records)

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("create stream: %w", err)
	}

	typeMap, err := stream.DecodeWithParsedTypesP2P(
		bytes.NewReader(data),
	)
	if err != nil {
		return nil, fmt.Errorf("decode stream: %w", err)
	}

	return typeMap, nil
}
