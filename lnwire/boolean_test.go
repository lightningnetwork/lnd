package lnwire

import (
	"bytes"
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestBooleanRecord tests the encoding and decoding of a boolean tlv record.
func TestBooleanRecord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		encodeFn     func(w *bytes.Buffer) error
		expectedBool bool
	}{
		{
			name:         "omitted boolean record",
			encodeFn:     encodedWireMsgOmitBool,
			expectedBool: false,
		},
		{
			name:         "zero length tlv",
			encodeFn:     encodedWireMsgZeroLenTrue,
			expectedBool: true,
		},
		{
			name:         "explicitly encoded false",
			encodeFn:     encodedWireMsgExplicitFalse,
			expectedBool: false,
		},
		{
			name:         "explicitly encoded true",
			encodeFn:     encodedWireMsgExplicitTrue,
			expectedBool: true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, test.encodeFn(&buf))

			msg := &wireMsg{}
			require.NoError(t, msg.decodeWireMsg(&buf))

			require.Equal(
				t, test.expectedBool, msg.DisableFlag.Val.B,
			)
		})
	}
}

type wireMsg struct {
	DisableFlag tlv.RecordT[tlv.TlvType2, Boolean]

	ExtraOpaqueData ExtraOpaqueData
}

func encodedWireMsgExplicitFalse(w *bytes.Buffer) error {
	disableFlag := tlv.ZeroRecordT[tlv.TlvType2, Boolean]()

	var b ExtraOpaqueData
	err := EncodeMessageExtraData(&b, &disableFlag)
	if err != nil {
		return err
	}

	return WriteBytes(w, b)
}

func encodedWireMsgExplicitTrue(w *bytes.Buffer) error {
	disableFlag := tlv.ZeroRecordT[tlv.TlvType2, bool]()
	disableFlag.Val = true

	var b ExtraOpaqueData
	err := EncodeMessageExtraData(&b, &disableFlag)
	if err != nil {
		return err
	}

	return WriteBytes(w, b)
}

func encodedWireMsgZeroLenTrue(w *bytes.Buffer) error {
	disableFlag := tlv.ZeroRecordT[tlv.TlvType2, Boolean]()
	disableFlag.Val.B = true

	var b ExtraOpaqueData
	err := EncodeMessageExtraData(&b, &disableFlag)
	if err != nil {
		return err
	}

	return WriteBytes(w, b)
}

func encodedWireMsgOmitBool(w *bytes.Buffer) error {
	var b ExtraOpaqueData
	err := EncodeMessageExtraData(&b)
	if err != nil {
		return err
	}

	return WriteBytes(w, b)
}

func (m *wireMsg) decodeWireMsg(r io.Reader) error {
	// First extract into extra opaque data.
	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	disableFlag := tlv.ZeroRecordT[tlv.TlvType2, Boolean]()

	typeMap, err := tlvRecords.ExtractRecords(&disableFlag)
	if err != nil {
		return err
	}

	if _, ok := typeMap[m.DisableFlag.TlvType()]; ok {
		m.DisableFlag = disableFlag
	}

	return nil
}
