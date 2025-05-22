package sweep

import (
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrMaxPosition is returned when trying to increase the position of
	// the fee function while it's already at its max.
	ErrMaxPosition = errors.New("position already at max")

	// ErrZeroFeeRateDelta is returned when the fee rate delta is zero.
	ErrZeroFeeRateDelta = errors.New("fee rate delta is zero")
)

// mSatPerKWeight represents a fee rate in msat/kw.
type mSatPerKWeight lnwire.MilliSatoshi

// String returns a human-readable string of the fee rate.
func (m mSatPerKWeight) String() string {
	s := lnwire.MilliSatoshi(m)
	return fmt.Sprintf("%v/kw", s)
}

// FeeFunction defines an interface that is used to calculate fee rates for transactions.
type FeeFunction interface {
	FeeRate() chainfee.SatPerKWeight
	Increment() (bool, error)
	IncreaseFeeRate(confTarget uint32) (bool, error)
}

// LinearFeeFunction implements a linear fee function: feeRate = baseFeeRate + position * delta.
type LinearFeeFunction struct {
	baseFeeRate    chainfee.SatPerKWeight
	endingFeeRate  chainfee.SatPerKWeight
	currentFeeRate chainfee.SatPerKWeight
	width          uint32
	position       uint32
	deltaFeeRate   mSatPerKWeight
	estimator      chainfee.Estimator
}

var _ FeeFunction = (*LinearFeeFunction)(nil)

func NewLinearFeeFunction(maxFeeRate, baseFeeRate chainfee.SatPerKWeight,
	confTarget uint32, estimator chainfee.Estimator) (*LinearFeeFunction, error) {

	if confTarget <= 1 {
		return &LinearFeeFunction{
			baseFeeRate:    maxFeeRate,
			endingFeeRate:  maxFeeRate,
			currentFeeRate: maxFeeRate,
		}, nil
	}

	l := &LinearFeeFunction{
		baseFeeRate:   baseFeeRate,
		endingFeeRate: maxFeeRate,
		width:         confTarget - 1,
		estimator:     estimator,
	}

	delta := btcutil.Amount(l.endingFeeRate-l.baseFeeRate).MulF64(1000/float64(l.width))
	l.deltaFeeRate = mSatPerKWeight(delta)

	if l.deltaFeeRate == 0 && l.width != 1 {
		log.Errorf("Failed to init linear fee function: baseFeeRate=%v, endingFeeRate=%v, width=%v, delta=%v",
			l.baseFeeRate, l.endingFeeRate, l.width, l.deltaFeeRate)
		return nil, ErrZeroFeeRateDelta
	}

	l.currentFeeRate = l.baseFeeRate
	log.Debugf("Linear fee function initialized: baseFeeRate=%v, endingFeeRate=%v, width=%v, delta=%v",
		l.baseFeeRate, l.endingFeeRate, l.width, l.deltaFeeRate)

	return l, nil
}

func (l *LinearFeeFunction) FeeRate() chainfee.SatPerKWeight {
	return l.currentFeeRate
}

func (l *LinearFeeFunction) Increment() (bool, error) {
	return l.increaseFeeRate(l.position + 1)
}

func (l *LinearFeeFunction) IncreaseFeeRate(confTarget uint32) (bool, error) {
	newPosition := uint32(0)
	if confTarget < l.width+1 {
		newPosition = l.width + 1 - confTarget
		log.Tracef("Increasing position from %v to %v", l.position, newPosition)
	}

	if newPosition <= l.position {
		log.Tracef("Skipped increase feerate: position=%v, newPosition=%v", l.position, newPosition)
		return false, nil
	}

	return l.increaseFeeRate(newPosition)
}

func (l *LinearFeeFunction) increaseFeeRate(position uint32) (bool, error) {
	if l.position >= l.width {
		return false, ErrMaxPosition
	}

	oldFeeRate := l.currentFeeRate
	l.position = position
	l.currentFeeRate = l.feeRateAtPosition(position)

	log.Tracef("Fee rate increased from %v to %v at position %v", oldFeeRate, l.currentFeeRate, l.position)
	return l.currentFeeRate > oldFeeRate, nil
}

func (l *LinearFeeFunction) feeRateAtPosition(p uint32) chainfee.SatPerKWeight {
	if p >= l.width {
		return l.endingFeeRate
	}

	feeRateDelta := btcutil.Amount(l.deltaFeeRate).MulF64(float64(p) / 1000)
	feeRate := l.baseFeeRate + chainfee.SatPerKWeight(feeRateDelta)
	if feeRate > l.endingFeeRate {
		return l.endingFeeRate
	}
	return feeRate
}

// CubicDelayFeeFunction implements a cubic delay fee function: feeRate = baseFeeRate + (position/width)^3 * (endingFeeRate - baseFeeRate).
type CubicDelayFeeFunction struct {
	baseFeeRate    chainfee.SatPerKWeight
	endingFeeRate  chainfee.SatPerKWeight
	currentFeeRate chainfee.SatPerKWeight
	width          uint32
	position       uint32
	estimator      chainfee.Estimator
}

var _ FeeFunction = (*CubicDelayFeeFunction)(nil)

