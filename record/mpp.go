package record

import (
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

type MPP struct {
	TotalMsat   uint64
	PaymentAddr [32]byte
}

func NewMPP(total uint64, addr [32]byte) *MPP {
	return &MPP{
		TotalMsat:   total,
		PaymentAddr: addr,
	}
}

func MPPEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*MPP); ok {
		if err := tlv.EUint64T(w, v.TotalMsat, buf); err != nil {
			return err
		}
		return tlv.EBytes32(w, &v.PaymentAddr, buf)
	}
	return tlv.NewTypeForEncodingErr(val, "MPP")
}

func MPPDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*MPP); ok && l == 40 {
		if err := tlv.DUint64(r, &v.TotalMsat, buf, 8); err != nil {
			return err
		}
		return tlv.DBytes32(r, &v.PaymentAddr, buf, 32)
	}
	return tlv.NewTypeForDecodingErr(val, "MPP", l, 40)
}

func (r *MPP) TLV() tlv.Record {
	return tlv.MakeStaticRecord(9, r, 40, MPPEncoder, MPPDecoder)
}
