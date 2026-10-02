// manager.p states the contract of the gossip sync manager in
// discovery/gossipsync (ManagerState in manager_state.go).
//
// The model is the ideal contract, not a transliteration. The Manager machine
// records what each event means, then a settle step restores the manager's
// two goals. Wherever the contract lets the manager pick, the model picks
// nondeterministically from the set of choices the contract allows: which
// eligible peer runs a historical sync, which passive peer is promoted, and
// which pair a rotation swaps. The model never sorts, never draws from a
// random source, and never depends on the order of a map.
//
// Each Syncer machine stands in for one session's peer syncer. It answers
// asynchronously, completing, failing with a peer or local fault, working a
// little longer, or draining after a peer fault and refusing new attempts as
// busy meanwhile. The checker therefore explores outcomes that race
// disconnects, reconnects with the same public key (a new session), ticks and
// rotations, and outcomes that arrive after their session was stopped.
//
// ManagerContract is the declarative safety contract. It keeps its own ledger
// of attempts, failure epochs and pinned peers from the manager's
// announcements, and checks every snapshot against it, so it does not trust
// the manager's bookkeeping. GraphEventuallySynced is the liveness contract.
//
// For the Go bridge, the manager prints every input it handles, every choice
// it makes along with the set it chose from, every forced action, and an
// observable snapshot after each input. See pmodel_bridge_test.go.

enum Role { PASSIVE, ACTIVE, PINNED }

enum Kind { COMPLETED, PEERFAULT, LOCALFAULT, BUSY }

// PickKind names what a choice decides, which is how the bridge finds the
// same decision in the Go manager's outbox.
enum PickKind { PICK_START, PICK_PROMOTE, PICK_DEMOTE }

enum Op { OP_CONNECT, OP_DISCONNECT, OP_ROTATE, OP_TICK, OP_OUTCOME }

// tProfile selects the production contract, or a variant that a test case
// uses to show a rule is, or is not, load-bearing.
//
//   noSettle: no settle step runs after an event.
//   legacyTick: a historical tick picks a peer before it opens the new epoch,
//     and never falls back to a peer that failed in the last epoch.
//   noLocalBackoff: a local fault does not record a failure epoch, which was
//     the behavior before the review fix.
//   noSessionCheck: an outcome counts if its attempt is in flight, whichever
//     session reports it.
//   noPinnedRetry: a pinned peer whose attempt failed is not retried, which
//     was the behavior before ticks retried pinned peers.
type tProfile = (
  noSettle: bool,
  legacyTick: bool,
  noLocalBackoff: bool,
  noSessionCheck: bool,
  noPinnedRetry: bool
);

// tManagerCfg configures a manager.
// faults bounds how many times each syncer may fail before it must
// complete; a negative bound is unlimited.
type tManagerCfg = (numActive: int, profile: tProfile, faults: int);

// tMember is a connected peer, keyed by its session. retry is set on a
// pinned peer whose attempt failed, and the next tick retries it.
type tMember = (pub: int, role: Role, pinned: bool, retry: bool);

// Inputs to the manager.
event eConnect: (pub: int, pinned: bool);
event eDisconnect: int;
event eRotateTick;
event eHistTick;
event eOutcome: (session: int, attempt: int, kind: Kind);

// eStatusQuery and eStatus let the liveness ticker ask whether a tick is
// needed. They are model plumbing, not manager inputs, and are not bridged.
event eStatusQuery: machine;
event eStatus: (synced: bool, needTick: bool, working: bool);

// Commands to a syncer, and the syncer's own progress steps.
event eStart: int;
event eSetRole: Role;
event eStopSyncer;
event eProgress;

// Announcements the spec monitors observe. eMInput comes before the manager
// changes anything, eMStarted for every attempt it starts, and eMSnap after
// it has handled the input.
event eMInput: (op: Op, pub: int, pinned: bool, session: int, attempt: int,
  kind: Kind);
event eMStarted: (attempt: int, session: int, pub: int, pinned: bool,
  tracked: bool);
event eMSnap: (members: map[int, tMember], inFlight: map[int, int],
  tracked: int, synced: bool, numActive: int);