func NewCubicDelayFeeFunction(maxFeeRate, baseFeeRate chainfee.SatPerKWeight,
	confTarget uint32, estimator chainfee.Estimator) (*CubicDelayFeeFunction, error) {

	if confTarget <= 1 {
		return &CubicDelayFeeFunction{
			baseFeeRate:    maxFeeRate,
			endingFeeRate:  maxFeeRate,
			currentFeeRate: maxFeeRate,
		}, nil
	}

	c := &CubicDelayFeeFunction{
		baseFeeRate:   baseFeeRate,
		endingFeeRate: maxFeeRate,
		width:         confTarget - 1,
		estimator:     estimator,
	}

	c.currentFeeRate = c.baseFeeRate
	log.Debugf("Cubic delay fee function initialized: baseFeeRate=%v, endingFeeRate=%v, width=%v",
		c.baseFeeRate, c.endingFeeRate, c.width)

	return c, nil
}

func (c *CubicDelayFeeFunction) FeeRate() chainfee.SatPerKWeight {
	return c.currentFeeRate
}

func (c *CubicDelayFeeFunction) Increment() (bool, error) {
	return c.increaseFeeRate(c.position + 1)
}

func (c *CubicDelayFeeFunction) IncreaseFeeRate(confTarget uint32) (bool, error) {
	newPosition := uint32(0)
	if confTarget < c.width+1 {
		newPosition = c.width + 1 - confTarget
		log.Tracef("Increasing position from %v to %v", c.position, newPosition)
	}

	if newPosition <= c.position {
		log.Tracef("Skipped increase feerate: position=%v, newPosition=%v", c.position, newPosition)
		return false, nil
	}

	return c.increaseFeeRate(newPosition)
}

func (c *CubicDelayFeeFunction) increaseFeeRate(position uint32) (bool, error) {
	if c.position >= c.width {
		return false, ErrMaxPosition
	}

	oldFeeRate := c.currentFeeRate
	c.position = position
	c.currentFeeRate = c.feeRateAtPosition(position)

	log.Tracef("Fee rate increased from %v to %v at position %v", oldFeeRate, c.currentFeeRate, c.position)
	return c.currentFeeRate > oldFeeRate, nil
}

func (c *CubicDelayFeeFunction) feeRateAtPosition(p uint32) chainfee.SatPerKWeight {
	if p >= c.width {
		return c.endingFeeRate
	}

	ratio := math.Pow(float64(p)/float64(c.width), 3)
	feeDelta := float64(c.endingFeeRate-c.baseFeeRate) * ratio
	feeRate := c.baseFeeRate + chainfee.SatPerKWeight(feeDelta)
	if feeRate > c.endingFeeRate {
		return c.endingFeeRate
	}
	return feeRate
}

// CubicEagerFeeFunction implements a cubic eager fee function: feeRate = baseFeeRate + (1 + ((position/width) - 1)^3) * (endingFeeRate - baseFeeRate).
type CubicEagerFeeFunction struct {
	baseFeeRate    chainfee.SatPerKWeight
	endingFeeRate  chainfee.SatPerKWeight
	currentFeeRate chainfee.SatPerKWeight
	width          uint32
	position       uint32
	estimator      chainfee.Estimator
}

var _ FeeFunction = (*CubicEagerFeeFunction)(nil)

func NewCubicEagerFeeFunction(maxFeeRate, baseFeeRate chainfee.SatPerKWeight,
	confTarget uint32, estimator chainfee.Estimator) (*CubicEagerFeeFunction, error) {

	if confTarget <= 1 {
		return &CubicEagerFeeFunction{
			baseFeeRate:    maxFeeRate,
			endingFeeRate:  maxFeeRate,
			currentFeeRate: maxFeeRate,
		}, nil
	}

	c := &CubicEagerFeeFunction{
		baseFeeRate:   baseFeeRate,
		endingFeeRate: maxFeeRate,
		width:         confTarget - 1,
		estimator:     estimator,
	}

	c.currentFeeRate = c.baseFeeRate
	log.Debugf("Cubic eager fee function initialized: baseFeeRate=%v, endingFeeRate=%v, width=%v",
		c.baseFeeRate, c.endingFeeRate, c.width)

	return c, nil
}

func (c *CubicEagerFeeFunction) FeeRate() chainfee.SatPerKWeight {
	return c.currentFeeRate
}

func (c *CubicEagerFeeFunction) Increment() (bool, error) {
	return c.increaseFeeRate(c.position + 1)
}

func (c *CubicEagerFeeFunction) IncreaseFeeRate(confTarget uint32) (bool, error) {
	newPosition := uint32(0)
	if confTarget < c.width+1 {
		newPosition = c.width + 1 - confTarget
		log.Tracef("Increasing position from %v to %v", c.position, newPosition)
	}

	if newPosition <= c.position {
		log.Tracef("Skipped increase feerate: position=%v, newPosition=%v", c.position, newPosition)
		return false, nil
	}

	return c.increaseFeeRate(newPosition)
}

func (c *CubicEagerFeeFunction) increaseFeeRate(position uint32) (bool, error) {
	if c.position >= c.width {
		return false, ErrMaxPosition
	}

	oldFeeRate := c.currentFeeRate
	c.position = position
	c.currentFeeRate = c.feeRateAtPosition(position)

	log.Tracef("Fee rate increased from %v to %v at position %v", oldFeeRate, c.currentFeeRate, c.position)
	return c.currentFeeRate > oldFeeRate, nil
}

func (c *CubicEagerFeeFunction) feeRateAtPosition(p uint32) chainfee.SatPerKWeight {
	if p >= c.width {
		return c.endingFeeRate
	}

	x := float64(p) / float64(c.width)
	ratio := 1 + math.Pow(x-1, 3)
	feeDelta := float64(c.endingFeeRate-c.baseFeeRate) * ratio
	feeRate := c.baseFeeRate + chainfee.SatPerKWeight(feeDelta)
	if feeRate > c.endingFeeRate {
		return c.endingFeeRate
	}
	return feeRate
}
