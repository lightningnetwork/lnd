package lnwire

import (
	"encoding/binary"
	"io"
	"math"

	"github.com/go-errors/errors"
)

// featureFlag represent the status of the feature optional/required and needed
// to allow future incompatible changes, or backward compatible changes.
type featureFlag uint8

// String returns the string representation for the featureFlag.
func (f featureFlag) String() string {
	switch f {
	case OptionalFlag:
		return "optional"
	case RequiredFlag:
		return "required"
	default:
		return "<unknown>"
	}
}

// featureName represent the name of the feature and needed in order to have the
// compile errors if we specify wrong feature name.
type featureName string

const (
	// OptionalFlag represent the feature which we already have but it isn't
	// required yet, and if remote peer doesn't have this feature we may
	// turn it off without disconnecting with peer.
	OptionalFlag featureFlag = 2 // 0b10

	// RequiredFlag represent the features which is required for proper
	// peer interaction, we disconnect with peer if it doesn't have this
	// particular feature.
	RequiredFlag featureFlag = 1 // 0b01

	// flagMask is a mask which is needed to extract feature flag value.
	flagMask = 3 // 0b11

	// flagBitsSize represent the size of the feature flag in bits. For
	// more information read the init message specification.
	flagBitsSize = 2

	// maxAllowedSize is a maximum allowed size of feature vector.
	// NOTE: Within the protocol, the maximum allowed message size is 65535
	// bytes. Adding the overhead from the crypto protocol (the 2-byte packet
	// length and 16-byte MAC), we arrive at 65569 bytes. Accounting for the
	// overhead within the feature message to signal the type of the message,
	// that leaves 65567 bytes for the init message itself. Next, we reserve
	// 4-bytes to encode the lengths of both the local and global feature
	// vectors, so 65563 for the global and local features. Knocking off one
	// byte for the sake of the calculation, that leads to a max allowed
	// size of 32781 bytes for each feature vector, or 131124 different
	// features.
	maxAllowedSize = 32781
)

// Feature represent the feature which is used on stage of initialization of
// feature vector. Initial feature flags might be changed dynamically later.
type Feature struct {
	Name featureName
	Flag featureFlag
}

// FeatureVector represents the global/local feature vector. With this structure
// you may set/get the feature by name and compare feature vector with remote
// one.
type FeatureVector struct {
	// featuresMap is the map which stores the correspondence between
	// feature name and its index within feature vector. Index within
	// feature vector and actual binary position of feature
	// are different things)
	featuresMap map[featureName]int // name -> index

	// flags is the map which stores the correspondence between feature
	// index and its flag.
	flags map[int]featureFlag // index -> flag
}

// NewFeatureVector creates new instance of feature vector.
func NewFeatureVector(features []Feature) *FeatureVector {
	featuresMap := make(map[featureName]int)
	flags := make(map[int]featureFlag)

	for index, feature := range features {
		featuresMap[feature.Name] = index
		flags[index] = feature.Flag
	}

	return &FeatureVector{
		featuresMap: featuresMap,
		flags:       flags,
	}
}

// SetFeatureFlag assign flag to the feature.
func (f *FeatureVector) SetFeatureFlag(name featureName, flag featureFlag) error {
	position, ok := f.featuresMap[name]
	if !ok {
		return errors.Errorf("can't find feature with name: %v", name)
	}

	f.flags[position] = flag
	return nil
}

// serializedSize returns the number of bytes which is needed to represent
// feature vector in byte format.
func (f *FeatureVector) serializedSize() uint16 {
	return uint16(math.Ceil(float64(flagBitsSize*len(f.flags)) / 8))
}

