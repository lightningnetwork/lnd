package chanstate

import "testing"

// TestHasChanStatus asserts the behavior of HasChanStatus by checking the
// behavior of various status flags in addition to the special case of
// ChanStatusDefault which is treated like a flag in the code base even though
// it isn't.
func TestHasChanStatus(t *testing.T) {
	tests := []struct {
		name   string
		status ChannelStatus
		expHas map[ChannelStatus]bool
	}{
		{
			name:   "default",
			status: ChanStatusDefault,
			expHas: map[ChannelStatus]bool{
				ChanStatusDefault: true,
				ChanStatusBorked:  false,
			},
		},
		{
			name:   "single flag",
			status: ChanStatusBorked,
			expHas: map[ChannelStatus]bool{
				ChanStatusDefault: false,
				ChanStatusBorked:  true,
			},
		},
		{
			name:   "multiple flags",
			status: ChanStatusBorked | ChanStatusLocalDataLoss,
			expHas: map[ChannelStatus]bool{
				ChanStatusDefault:       false,
				ChanStatusBorked:        true,
				ChanStatusLocalDataLoss: true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := &OpenChannel{}
			c.SetChannelStatusForStore(test.status)

			for status, expHas := range test.expHas {
				has := c.HasChanStatus(status)
				if has == expHas {
					continue
				}

				t.Fatalf("expected chan status to "+
					"have %s? %t, got: %t",
					status, expHas, has)
			}
		})
	}
}
