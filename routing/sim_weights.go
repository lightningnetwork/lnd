package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// SimObservation is one directed-channel liquidity observation: at a point in
// time, this amount either did or did not pass from one node to the next.
//
// This is deliberately the LOWEST common denominator of the two paradigms
// under comparison, and choosing it is the substantive design claim of the
// weight-serving proposal. lnd's mission control stores exactly this per
// directed pair (a last-fail amount and time, a highest-success amount and
// time) and the evolved routers store exactly this per directed channel (an
// upper-fail bound and a lower-ok bound). Neither side's internal
// representation is servable — one is a decaying penalty history keyed by
// the observer, the other an interval with an evidence count — but both are
// derivable from a stream of these.
//
// So a weight-serving API should serve OBSERVATIONS, not weights. Serving
// weights would force every consumer into the server's probability model;
// serving observations lets each consumer build its own. That is the
// difference between an API that only lnd can consume and one that a
// competing router design can also use.
type SimObservation struct {
	// From and To are the endpoints of the directed edge the amount was
	// forwarded over. To is the receiving node.
	From route.Vertex `json:"from"`
	To   route.Vertex `json:"to"`

	// ChanID identifies the channel. Mission control ignores it, keying
	// on the node pair instead; interval routers key on it directly,
	// which is why it has to be served even though half the consumers
	// throw it away.
	ChanID uint64 `json:"chan_id"`

	// AmtMsat is the amount that was carried over this edge.
	AmtMsat uint64 `json:"amt_msat"`

	// Success records whether the amount passed. A success is evidence
	// the edge held at least this much; a failure is evidence it held
	// less.
	Success bool `json:"success"`

	// TimeUnix is when the observation was made, so a consumer that ages
	// evidence can, and one that does not can ignore it.
	TimeUnix int64 `json:"time_unix"`
}

// SimObservationImporter is the optional half of the SimRouter contract that
// a router implements if it can accept knowledge it did not gather itself.
//
// It is deliberately optional, and what that costs is itself a finding: none
// of the champions evolved so far implement it, because nothing in the
// contract ever asked them to. A router that cannot be told what another
// node learned can only warm its beliefs by spending payments, which is the
// precise confound exp-012 could not escape.
type SimObservationImporter interface {
	// ImportObservations delivers third-party observations before any
	// payment is sent. Implementations must treat this as additive
	// evidence, and must tolerate being called with observations about
	// channels they will never use.
	ImportObservations(obs []SimObservation) error
}

// observationsFromAttempt derives what an attempt revealed about the edges it
// crossed.
//
// The information content of one attempt is asymmetric and that asymmetry is
// the whole reason attempts are worth anything: every hop BEFORE the failure
// point demonstrably carried its amount, and only the failing hop is known to
// have refused. A settled attempt proves every hop on the route.
func observationsFromAttempt(rt *route.Route, res SimHtlcResult,
	now time.Time) []SimObservation {

	// failIdx is the index of the node that reported the failure, i.e.
	// the sending end of the edge that refused. Everything strictly
	// before it forwarded successfully.
	failIdx := len(rt.Hops)
	if res.Failure != nil {
		idx := getNodeIndexSim(rt, res.FailureSource)
		if idx == nil {
			return nil
		}
		failIdx = *idx
	}

	obs := make([]SimObservation, 0, len(rt.Hops))
	prevNode := rt.SourcePubKey
	for i, hop := range rt.Hops {
		// The amount carried over channel i is the route total for the
		// first channel and the previous hop's amt-to-forward after
		// that, matching walkHtlc's accounting.
		amt := rt.TotalAmount
		if i > 0 {
			amt = rt.Hops[i-1].AmtToForward
		}

		if i > failIdx {
			break
		}

		obs = append(obs, SimObservation{
			From:     prevNode,
			To:       hop.PubKeyBytes,
			ChanID:   hop.ChannelID,
			AmtMsat:  uint64(amt),
			Success:  i < failIdx,
			TimeUnix: now.Unix(),
		})

		prevNode = hop.PubKeyBytes
	}

	return obs
}

// simObservationJSON is the wire shape of an observation.
//
// route.Vertex is a [33]byte, which encoding/json renders as a 33-element
// array of numbers. That is unreadable, four times larger than it needs to
// be, and wrong for something standing in for an API payload — a served
// observation should name its nodes the way every other Lightning interface
// does, as a hex pubkey.
type simObservationJSON struct {
	From     string `json:"from"`
	To       string `json:"to"`
	ChanID   uint64 `json:"chan_id"`
	AmtMsat  uint64 `json:"amt_msat"`
	Success  bool   `json:"success"`
	TimeUnix int64  `json:"time_unix"`
}

// MarshalJSON renders the observation with hex pubkeys.
func (o SimObservation) MarshalJSON() ([]byte, error) {
	return json.Marshal(simObservationJSON{
		From:     o.From.String(),
		To:       o.To.String(),
		ChanID:   o.ChanID,
		AmtMsat:  o.AmtMsat,
		Success:  o.Success,
		TimeUnix: o.TimeUnix,
	})
}

