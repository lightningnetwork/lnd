package signrpc

import (
	"fmt"

	"github.com/lightningnetwork/lnd/input"
)

// UnmarshalMuSig2Version parses the RPC MuSig2 version into the native
// counterpart.
func UnmarshalMuSig2Version(rpcVersion MuSig2Version) (input.MuSig2Version,
	error) {

	switch rpcVersion {
	case MuSig2Version_MUSIG2_VERSION_V040:
		return input.MuSig2Version040, nil

	case MuSig2Version_MUSIG2_VERSION_V100RC2:
		return input.MuSig2Version100RC2, nil

	default:
		return 0, fmt.Errorf("unknown MuSig2 version <%v>, make sure "+
			"your client software is up to date, the version "+
			"field is mandatory for this release of lnd",
			rpcVersion.String())
	}
}

// MarshalMuSig2Version turns the native MuSig2 version into its RPC
// counterpart.
func MarshalMuSig2Version(version input.MuSig2Version) (MuSig2Version, error) {
	switch version {
	case input.MuSig2Version040:
		return MuSig2Version_MUSIG2_VERSION_V040, nil

	case input.MuSig2Version100RC2:
		return MuSig2Version_MUSIG2_VERSION_V100RC2, nil

	default:
		return 0, fmt.Errorf("unknown MuSig2 version <%d>", version)
	}
}