machine Manager {
  var cfg: tManagerCfg;

  // members are the connected peers by session, and sessionOf maps a
  // connected peer to its session.
  var members: map[int, tMember];
  var sessionOf: map[int, int];

  // inFlight maps each attempt in flight to its session. tracked is the
  // attempt the manager fails over while unsynced, or zero.
  var inFlight: map[int, int];
  var tracked: int;

  // failedAt is the epoch in which each peer last broke an attempt.
  var failedAt: map[int, int];
  var epoch: int;

  var nextSession: int;
  var nextAttempt: int;
  var synced: bool;

  var syncers: map[int, machine];

  // The observable effects of the input being handled.
  var started: seq[int];
  var reset: bool;
  var publish: bool;

  start state Run {
    entry (c: tManagerCfg) {
      cfg = c;
      nextSession = 1;
      nextAttempt = 1;
      print format("MTRACE begin num_active={0}", cfg.numActive);
    }

    on eConnect do (e: (pub: int, pinned: bool)) {
      Begin(format("connect pub={0} pinned={1}", e.pub, B(e.pinned)),
        (op = OP_CONNECT, pub = e.pub, pinned = e.pinned, session = 0,
         attempt = 0, kind = COMPLETED));
      OnConnect(e.pub, e.pinned);
      Finish();
    }

    on eDisconnect do (pub: int) {
      Begin(format("disconnect pub={0}", pub),
        (op = OP_DISCONNECT, pub = pub, pinned = false, session = 0,
         attempt = 0, kind = COMPLETED));
      OnDisconnect(pub);
      Finish();
    }

    on eOutcome do (e: (session: int, attempt: int, kind: Kind)) {
      Begin(format("outcome session={0} attempt={1} kind={2}", e.session,
        e.attempt, e.kind to int),
        (op = OP_OUTCOME, pub = 0, pinned = false, session = e.session,
         attempt = e.attempt, kind = e.kind));
      OnOutcome(e.session, e.attempt, e.kind);
      Finish();
    }

    on eHistTick do {
      Begin("hist_tick", (op = OP_TICK, pub = 0, pinned = false,
        session = 0, attempt = 0, kind = COMPLETED));
      OnHistTick();
      Finish();
    }

    on eRotateTick do {
      Begin("rotate", (op = OP_ROTATE, pub = 0, pinned = false,
        session = 0, attempt = 0, kind = COMPLETED));
      OnRotate();
      Finish();
    }

    on eStatusQuery do (who: machine) {
      send who, eStatus, (
        synced = synced,
        needTick = !synced && sizeof(members) > 0 && sizeof(inFlight) == 0,
        working = sizeof(inFlight) > 0
      );
    }
  }

  // Begin prints and announces the input, and clears the step's effects.
  fun Begin(line: string, inp: (op: Op, pub: int, pinned: bool,
    session: int, attempt: int, kind: Kind)) {

    print format("MTRACE in {0}", line);
    announce eMInput, inp;
    started = default(seq[int]);
    reset = false;
    publish = false;
  }

  // Finish runs settle, then prints and announces the snapshot.
  fun Finish() {
    if (!cfg.profile.noSettle) {
      Settle();
    }

    print format("MTRACE snap {0}", RenderSnap());
    announce eMSnap, (members = members, inFlight = inFlight,
      tracked = tracked, synced = synced, numActive = cfg.numActive);
  }

  // Decide picks any element of the allowed set, and records the choice and
  // the set for the bridge.
  fun Decide(kind: PickKind, allowed: set[int]): int {
    var s: int;
    s = choose(allowed);
    print format("MTRACE choice kind={0} session={1} from={2}",
      PickName(kind), s, Join(SortSet(allowed)));
    return s;
  }

  // Force records an action the contract requires with no choice.
  fun Force(kind: PickKind, s: int) {
    print format("MTRACE force kind={0} session={1}", PickName(kind), s);
  }

  fun Busy(s: int): bool {
    var a: int;
    foreach (a in keys(inFlight)) {
      if (inFlight[a] == s) {
        return true;
      }
    }
    return false;
  }

  fun FailedIn(pub: int, e: int): bool {
    return pub in failedAt && failedAt[pub] == e;
  }

  // NonPinned returns the connected sessions whose peer is not pinned.
  fun NonPinned(): set[int] {
    var out: set[int];
    var s: int;
    foreach (s in keys(members)) {
      if (!members[s].pinned) {
        out += (s);
      }
    }
    return out;
  }

  fun WithRole(r: Role): set[int] {
    var out: set[int];
    var s: int;
    foreach (s in NonPinned()) {
      if (members[s].role == r) {
        out += (s);
      }
    }
    return out;
  }

  // Eligible returns the sessions a historical sync may run on: connected,
  // not pinned, no attempt in flight, not failed in the current epoch, and
  // not the excluded session. Zero excludes nothing.
  fun Eligible(exclude: int): set[int] {
    var out: set[int];
    var s: int;
    foreach (s in NonPinned()) {
      if (!Busy(s) && !FailedIn(members[s].pub, epoch) && s != exclude) {
        out += (s);
      }
    }
    return out;
  }

  fun StartAttempt(s: int, isTracked: bool) {
    var a: int;
    a = nextAttempt;
    nextAttempt = nextAttempt + 1;
    inFlight[a] = s;
    started += (sizeof(started), a);

    announce eMStarted, (attempt = a, session = s, pub = members[s].pub,
      pinned = members[s].pinned, tracked = isTracked);
    send syncers[s], eStart, a;

    if (isTracked) {
      tracked = a;
      reset = true;
    }
  }

  fun SetRole(s: int, r: Role) {
    var m: tMember;
    m = members[s];
    if (m.role == r) {
      return;
    }
    m.role = r;
    members[s] = m;
    send syncers[s], eSetRole, r;
  }

  // Settle restores the two goals. While unsynced, a tracked historical
  // sync runs whenever some peer is eligible. Once synced, the active quota
  // is as full as the connected peers allow.
  fun Settle() {
    var c: set[int];
    var passive: set[int];

    if (synced) {
      passive = WithRole(PASSIVE);
      while (sizeof(WithRole(ACTIVE)) < cfg.numActive &&
        sizeof(passive) > 0) {

        SetRole(Decide(PICK_PROMOTE, passive), ACTIVE);
        passive = WithRole(PASSIVE);
      }
      return;
    }

    if (tracked == 0 && cfg.numActive > 0) {
      c = Eligible(0);
      if (sizeof(c) > 0) {
        StartAttempt(Decide(PICK_START, c), true);
      }
    }
  }

  // OnConnect adds a peer. A pinned peer is pinned and runs its own
  // untracked attempt. The first non-pinned peer after losing every peer,
  // once synced, runs an untracked resync. Everything else is settle's job.
  fun OnConnect(pub: int, isPinned: bool) {
    var s: int;
    var hadNoPeers: bool;

    if (pub in sessionOf) {
      return;
    }

    s = nextSession;
    nextSession = nextSession + 1;
    hadNoPeers = sizeof(NonPinned()) == 0;

    members[s] = (pub = pub, role = PASSIVE, pinned = isPinned,
      retry = false);
    sessionOf[pub] = s;
    syncers[s] = new Syncer((manager = this, session = s,
      faults = cfg.faults));

    if (isPinned) {
      SetRole(s, PINNED);
      Force(PICK_START, s);
      StartAttempt(s, false);
      return;
    }

    if (synced && hadNoPeers && cfg.numActive > 0 && !FailedIn(pub, epoch)) {
      Force(PICK_START, s);
      StartAttempt(s, false);
    }
  }

  // OnDisconnect removes a peer and forgets its attempts.
  fun OnDisconnect(pub: int) {
    var s: int;
    var a: int;
    var gone: seq[int];

    if (!(pub in sessionOf)) {
      return;
    }

    s = sessionOf[pub];
    sessionOf -= (pub);
    members -= (s);
    send syncers[s], eStopSyncer;
    syncers -= (s);

    foreach (a in keys(inFlight)) {
      if (inFlight[a] == s) {
        gone += (sizeof(gone), a);
      }
    }
    foreach (a in gone) {
      inFlight -= (a);
      if (tracked == a) {
        tracked = 0;
      }
    }
  }

  // OnOutcome records how an attempt ended. An outcome counts only if its
  // attempt is in flight on the session that reports it.
  fun OnOutcome(s: int, a: int, kind: Kind) {
    var owner: int;
    var m: tMember;

    if (!(a in inFlight)) {
      return;
    }
    owner = inFlight[a];
    if (owner != s && !cfg.profile.noSessionCheck) {
      return;
    }

    inFlight -= (a);
    if (tracked == a) {
      tracked = 0;
    }

    if (kind == COMPLETED) {
      if (!synced) {
        synced = true;
        tracked = 0;
        publish = true;
      }
      return;
    }

    if (kind == LOCALFAULT && cfg.profile.noLocalBackoff) {
      return;
    }

    // Nothing else ever picks a pinned peer, so instead of a failure epoch
    // it is marked to be retried by the next tick.
    if (members[owner].pinned) {
      if (!cfg.profile.noPinnedRetry) {
        m = members[owner];
        m.retry = true;
        members[owner] = m;
      }
      return;
    }

    failedAt[members[owner].pub] = epoch;
  }

  // OnHistTick opens a new epoch, retries every pinned peer marked to
  // retry, and starts a historical sync on an eligible peer other than the
  // one running the tracked attempt, preferring peers that did not fail in
  // the epoch just closed. That attempt is tracked while the graph is
  // unsynced.
  fun OnHistTick() {
    var exclude: int;
    var c: set[int];
    var preferred: set[int];
    var s: int;

    if (!cfg.profile.legacyTick) {
      epoch = epoch + 1;
    }

    RetryPinned();

    if (cfg.numActive > 0) {
      exclude = 0;
      if (tracked != 0) {
        exclude = inFlight[tracked];
      }

      c = Eligible(exclude);
      if (!cfg.profile.legacyTick) {
        foreach (s in c) {
          if (!FailedIn(members[s].pub, epoch - 1)) {
            preferred += (s);
          }
        }
        if (sizeof(preferred) > 0) {
          c = preferred;
        }
      }

      if (sizeof(c) > 0) {
        StartAttempt(Decide(PICK_START, c), !synced);
      }
    }

    if (cfg.profile.legacyTick) {
      epoch = epoch + 1;
    }
  }

  // RetryPinned starts an untracked attempt on every pinned peer marked to
  // retry that has none in flight, whatever the quota. The order is not a
  // choice the contract makes; the ascending order only keeps attempt IDs
  // aligned with the Go manager's.
  fun RetryPinned() {
    var s: int;
    var m: tMember;

    foreach (s in SortSeq(keys(members))) {
      if (members[s].pinned && members[s].retry && !Busy(s)) {
        m = members[s];
        m.retry = false;
        members[s] = m;
        Force(PICK_START, s);
        StartAttempt(s, false);
      }
    }
  }

  // OnRotate swaps any active peer for any passive one.
  fun OnRotate() {
    var active: set[int];
    var passive: set[int];
    var a: int;
    var p: int;

    active = WithRole(ACTIVE);
    passive = WithRole(PASSIVE);
    if (sizeof(active) == 0 || sizeof(passive) == 0) {
      return;
    }

    a = Decide(PICK_DEMOTE, active);
    p = Decide(PICK_PROMOTE, passive);
    SetRole(a, PASSIVE);
    SetRole(p, ACTIVE);
  }

  // RenderSnap renders the observable snapshot the bridge compares.
  fun RenderSnap(): string {
    var roles: string;
    var s: int;
    var a: int;
    var sess: seq[int];
    var busy: seq[int];
    var trackedSession: int;
    var first: bool;

    roles = "-";
    first = true;
    foreach (s in SortSeq(keys(members))) {
      if (first) {
        roles = format("{0}:{1}", s, RoleName(members[s].role));
        first = false;
      } else {
        roles = format("{0},{1}:{2}", roles, s, RoleName(members[s].role));
      }
    }

    foreach (a in keys(inFlight)) {
      busy += (sizeof(busy), inFlight[a]);
    }

    trackedSession = 0;
    if (tracked != 0) {
      trackedSession = inFlight[tracked];
    }

    return format("{0} {1}",
      format("roles={0} synced={1} tracked={2} inflight={3}", roles,
        B(synced), trackedSession, Join(SortSeq(busy))),
      format("started={0} reset={1} publish={2}", Join(started),
        B(reset), B(publish)));
  }
}