// UnmarshalJSON parses hex pubkeys back into vertices.
func (o *SimObservation) UnmarshalJSON(data []byte) error {
	var raw simObservationJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	from, err := route.NewVertexFromStr(raw.From)
	if err != nil {
		return fmt.Errorf("bad from pubkey %q: %w", raw.From, err)
	}

	to, err := route.NewVertexFromStr(raw.To)
	if err != nil {
		return fmt.Errorf("bad to pubkey %q: %w", raw.To, err)
	}

	*o = SimObservation{
		From:     from,
		To:       to,
		ChanID:   raw.ChanID,
		AmtMsat:  raw.AmtMsat,
		Success:  raw.Success,
		TimeUnix: raw.TimeUnix,
	}

	return nil
}

// WriteObservations serialises observations to a file.
//
// A nil slice is written as an empty array rather than null. A server with
// nothing to offer is a real case — a poorly connected node observes almost
// nothing — and it should serve an empty response, not a malformed one.
func WriteObservations(path string, obs []SimObservation) error {
	if obs == nil {
		obs = []SimObservation{}
	}

	encoded, err := json.MarshalIndent(obs, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, encoded, 0644)
}

// ReadObservations loads observations from a file.
func ReadObservations(path string) ([]SimObservation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var obs []SimObservation
	if err := json.Unmarshal(data, &obs); err != nil {
		return nil, fmt.Errorf("unable to parse observations: %w", err)
	}

	return obs, nil
}

// Observations returns everything the runner has seen so far, in order. This
// is the server side of the proposed API: what a node that has been paying
// could hand to one that has not.
func (r *SimRunner) Observations() []SimObservation {
	return r.observations
}

// ImportObservations injects third-party knowledge WITHOUT sending a single
// payment, which is the entire point of the mechanism.
//
// exp-012 tried to measure the value of a warm cache by running unscored
// warmup payments, and could not separate the knowledge from its price: the
// warmup drains the very corridors it teaches about, so the drain arm pays in
// depletion and the restore arm pays in staleness. Served weights arrive over
// an API and cost the consumer nothing, and this is the only construction
// that reproduces that.
//
// Both consumers are fed from the same observation stream. lnd's side goes
// through MissionControl.ImportHistory, which already ships — worth noting
// for the upstream conversation, since it means the serving proposal needs no
// new consumer machinery on lnd's side, only a source of snapshots.
func (r *SimRunner) ImportObservations(obs []SimObservation) error {
	return r.ImportObservationsPolicy(obs, SimImportPolicy{
		ExcludeLocal: true,
	})
}

// SimImportPolicy controls which served observations a consumer accepts.
type SimImportPolicy struct {
	// ExcludeLocal drops observations about channels the consumer is
	// itself an endpoint of.
	//
	// This defaults on because exp-012 part 4 measured the cost of the
	// alternative. Observations about remote pairs transfer between
	// vantages perfectly well — a failure at a distant relay names no
	// observer, and a bound on a channel is a fact any observer would
	// have recorded identically. Observations about the consumer's OWN
	// channels are different: every payment it sends must cross one of
	// them, so importing stale claims about them poisons the first hop
	// of everything. lnd's attempt count tripled (0.8 → 3.0) when warmed
	// from its own vantage rather than a stranger's, thrashing around
	// its own stale first hop, while the interval routers were
	// unaffected either way.
	//
	// Set it false only to reproduce that failure deliberately.
	ExcludeLocal bool
}

// SimImportStats reports what an import actually delivered, so a sweep can
// tell an ineffective import from an empty one.
type SimImportStats struct {
	// Offered is how many observations the file contained.
	Offered int

	// Accepted is how many survived the policy filter.
	Accepted int

	// DroppedLocal is how many were about the consumer's own channels.
	DroppedLocal int

	// RouterAccepts records whether the routing strategy under test can
	// consume observations at all.
	RouterAccepts bool
}

// ImportObservationsPolicy is ImportObservations with an explicit policy, and
// returns what the import actually delivered.
func (r *SimRunner) ImportObservationsPolicy(obs []SimObservation,
	policy SimImportPolicy) error {

	_, err := r.importWithStats(obs, policy)

	return err
}

// ImportWeightsFile loads served observations and imports them under the
// given policy, reporting what landed.
func (r *SimRunner) ImportWeightsFile(path string,
	policy SimImportPolicy) (*SimImportStats, error) {

	obs, err := ReadObservations(path)
	if err != nil {
		return nil, err
	}

	return r.importWithStats(obs, policy)
}

