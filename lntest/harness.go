package lntest

import (
	"context"
	"testing"

	"github.com/go-errors/errors"
)

// TestCase defines a test case that's been used in the integration test.
type TestCase struct {
	// Name specifies the test name.
	Name string

	// TestFunc is the test case wrapped in a function.
	TestFunc func(t *HarnessTest)
}

// HarnessTest wraps a regular testing.T providing enhanced error detection
// and propagation. All error will be augmented with a full stack-trace in
// order to aid in debugging. Additionally, any panics caused by active
// test cases will also be handled and represented as fatals.
type HarnessTest struct {
	*testing.T

	// net is a reference to the current network harness.
	net *NetworkHarness

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	runCtx context.Context
	cancel context.CancelFunc
}

// NewHarnessTest creates a new instance of a harnessTest from a regular
// testing.T instance.
func NewHarnessTest(t *testing.T, net *NetworkHarness) *HarnessTest {
	ctxt, cancel := context.WithCancel(context.Background())
	return &HarnessTest{t, net, ctxt, cancel}
}

// RunTestCase executes a harness test case. Any errors or panics will be
// represented as fatal.
func (h *HarnessTest) RunTestCase(testCase *TestCase) {
	defer func() {
		// Canel the run context.
		h.cancel()

		if err := recover(); err != nil {
			description := errors.Wrap(err, 2).ErrorStack()
			h.Fatalf("Failed: (%v) panic with: \n%v",
				testCase.Name, description)
		}
	}()

	testCase.TestFunc(h)
}

// FailNow is called by require at the very end if a test failed.
func (h *HarnessTest) FailNow() {
	h.T.FailNow()
}

// Subtest creates a child HarnessTest.
func (h *HarnessTest) Subtest(t *testing.T) *HarnessTest {
	return NewHarnessTest(t, h.net)
}
