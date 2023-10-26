package lnwire

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// MsgHashTag is will prefix the message name and the field name in order to
// construct the message tag.
const MsgHashTag = "lightning"

func MsgTag(msgName, fieldName string) []byte {
	tag := []byte(MsgHashTag)
	tag = append(tag, []byte(msgName)...)

	return append(tag, []byte(fieldName)...)
}

// MsgHash computes the tagged hash of the given message as follows:
//
//	tag = "lightning"||"msg_name"||"field_name"
//	hash = sha256(sha246(tag) || sha256(tag) || msg)
func MsgHash(msgName, fieldName string, msg []byte) *chainhash.Hash {
	tag := MsgTag(msgName, fieldName)

	return chainhash.TaggedHash(tag, msg)
}