// Syncer stands in for one session's peer syncer. It completes, fails or
// keeps working at its own pace, drains after a peer fault, and refuses a new
// attempt as busy until it is idle again. It fails at most the configured
// number of times, so under a liveness profile a peer that keeps being
// picked eventually completes.
machine Syncer {
  var manager: machine;
  var session: int;
  var attempt: int;
  var budget: int;
  var faultsLeft: int;

  start state Init {
    entry (p: (manager: machine, session: int, faults: int)) {
      manager = p.manager;
      session = p.session;
      faultsLeft = p.faults;
      goto Idle;
    }
  }

  state Idle {
    on eStart do (a: int) {
      attempt = a;
      budget = 3;
      goto Working;
    }

    ignore eSetRole, eProgress;
    on eStopSyncer goto Stopped;
  }

  state Working {
    entry {
      send this, eProgress;
    }

    on eProgress do {
      var roll: int;

      budget = budget - 1;
      if (budget > 0 && $) {
        send this, eProgress;
        return;
      }

      roll = choose(3);
      if (faultsLeft == 0) {
        roll = 0;
      }
      if (roll != 0 && faultsLeft > 0) {
        faultsLeft = faultsLeft - 1;
      }

      if (roll == 0) {
        Report(attempt, COMPLETED);
        goto Idle;
      } else if (roll == 1) {
        Report(attempt, PEERFAULT);
        goto Draining;
      } else {
        Report(attempt, LOCALFAULT);
        goto Idle;
      }
    }

    on eStart do (a: int) {
      Report(a, BUSY);
    }

    ignore eSetRole;
    on eStopSyncer goto Stopped;
  }

  state Draining {
    entry {
      budget = 2;
      send this, eProgress;
    }

    on eProgress do {
      budget = budget - 1;
      if (budget <= 0 || $) {
        goto Idle;
      }
      send this, eProgress;
    }

    on eStart do (a: int) {
      Report(a, BUSY);
    }

    ignore eSetRole;
    on eStopSyncer goto Stopped;
  }

  state Stopped {
    ignore eStart, eSetRole, eProgress, eStopSyncer;
  }

  fun Report(a: int, k: Kind) {
    send manager, eOutcome, (session = session, attempt = a, kind = k);
  }
}

