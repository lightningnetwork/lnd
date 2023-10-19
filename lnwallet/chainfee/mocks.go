package chainfee

import "github.com/stretchr/testify/mock"

type mockFeeSource struct {
	mock.Mock
}

// A compile-time assertion to ensure that mockFeeSource implements the
// WebAPIFeeSource interface.
var _ WebAPIFeeSource = (*mockFeeSource)(nil)

func (m *mockFeeSource) GetFeeMap() (map[uint32]uint32, error) {
	args := m.Called()

	return args.Get(0).(map[uint32]uint32), args.Error(1)
}
