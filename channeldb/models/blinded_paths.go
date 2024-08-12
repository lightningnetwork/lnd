package models

import (
	"bytes"
	"errors"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	blindedPathInfoRouteType      tlv.Type = 0
	blindedPathInfoSessionKeyType tlv.Type = 1
)

// BlindedPathsInfo is a map from an incoming blinding key to the associated
// blinded path info. An invoice may contain multiple blinded paths but each one
// will have a unique session key and thus a unique final ephemeral key. One
// receipt of a payment along a blinded path, we can use the incoming blinding
// key to thus identify which blinded path in the invoice was used.
type BlindedPathsInfo map[route.Vertex]*BlindedPathInfo

// BlindedPathInfoEncoder is a custom TLV encoder for a BlindedPathInfo
// record.
func BlindedPathInfoEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*BlindedPathsInfo); ok {
		for key, info := range *v {
			// Write 33 byte key.
			if err := wire.WriteVarBytes(w, 0, key[:]); err != nil {
				return err
			}

			// Serialise the info.
			var infoBytes bytes.Buffer
			err := info.Serialize(&infoBytes)
			if err != nil {
				return err
			}

			// Write the length of the serialised info.
			err = wire.WriteVarInt(w, 0, uint64(infoBytes.Len()))
			if err != nil {
				return err
			}

			// Finally, write the serialise info.
			if _, err := w.Write(infoBytes.Bytes()); err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "*BlindedPathsInfo")
}

// BlindedPathInfoDecoder is a custom TLV decoder for a BlindedPathInfo
// record.
func BlindedPathInfoDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*BlindedPathsInfo); ok {
		for {
			key, err := wire.ReadVarBytes(r, 0, 66000, "[]byte")
			switch {
			// We'll silence an EOF when zero bytes remain, meaning
			// the stream was cleanly encoded.
			case errors.Is(err, io.EOF):
				return nil

			// Other unexpected errors.
			case err != nil:
				return err
			}

			// Read the length of the serialised info.
			infoLen, err := wire.ReadVarInt(r, 0)
			if err != nil {
				return err
			}

			// Create a limited reader using the info length.
			lr := io.LimitReader(r, int64(infoLen))

			// Deserialize the path info using the limited reader.
			info, err := DeserializeBlindedPathInfo(lr)
			if err != nil {
				return err
			}

			var k route.Vertex
			copy(k[:], key)

			(*v)[k] = info
		}
	}

	return tlv.NewTypeForDecodingErr(val, "*BlindedPathsInfo", l, l)
}

// BlindedPathInfo holds information we may need regarding a blinded path
// included in an invoice.
type BlindedPathInfo struct {
	// Route is the real route of the blinded path.
	Route *MCRoute

	// SessionKey is the private key used as the first ephemeral key of the
	// path. We can use this key to decrypt any data we encrypted for the
	// path.
	SessionKey *btcec.PrivateKey
}

// Copy makes a deep copy of the BlindedPathInfo.
func (i *BlindedPathInfo) Copy() *BlindedPathInfo {
	hops := make([]*MCHop, len(i.Route.Hops))
	for i, hop := range i.Route.Hops {
		hops[i] = &MCHop{
			ChannelID:        hop.ChannelID,
			AmtToFwd:         hop.AmtToFwd,
			HasBlindingPoint: hop.HasBlindingPoint,
		}

		copy(hops[i].PubKeyBytes[:], hop.PubKeyBytes[:])
	}

	r := &MCRoute{
		TotalAmount: i.Route.TotalAmount,
		Hops:        hops,
	}
	copy(r.SourcePubKey[:], i.Route.SourcePubKey[:])

	return &BlindedPathInfo{
		Route:      r,
		SessionKey: btcec.PrivKeyFromScalar(&i.SessionKey.Key),
	}
}

// Serialize serializes the BlindedPathInfo into a TLV stream and writes the
// resulting bytes to the given io.Writer.
func (i *BlindedPathInfo) Serialize(w io.Writer) error {
	var routeBuffer bytes.Buffer
	if err := i.Route.Serialize(&routeBuffer); err != nil {
		return err
	}

	var (
		privKeyBytes = i.SessionKey.Serialize()
		routeBytes   = routeBuffer.Bytes()
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			blindedPathInfoRouteType, &routeBytes,
		),
		tlv.MakePrimitiveRecord(
			blindedPathInfoSessionKeyType, &privKeyBytes,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// DeserializeBlindedPathInfo attempts to deserialize a BlindedPathInfo from the
// given io.Reader.
func DeserializeBlindedPathInfo(r io.Reader) (*BlindedPathInfo, error) {
	var (
		privKeyBytes []byte
		routeBytes   []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			blindedPathInfoRouteType, &routeBytes,
		),
		tlv.MakePrimitiveRecord(
			blindedPathInfoSessionKeyType, &privKeyBytes,
		),
	)
	if err != nil {
		return nil, err
	}

	if err := stream.Decode(r); err != nil {
		return nil, err
	}

	routeReader := bytes.NewReader(routeBytes)
	mcRoute, err := DeserializeRoute(routeReader)
	if err != nil {
		return nil, err
	}

	sessionKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)
	if err != nil {
		return nil, err
	}

	return &BlindedPathInfo{
		Route:      mcRoute,
		SessionKey: sessionKey,
	}, nil
}