// ManagerContract is the manager's safety contract. It rebuilds the
// attempts in flight, the failure epochs and the pinned peers from the
// announcements alone, and after every input checks:
//
//   (a) while unsynced with a quota, if some peer is eligible, a tracked
//       attempt is in flight;
//   (b) once synced, the active non-pinned count is min(quota, non-pinned);
//   (c) the active count never exceeds the quota;
//   (d) pinned peers are PINNED and no other peer is;
//   (e) at most one attempt is in flight per session;
//   (f) a stale outcome changes nothing observable;
//   (g) graph-synced is monotonic, and only a Completed outcome of an
//       attempt in flight on its session sets it;
//   (h) a non-pinned peer that failed in an epoch is not started again in
//       that epoch;
//   and that the manager's attempts in flight match the ledger, and its
//   tracked attempt is one of them.
spec ManagerContract observes eMInput, eMStarted, eMSnap {
  var ledger: map[int, int];
  var failed: map[int, int];
  var epoch: int;
  var pinnedPubs: set[int];

  // connected is the set of connected peers, from the inputs alone.
  var connected: set[int];

  var prevMembers: map[int, tMember];
  var prevInFlight: map[int, int];
  var prevTracked: int;
  var prevSynced: bool;

  var stale: bool;
  var completes: bool;
  var startedThisStep: int;

  start state Watch {
    on eMInput do (inp: (op: Op, pub: int, pinned: bool, session: int,
      attempt: int, kind: Kind)) {

      var s: int;
      var a: int;
      var gone: seq[int];
      var pub: int;

      stale = false;
      completes = false;
      startedThisStep = 0;

      if (inp.op == OP_CONNECT && inp.pinned) {
        pinnedPubs += (inp.pub);
      }
      if (inp.op == OP_CONNECT) {
        connected += (inp.pub);
      }

      if (inp.op == OP_TICK) {
        epoch = epoch + 1;
      }

      if (inp.op == OP_DISCONNECT) {
        foreach (s in keys(prevMembers)) {
          if (prevMembers[s].pub == inp.pub) {
            foreach (a in keys(ledger)) {
              if (ledger[a] == s) {
                gone += (sizeof(gone), a);
              }
            }
          }
        }
        foreach (a in gone) {
          ledger -= (a);
        }
        if (inp.pub in connected) {
          connected -= (inp.pub);
        }
      }

      if (inp.op == OP_OUTCOME) {
        if (!(inp.attempt in ledger) || ledger[inp.attempt] != inp.session) {
          stale = true;
          return;
        }

        ledger -= (inp.attempt);
        if (inp.kind == COMPLETED) {
          completes = true;
          return;
        }

        pub = prevMembers[inp.session].pub;
        if (!(pub in pinnedPubs)) {
          failed[pub] = epoch;
        }
      }
    }

    on eMStarted do (st: (attempt: int, session: int, pub: int,
      pinned: bool, tracked: bool)) {

      var a: int;

      foreach (a in keys(ledger)) {
        assert ledger[a] != st.session,
          format("(e) attempt {0} started on session {1}, busy with {2}",
            st.attempt, st.session, a);
      }

      if (!(st.pub in pinnedPubs)) {
        assert !(st.pub in failed && failed[st.pub] == epoch),
          format("(h) attempt {0} started on peer {1}, failed in epoch {2}",
            st.attempt, st.pub, epoch);
      }

      ledger[st.attempt] = st.session;
      startedThisStep = startedThisStep + 1;
    }

    on eMSnap do (sn: (members: map[int, tMember], inFlight: map[int, int],
      tracked: int, synced: bool, numActive: int)) {

      var s: int;
      var a: int;
      var active: int;
      var nonPinned: int;
      var eligible: int;
      var busy: bool;
      var pub: int;
      var pubs: set[int];

      // Every connected peer has exactly one session, and nobody else
      // has one.
      pubs = default(set[int]);
      foreach (s in keys(sn.members)) {
        pubs += (sn.members[s].pub);
      }
      assert pubs == connected && sizeof(sn.members) == sizeof(connected),
        format("sessions {0} do not match the connected peers {1}",
          sn.members, connected);

      assert sn.inFlight == ledger,
        "the manager's attempts in flight differ from the contract's ledger";
      assert sn.tracked == 0 || sn.tracked in sn.inFlight,
        "the tracked attempt is not in flight";

      foreach (s in keys(sn.members)) {
        pub = sn.members[s].pub;
        assert (pub in pinnedPubs) == (sn.members[s].role == PINNED),
          format("(d) session {0} of peer {1} has role {2}", s, pub,
            sn.members[s].role);

        if (pub in pinnedPubs) {
          continue;
        }

        nonPinned = nonPinned + 1;
        if (sn.members[s].role == ACTIVE) {
          active = active + 1;
        }

        busy = false;
        foreach (a in keys(ledger)) {
          if (ledger[a] == s) {
            busy = true;
          }
        }
        if (!busy && !(pub in failed && failed[pub] == epoch)) {
          eligible = eligible + 1;
        }
      }

      assert active <= sn.numActive,
        format("(c) {0} active peers exceed the quota {1}", active,
          sn.numActive);

      if (!sn.synced && sn.numActive > 0 && eligible > 0) {
        assert sn.tracked != 0,
          format("(a) unsynced, {0} eligible peers, no tracked attempt",
            eligible);
      }

      if (sn.synced) {
        assert active == MinInt(sn.numActive, nonPinned),
          format("(b) synced, {0} active of {1} non-pinned, quota {2}",
            active, nonPinned, sn.numActive);
      }

      if (stale) {
        assert sn.members == prevMembers && sn.inFlight == prevInFlight &&
          sn.tracked == prevTracked && sn.synced == prevSynced &&
          startedThisStep == 0,
          "(f) a stale outcome changed the manager's observable state";
      }

      assert !prevSynced || sn.synced, "(g) graph-synced was cleared";
      assert prevSynced || !sn.synced || completes,
        "(g) graph-synced set without a Completed outcome in flight";

      prevMembers = sn.members;
      prevInFlight = sn.inFlight;
      prevTracked = sn.tracked;
      prevSynced = sn.synced;
    }
  }
}

