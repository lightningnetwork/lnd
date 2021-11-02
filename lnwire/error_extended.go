package lnwire

import (
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// typeErrorCode contains an error code, and additional nested tlvs
	// that provide more context for the error.
	typeErrorCode tlv.Type = 1

	// The following TLVs are _nested_ within the typeErrorCode tlv to
	// provide more context for errors.
	typeNestedCommitHeight   tlv.Type = 1
	typeNestedCommitSig      tlv.Type = 3
	typeNestedSigHash        tlv.Type = 5
	typeNestedCommitTx       tlv.Type = 7
	typeNestedHtlcSig        tlv.Type = 9
	typeNestedHtlcIndex      tlv.Type = 11
	typeNestedErroneousField tlv.Type = 13
	typeNestedSuggestedValue tlv.Type = 15
)

// ExtendedError is an interface implemented by any error that adds more
// information using tlv values.
type ExtendedError interface {
	// Record provides a tlv record which contains the additional
	// information for the error.
	Record() tlv.Record

	error
}

// WireErrorFromExtended creates a wire error with additional fields packed into
// its tlvs.
func WireErrorFromExtended(extendedError ExtendedError,
	chanID ChannelID) (*Error, error) {

	resp := &Error{
		ChanID: chanID,
		Data:   ErrorData(extendedError.Error()),
	}

	records := []tlv.RecordProducer{
		extendedError,
	}

	if err := resp.ExtraData.PackRecords(records...); err != nil {
		return nil, err
	}

	return resp, nil
}

// ExtendedErrorFromWire extracts an enriched error from our error's extra
// data, if present.
func ExtendedErrorFromWire(err *Error) (ExtendedError, error) {
	if err == nil {
		return nil, nil
	}

	if len(err.ExtraData) == 0 {
		return nil, nil
	}

	// Try to extract coded error, if present.
	codedError := &CodedError{}
	records := []tlv.RecordProducer{
		codedError,
	}

	extractedTypes, extractErr := err.ExtraData.ExtractRecords(records...)
	if extractErr != nil {
		return nil, extractErr
	}

	// If we extracted an error code tlv, return the coded error that we
	// read the record values in.
	if val, ok := extractedTypes[typeErrorCode]; ok && val == nil {
		return codedError, nil
	}

	// Otherwise, return nil because there is no additional information
	// included in this error that we understand.
	return nil, nil
}
