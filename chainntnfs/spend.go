package chainntnfs

// SpendFinality validates and applies a confirmation requirement.
type SpendFinality struct {
	numConfs uint32
}

// NewSpendFinality constructs a validated spend finality policy.
func NewSpendFinality(numConfs uint32) (*SpendFinality, error) {
	if numConfs == 0 || numConfs > MaxNumConfs {
		return nil, ErrNumConfsOutOfRange
	}

	return &SpendFinality{numConfs: numConfs}, nil
}

// NumConfs returns the confirmation requirement.
func (s *SpendFinality) NumConfs() uint32 {
	return s.numConfs
}

// IsFinal reports whether a spend has reached the policy's confirmation
// requirement at currentHeight.
func (s *SpendFinality) IsFinal(spendHeight, currentHeight int32) bool {
	if currentHeight < spendHeight {
		return false
	}

	confirmations := uint64(int64(currentHeight)-int64(spendHeight)) + 1

	return confirmations >= uint64(s.numConfs)
}