// GraphEventuallySynced is the manager's liveness contract: while the graph
// is unsynced, the quota is non-zero and a non-pinned peer is connected, the
// monitor is hot. It must be cold when the system quiesces. It only holds
// under the fairness the liveness drivers provide: every syncer eventually
// answers, a peer that is picked often enough eventually completes, and,
// where peers can fail, historical ticks keep coming while none is in
// flight.
spec GraphEventuallySynced observes eMSnap {
  start cold state Settled {
    on eMSnap do (sn: (members: map[int, tMember], inFlight: map[int, int],
      tracked: int, synced: bool, numActive: int)) {

      if (Pending(sn.members, sn.synced, sn.numActive)) {
        goto Unsynced;
      }
    }
  }

  hot state Unsynced {
    on eMSnap do (sn: (members: map[int, tMember], inFlight: map[int, int],
      tracked: int, synced: bool, numActive: int)) {

      if (!Pending(sn.members, sn.synced, sn.numActive)) {
        goto Settled;
      }
    }
  }
}

// GraphEventuallySyncedWithPinned extends GraphEventuallySynced to pinned
// peers, whatever the quota: while the graph is unsynced and a pinned peer
// is connected, the monitor is hot. It holds because a tick retries a pinned
// peer whose attempt failed, under the same fairness as
// GraphEventuallySynced.
spec GraphEventuallySyncedWithPinned observes eMSnap {
  start cold state Settled {
    on eMSnap do (sn: (members: map[int, tMember], inFlight: map[int, int],
      tracked: int, synced: bool, numActive: int)) {

      if (PendingPinned(sn.members, sn.synced)) {
        goto Unsynced;
      }
    }
  }

  hot state Unsynced {
    on eMSnap do (sn: (members: map[int, tMember], inFlight: map[int, int],
      tracked: int, synced: bool, numActive: int)) {

      if (!PendingPinned(sn.members, sn.synced)) {
        goto Settled;
      }
    }
  }
}

