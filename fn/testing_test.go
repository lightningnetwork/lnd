package fn

// testingMock is a mock used in tests of UnwrapOrFail methods of Option and
// Result. It implements TestingT, TestingHelper, and TestingFailer and just
// records if a method was called.
type testingMock struct {
	errorfCalled  bool
	helperCalled  bool
	failNowCalled bool
}

// Errorf records the fact that the method was called.
func (t *testingMock) Errorf(format string, args ...any) {
	t.errorfCalled = true
}

// Helper records the fact that the method was called.
func (t *testingMock) Helper() {
	t.helperCalled = true
}

// FailNow records the fact that the method was called.
func (t *testingMock) FailNow() {
	t.failNowCalled = true
}
