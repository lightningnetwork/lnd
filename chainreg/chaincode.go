package chainreg

// ChainCode is an enum-like structure for keeping track of the chains
// currently supported within lnd.
type ChainCode uint32

const (
	// BitcoinChain is Bitcoin's chain.
	BitcoinChain ChainCode = iota

	// LitecoinChain is Litecoin's chain.
	LitecoinChain
)

// String returns a string representation of the target ChainCode.
func (c ChainCode) String() string {
	switch c {
	case BitcoinChain:
		return "bitcoin"
	case LitecoinChain:
		return "litecoin"
	default:
		return "kekcoin"
	}
}
