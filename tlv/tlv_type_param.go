package tlv

import "fmt"

// TlvType is an interface used to enable binding the integer type of a TLV
// record to the type at compile time.
type TlvType interface {
	// TypeVal returns the integer TLV type that this TlvType struct
	// instance maps to.
	TypeVal() Type

	// tlv is an internal method to make this a "sealed" interface, meaning
	// only this package can declare new instances.
	tlv()
}

//go:generate go run internal/gen/gen_tlv_types.go -o tlv_types_generated.go

func main() {
	// This function is only here to satisfy the go:generate directive.
	fmt.Println("Generating TLV type structures...")
}
