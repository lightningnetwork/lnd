// NOTE: forcetypeassert is skipped for the mock because the test would fail if
// the returned value doesn't match the type.
package lnmock

import (
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/mock"
)

// MockClock implements the `clock.Clock` interface.
type MockClock struct {
	mock.Mock
}

// Compile time assertion that MockClock implements clock.Clock.
var _ clock.Clock = (*MockClock)(nil)

func (m *MockClock) Now() time.Time {
	args := m.Called()

	return args.Get(0).(time.Time)
}

func (m *MockClock) TickAfter(d time.Duration) <-chan time.Time {
	args := m.Called(d)

	return args.Get(0).(chan time.Time)
}