// importWithStats applies the policy filter and hands the survivors to both
// consumer paths.
func (r *SimRunner) importWithStats(obs []SimObservation,
	policy SimImportPolicy) (*SimImportStats, error) {

	stats := &SimImportStats{
		Offered:       len(obs),
		RouterAccepts: r.RouterAcceptsImports(),
	}

	kept := obs
	if policy.ExcludeLocal {
		kept = make([]SimObservation, 0, len(obs))
		for _, o := range obs {
			if r.touchesLocalChannel(o) {
				stats.DroppedLocal++

				continue
			}

			kept = append(kept, o)
		}
	}
	stats.Accepted = len(kept)

	if len(kept) == 0 {
		return stats, nil
	}

	if err := r.importToMissionControl(kept); err != nil {
		return nil, err
	}

	// The candidate side needs a router instance to import into, and
	// routers are constructed per payment. Every evolved router so far
	// keeps its beliefs in package-level state that outlives the
	// instance, so importing into one throwaway router reaches the state
	// the scored payments will read. A router with genuinely per-instance
	// state would need the contract to say more than it does.
	r.pendingImport = kept

	return stats, nil
}

// touchesLocalChannel reports whether an observation describes a channel the
// consumer is an endpoint of.
func (r *SimRunner) touchesLocalChannel(o SimObservation) bool {
	if o.From == r.source || o.To == r.source {
		return true
	}

	// A channel id can name a local channel even when neither endpoint
	// in the observation is the source, if the server recorded the pair
	// from its own side of a shared channel.
	channel, ok := r.graph.channels[o.ChanID]
	if !ok {
		return false
	}

	return channel.ends[0].owner == r.source ||
		channel.ends[1].owner == r.source
}

// importToMissionControl folds the observation stream into lnd's own history
// format and hands it to the production import path.
//
// Mission control keys on the directed NODE PAIR, not the channel, so
// observations about different channels between the same two nodes collapse
// into one entry here — the first place the served representation loses
// something a consumer wanted. An interval router keeps them apart.
func (r *SimRunner) importToMissionControl(obs []SimObservation) error {
	pairs := make(map[DirectedNodePair]*TimedPairResult)

	for _, o := range obs {
		pair := NewDirectedNodePair(o.From, o.To)
		when := time.Unix(o.TimeUnix, 0)
		amt := lnwire.MilliSatoshi(o.AmtMsat)

		entry, ok := pairs[pair]
		if !ok {
			entry = &TimedPairResult{}
			pairs[pair] = entry
		}

		// Mission control's own semantics: the last failure by time,
		// and the HIGHEST success rather than the last one.
		if o.Success {
			if amt > entry.SuccessAmt {
				entry.SuccessAmt = amt
				entry.SuccessTime = when
			}

			continue
		}

		if entry.FailTime.IsZero() || when.After(entry.FailTime) {
			entry.FailTime = when
			entry.FailAmt = amt
		}
	}

	snapshot := &MissionControlSnapshot{
		Pairs: make([]MissionControlPairSnapshot, 0, len(pairs)),
	}
	for pair, result := range pairs {
		snapshot.Pairs = append(snapshot.Pairs,
			MissionControlPairSnapshot{
				Pair:            pair,
				TimedPairResult: *result,
			},
		)
	}

	// force=false, and the reason is not caution but correctness.
	// importSnapshot always applies BOTH a failure and a success entry
	// for every pair, so a pair we only ever observed failing carries a
	// zero-amount success alongside it. Under force, setLastPairResult
	// takes that zero at face value and rewrites the failure amount to
	// successAmt+1 — a 750k msat failure lands in mission control as a
	// 1 msat failure, which is a far more severe claim than anything we
	// observed. Unforced, the zero success is correctly ignored.
	//
	// It also gives the right semantics for a served cache generally:
	// third-party knowledge must not overwrite fresher local knowledge.
	return r.mc.ImportHistory(snapshot, false)
}

// deliverPendingImport hands imported observations to a freshly built router
// if it can take them, and clears the pending set so the evidence is counted
// once rather than once per payment.
func (r *SimRunner) deliverPendingImport(router SimRouter) error {
	if len(r.pendingImport) == 0 {
		return nil
	}

	importer, ok := router.(SimObservationImporter)
	if !ok {
		// Not an error: the lnd stack takes its copy through mission
		// control, and an evolved router that never implemented the
		// optional half of the contract simply cannot be told. The
		// caller reports the distinction.
		r.pendingImport = nil

		return nil
	}

	obs := r.pendingImport
	r.pendingImport = nil

	return importer.ImportObservations(obs)
}

// RouterAcceptsImports reports whether the routing strategy under test can
// consume served observations at all, so a sweep can distinguish "imports did
// not help" from "imports were never delivered".
func (r *SimRunner) RouterAcceptsImports() bool {
	// The lnd stack takes its copy through mission control, which every
	// import already writes to, so the default strategy always consumes
	// served observations even though lndStackRouter does not implement
	// the optional interface.
	if !r.customRouter {
		return true
	}

	router, err := r.routerFactory(
		&simGossipView{g: r.graph, now: r.clk.Now}, r.source,
		r.graph.LocalBalances(r.source), &SimPaymentSpec{
			Target:   r.source,
			Amount:   1,
			MaxParts: 1,
		},
	)
	if err != nil {
		return false
	}

	_, ok := router.(SimObservationImporter)

	return ok
}
