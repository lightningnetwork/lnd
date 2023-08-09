package input

import (
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/lnutils"
)

// ErrUnknownScriptType is returned when an unknown script type is encountered.
var ErrUnknownScriptType = errors.New("unknown script type")

// ScriptPath is used to indicate the spending path of a given script. Possible
// paths include: timeout, success, revocation, and others.
type ScriptPath uint8

const (
	// ScriptPathTimeout is a script path that can be taken only after a
	// timeout has elapsed.
	ScriptPathTimeout ScriptPath = iota

	// ScriptPathSuccess is a script path that can be taken only with some
	// secret data.
	ScriptPathSuccess

	// ScriptPathRevocation is a script path used when a contract has been
	// breached.
	ScriptPathRevocation

	// ScriptPathDelay is a script path used when a contract has relative
	// delay that must elapse before it can be swept.
	ScriptPathDelay
)

// ScriptDesciptor is an interface that abstracts over the various ways a
// pkScript can be spent from an output. This supports both normal p2wsh
// (witness script, etc), and also tapscript paths which have distinct
// tapscript leaves.
type ScriptDescriptor interface {
	// PkScript is the public key script that commits to the final
	// contract.
	PkScript() []byte

	// WitnessScript returns the witness script that we'll use when signing
	// for the remote party, and also verifying signatures on our
	// transactions. As an example, when we create an outgoing HTLC for the
	// remote party, we want to sign their success path.
	//
	// TODO(roasbeef): break out into HTLC specific desc? or Branching Desc
	// w/ the below?
	WitnessScriptToSign() []byte

	// WitnessScriptForPath returns the witness script for the given
	// spending path. An error is returned if the path is unknown. This is
	// useful as when constructing a control block for a given path, one
	// also needs witness script being signed.
	WitnessScriptForPath(path ScriptPath) ([]byte, error)
}

// TapscriptDescriptor is a super-set of the normal script multiplexer that
// adds in taproot specific details such as the control block, or top-level tap
// tweak.
type TapscriptDescriptor interface {
	ScriptDescriptor

	// CtrlBlockForPath returns the control block for the given spending
	// path. For unknown paths, an error is returned.
	CtrlBlockForPath(path ScriptPath) (*txscript.ControlBlock, error)

	// TapTweak returns the top-level taproot tweak for the script.
	TapTweak() []byte

	// TapScriptTree returns the underlying tapscript tree.
	TapScriptTree() *txscript.IndexedTapScriptTree
}

// ScriptTree holds the contents needed to spend a script within a tapscript
// tree.
type ScriptTree struct {
	// InternalKey is the internal key of the Taproot output key.
	InternalKey *btcec.PublicKey

	// TaprootKey is the key that will be used to generate the taproot
	// output.
	TaprootKey *btcec.PublicKey

	// TapscriptTree is the full tapscript tree that also includes the
	// control block needed to spend each of the leaves.
	TapscriptTree *txscript.IndexedTapScriptTree

	// TapscriptTreeRoot is the root hash of the tapscript tree.
	TapscriptRoot []byte
}

// PkScript is the public key script that commits to the final contract.
func (s *ScriptTree) PkScript() []byte {
	// Script building can never internally return an error, so we ignore
	// the error to simplify the interface.
	pkScript, _ := PayToTaprootScript(s.TaprootKey)
	return pkScript
}

// TapTweak returns the top-level taproot tweak for the script.
func (s *ScriptTree) TapTweak() []byte {
	return lnutils.ByteSlice(s.TapscriptTree.RootNode.TapHash())
}

// TapScriptTree returns the underlying tapscript tree.
func (s *ScriptTree) TapScriptTree() *txscript.IndexedTapScriptTree {
	return s.TapscriptTree
}
