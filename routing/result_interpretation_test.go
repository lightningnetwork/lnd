package routing

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"

	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	hops = []route.Vertex{
		route.Vertex{1}, route.Vertex{2}, route.Vertex{3},
		route.Vertex{4},
	}

	routeOneHop = route.Route{
		Hops: []*route.Hop{
			&route.Hop{PubKeyBytes: hops[0]},
		},
	}

	routeTwoHop = route.Route{
		Hops: []*route.Hop{
			&route.Hop{PubKeyBytes: hops[0]},
			&route.Hop{PubKeyBytes: hops[1]},
		},
	}

	routeThreeHop = route.Route{
		Hops: []*route.Hop{
			&route.Hop{PubKeyBytes: hops[0]},
			&route.Hop{PubKeyBytes: hops[1]},
			&route.Hop{PubKeyBytes: hops[2]},
		},
	}

	routeFourHop = route.Route{
		Hops: []*route.Hop{
			&route.Hop{PubKeyBytes: hops[0]},
			&route.Hop{PubKeyBytes: hops[1]},
			&route.Hop{PubKeyBytes: hops[2]},
			&route.Hop{PubKeyBytes: hops[3]},
		},
	}
)

func TestResultInterpretationSuccess(t *testing.T) {
	i := newInterpretedResult(&routeTwoHop, true, nil, nil)

	if len(i.pairResults) != 1 {
		t.Fatal("expected one pair result")
	}

	if i.pairResults[newNodePair(hops[0], hops[1])].resultType !=
		ChannelResultSuccess {

		t.Fatal("wrong pair result")
	}
}

func TestResultInterpretationSuccessDirect(t *testing.T) {
	i := newInterpretedResult(&routeOneHop, true, nil, nil)

	if len(i.pairResults) != 0 {
		t.Fatal("expected no results")
	}
}

func TestResultInterpretationFail(t *testing.T) {
	failureSrcIdx := 1
	i := newInterpretedResult(
		&routeTwoHop, false, &failureSrcIdx,
		lnwire.NewTemporaryChannelFailure(nil),
	)

	if len(i.pairResults) != 1 {
		t.Fatal("expected one pair result")
	}

	if i.pairResults[newNodePair(hops[0], hops[1])].resultType !=
		ChannelResultFailBalance {

		t.Fatal("wrong pair result")
	}

	if i.finalFailure {
		t.Fatal("expected attempt to be non-final")
	}
}

func TestResultInterpretationFailExpiryTooSoon(t *testing.T) {
	failureSrcIdx := 3
	i := newInterpretedResult(
		&routeFourHop, false, &failureSrcIdx,
		lnwire.NewExpiryTooSoon(lnwire.ChannelUpdate{}),
	)

	if len(i.pairResults) != 2 {
		t.Fatal("expected two pair results")
	}

	if i.pairResults[newNodePair(hops[0], hops[1])].resultType !=
		ChannelResultFail {

		t.Fatal("wrong pair result")
	}

	if i.pairResults[newNodePair(hops[1], hops[2])].resultType !=
		ChannelResultFail {

		t.Fatal("wrong pair result")
	}

	if i.finalFailure {
		t.Fatal("expected attempt to be non-final")
	}
}