// PendingPinned reports whether the graph is unsynced with a pinned peer
// connected.
fun PendingPinned(members: map[int, tMember], synced: bool): bool {
  var s: int;

  if (synced) {
    return false;
  }
  foreach (s in keys(members)) {
    if (members[s].pinned) {
      return true;
    }
  }
  return false;
}

// Pending reports whether the graph still owes a sync to a connected,
// non-pinned peer.
fun Pending(members: map[int, tMember], synced: bool, numActive: int): bool {
  var s: int;

  if (synced || numActive == 0) {
    return false;
  }
  foreach (s in keys(members)) {
    if (!members[s].pinned) {
      return true;
    }
  }
  return false;
}

fun MinInt(a: int, b: int): int {
  if (a < b) {
    return a;
  }
  return b;
}

fun B(b: bool): int {
  if (b) {
    return 1;
  }
  return 0;
}

fun RoleName(r: Role): string {
  if (r == ACTIVE) {
    return "A";
  }
  if (r == PINNED) {
    return "X";
  }
  return "P";
}

fun PickName(k: PickKind): string {
  if (k == PICK_PROMOTE) {
    return "promote";
  }
  if (k == PICK_DEMOTE) {
    return "demote";
  }
  return "start";
}

// SortSeq returns xs in ascending order. It only canonicalizes printed
// output; no decision depends on it.
fun SortSeq(xs: seq[int]): seq[int] {
  var out: seq[int];
  var i: int;
  var j: int;
  var tmp: int;

  out = xs;
  i = 1;
  while (i < sizeof(out)) {
    j = i;
    while (j > 0 && out[j] < out[j - 1]) {
      tmp = out[j];
      out[j] = out[j - 1];
      out[j - 1] = tmp;
      j = j - 1;
    }
    i = i + 1;
  }
  return out;
}

fun SortSet(xs: set[int]): seq[int] {
  var out: seq[int];
  var x: int;
  foreach (x in xs) {
    out += (sizeof(out), x);
  }
  return SortSeq(out);
}

// Join renders a list as comma-separated integers, or "-" if it is empty.
fun Join(xs: seq[int]): string {
  var out: string;
  var i: int;

  if (sizeof(xs) == 0) {
    return "-";
  }
  out = format("{0}", xs[0]);
  i = 1;
  while (i < sizeof(xs)) {
    out = format("{0},{1}", out, xs[i]);
    i = i + 1;
  }
  return out;
}