// NewFeatureVectorFromReader decodes the feature vector from binary
// representation and creates the instance of it.
// Every feature decoded as 2 bits where odd bit determine whether the feature
// is "optional" and even bit told us whether the feature is "required". The
// even/odd semantic allows future incompatible changes, or backward compatible
// changes. Bits generally assigned in pairs, so that optional features can
// later become compulsory.
func NewFeatureVectorFromReader(r io.Reader) (*FeatureVector, error) {
	f := &FeatureVector{
		flags: make(map[int]featureFlag),
	}

	getFlag := func(data []byte, position int) featureFlag {
		byteNumber := uint(position / 8)
		bitNumber := uint(position % 8)

		return featureFlag((data[byteNumber] >> bitNumber) & flagMask)
	}

	// Read the length of the feature vector.
	var l [2]byte
	if _, err := r.Read(l[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint16(l[:])

	// Read the feature vector data.
	data := make([]byte, length)
	if _, err := r.Read(data); err != nil {
		return nil, err
	}

	// Initialize feature vector.
	bitsNumber := len(data) * 8
	for position := 0; position <= bitsNumber-flagBitsSize; position += flagBitsSize {
		flag := getFlag(data, position)
		switch flag {
		case OptionalFlag, RequiredFlag:
			// Every feature/flag takes 2 bits, so in order to get
			// the feature/flag index we should divide position
			// on 2.
			index := position / flagBitsSize
			f.flags[index] = flag
		default:
			continue
		}
	}

	return f, nil
}

// Encode encodes the features vector into bytes representation, every
// feature encoded as 2 bits where odd bit determine whether the feature is
// "optional" and even bit told us whether the feature is "required". The
// even/odd semantic allows future incompatible changes, or backward compatible
// changes. Bits generally assigned in pairs, so that optional features can
// later become compulsory.
func (f *FeatureVector) Encode(w io.Writer) error {
	setFlag := func(data []byte, position int, flag featureFlag) {
		byteNumber := uint(position / 8)
		bitNumber := uint(position % 8)

		data[byteNumber] |= (byte(flag) << bitNumber)
	}

	// Write length of feature vector.
	var l [2]byte
	length := f.serializedSize()
	binary.BigEndian.PutUint16(l[:], length)
	if _, err := w.Write(l[:]); err != nil {
		return err
	}

	// Generate the data and write it.
	data := make([]byte, length)
	for index, flag := range f.flags {
		// Every feature takes 2 bits, so in order to get the
		// feature bits position we should multiply index by 2.
		position := index * flagBitsSize
		setFlag(data, position, flag)
	}

	_, err := w.Write(data)
	return err
}

// Compare checks that features are compatible and returns the features which
// were present in both remote and local feature vectors. If remote/local node
// doesn't have the feature and local/remote node require it than such vectors
// are incompatible.
func (f *FeatureVector) Compare(f2 *FeatureVector) (*SharedFeatures, error) {
	shared := newSharedFeatures(f.Copy())

	for index, flag := range f.flags {
		if _, exist := f2.flags[index]; !exist {
			switch flag {
			case RequiredFlag:
				return nil, errors.New("Remote node hasn't " +
					"locally required feature")
			case OptionalFlag:
				// If feature is optional and remote side
				// haven't it than it might be safely disabled.
				delete(shared.flags, index)
				continue
			}
		}

		// If feature exists on both sides than such feature might be
		// considered as active.
		shared.flags[index] = flag
	}

	for index, flag := range f2.flags {
		if _, exist := f.flags[index]; !exist {
			switch flag {
			case RequiredFlag:
				return nil, errors.New("Local node hasn't " +
					"locally required feature")
			case OptionalFlag:
				// If feature is optional and local side
				// haven't it than it might be safely disabled.
				delete(shared.flags, index)
				continue
			}
		}

		// If feature exists on both sides than such feature might be
		// considered as active.
		shared.flags[index] = flag
	}

	return shared, nil
}

// Copy generate new distinct instance of the feature vector.
func (f *FeatureVector) Copy() *FeatureVector {
	features := make([]Feature, len(f.featuresMap))

	for name, index := range f.featuresMap {
		features[index] = Feature{
			Name: name,
			Flag: f.flags[index],
		}
	}

	return NewFeatureVector(features)
}

// SharedFeatures is a product of comparison of two features vector
// which consist of features which are present in both local and remote
// features vectors.
type SharedFeatures struct {
	*FeatureVector
}

// newSharedFeatures creates new shared features instance.
func newSharedFeatures(f *FeatureVector) *SharedFeatures {
	return &SharedFeatures{f}
}

// IsActive checks is feature active or not, it might be disabled during
// comparision with remote feature vector if it was optional and
// remote peer doesn't support it.
func (f *SharedFeatures) IsActive(name featureName) bool {
	index, ok := f.featuresMap[name]
	if !ok {
		// If we even have no such feature in feature map, than it
		// can't be active in any circumstances.
		return false
	}

	_, exist := f.flags[index]
	return exist
}
