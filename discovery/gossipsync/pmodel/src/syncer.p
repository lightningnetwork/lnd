// syncer.p states the contract of a peer syncer's historical sync in
// discovery/gossipsync (Idle, AwaitingRange, QueryingSCIDs and Draining in
// syncer_states.go, and the reply accumulator in range_reply.go).
//
// The chain is NumBlocks() abstract blocks. The peer has at most one channel
// per block, and channel c lives in block c - 1. A range query always covers
// the whole chain. The peer answers it with a stream of replies that tile the
// chain, each covering a contiguous block range, the last one marked
// complete. It answers an SCID query with one end message.
//
// The Syncer machine is the component under contract. The World machine is
// everything around it: the peer, the link, the reply timer and the manager.
// World owns every nondeterministic choice. It delivers the link's messages
// in order, may stall the rest of a stream (an honest but lossy peer), fires
// the timer when the timing profile allows, fires stale timers, starts new
// attempts and changes the role. A byzantine profile also duplicates and
// corrupts replies. World waits for the syncer to handle each event before it
// acts again, because an actor handles one message at a time and executes its
// outbox at once. The asynchrony that matters is in the link: replies in
// flight while the timer fires, and a new query sent while an old stream is
// still arriving.
//
// Every query carries a ghost ID that only the model sees, and every
// message the peer sends is tagged with the query it answers and its place in
// that stream. The monitors use the tags to state what the wire cannot: which
// attempt a reply was credited to, and whether a query was still being
// answered when the next one was sent.
//
// Timers are inactivity timers: the reply timer and the drain timer have the
// same timeout, and a message from the peer re-arms whichever is armed. So a
// timer fires with messages still in flight only if the peer paused longer
// than the timeout before the next one, a long pause. With the link empty,
// the peer has sent everything it will, and a timer may always fire. The
// timing profile bounds the long pauses, which is an assumption about the
// peer, not about us:
//
//   PROMPT: no long pause. No honest peer is timed out.
//   SLOW: at most one long pause per stream (assumption A-DRAIN). The pause
//     may time out a live exchange, but the rest of the stream arrives
//     within the timeout of each message, so a drain outlasts it.
//   VERYSLOW: any number of long pauses. A-DRAIN does not hold.
//
// The fixedDrainDeadline profile stands for the drain timer before commit
// that made it an inactivity timer: a deadline from abandonment, which a
// stream of prompt messages can outlast, so it may fire at any time.

enum ReplyFormat { FMT_LND, FMT_LEGACY }

enum Timing { PROMPT, SLOW, VERYSLOW }

// tSyncerProfile selects the production syncer, or a variant that a
// counterexample test case must catch.
//
//   noDraining: a failed attempt goes straight back to Idle.
//   noFirstReplyCheck: the first reply of a stream may start after the
//     query's first block.
//   legacyDrainOnLastBlock: Draining ends a legacy stream on its first
//     reply, since every legacy reply covers the query's last block, which
//     was the rule before the legacy drain was fixed.
//   fixedDrainDeadline: an absorbed reply does not re-arm the drain timer,
//     which is a deadline from abandonment, as it was before the drain timer
//     became an inactivity timer.
type tSyncerProfile = (
  noDraining: bool,
  noFirstReplyCheck: bool,
  legacyDrainOnLastBlock: bool,
  fixedDrainDeadline: bool
);

// tSyncerCfg configures one run. chans are the peer's channels and known
// the ones we already have.
type tSyncerCfg = (
  chans: set[int],
  known: set[int],
  chunk: int,
  batch: int,
  maxReplies: int,
  lookupErr: bool,
  replyFormat: ReplyFormat,
  timing: Timing,
  stalls: bool,
  byzantine: bool,
  profile: tSyncerProfile,
  starts: int,
  steps: int
);

// tMsg is a message from the peer. A range reply covers blocks
// [first, last]. bad marks a reply the byzantine peer corrupted by shifting
// its first block height by one. qid, idx and final are ghost tags: the
// query the message answers, its place in that stream, and whether it ends
// the stream.
type tMsg = (
  isEnd: bool,
  first: int,
  last: int,
  chans: seq[int],
  complete: bool,
  bad: bool,
  qid: int,
  idx: int,
  final: bool
);

// tCredit tags a reply the syncer folded into an attempt.
type tCredit = (qid: int, idx: int);

// Events from World to the syncer.
event eSyStart: int;
event eSySetRole: Role;
event eSyMsg: tMsg;
event eSyTimer: int;

// Events from the syncer to World: its outbox, then an acknowledgement.
event eSyQuery: (isRange: bool, scids: seq[int], qid: int);
event eSyArm: (timer: int, drain: bool);
event eSyDisarm;
event eSyAck;

// World's own step.
event eWStep;

// Announcements the spec monitors observe.
event eAnConfig: tSyncerCfg;
event eAnStarted: int;
event eAnOutcome: (attempt: int, kind: Kind);
event eAnQuery: (attempt: int, isRange: bool, scids: seq[int], qid: int);
event eAnCredit: (attempt: int, want: int, got: int);
event eAnRangeDone: (attempt: int, credits: seq[tCredit]);
event eAnStreamEnded: int;

fun NumBlocks(): int {
  return 4;
}

// IsLegacy reports whether a range reply echoes the whole query, as an old
// lnd peer's replies do. A corrupted reply never does.
fun IsLegacy(m: tMsg): bool {
  return !m.bad && m.first == 0 && m.last == NumBlocks() - 1;
}

machine PeerSyncer {
  var world: machine;
  var cfg: tSyncerCfg;

  var syncType: Role;
  var timerSeq: int;
  var nextQid: int;

  // The attempt in flight, and the ghost ID of its outstanding query.
  var attempt: int;
  var qid: int;

  // The range accumulator: the last block of the previous reply, or -1,
  // the channels buffered, the reply budget used, and the ghost tags of
  // the replies credited.
  var prevLast: int;
  var accChans: seq[int];
  var used: int;
  var credits: seq[tCredit];

  // The SCIDs not yet queried.
  var pending: seq[int];

  // Whether the abandoned exchange is a range query, and how many of its
  // replies were absorbed.
  var drainRange: bool;
  var absorbed: int;

  start state Init {
    entry (p: (world: machine, cfg: tSyncerCfg)) {
      world = p.world;
      cfg = p.cfg;
      syncType = PASSIVE;
      goto Idle;
    }
  }

  state Idle {
    on eSyStart do (a: int) {
      print format("STRACE in start attempt={0}", a);
      announce eAnStarted, a;
      attempt = a;
      prevLast = -1;
      accChans = default(seq[int]);
      used = 0;
      credits = default(seq[tCredit]);
      SendRange();
      Arm(false);
      Ack();
      goto AwaitingRange;
    }

    // A reply we didn't ask for, or a timer of an ended exchange, changes
    // nothing.
    on eSyMsg do (m: tMsg) {
      PrintMsg(m);
      Ack();
    }

    on eSyTimer do (s: int) {
      print format("STRACE in timer seq={0}", s);
      Ack();
    }

    on eSySetRole do (r: Role) {
      SetType(r);
      Ack();
    }
  }

  state AwaitingRange {
    on eSyMsg do (m: tMsg) {
      PrintMsg(m);
      if (m.isEnd) {
        Ack();
        return;
      }
      OnReply(m);
    }

    on eSyTimer do (s: int) {
      print format("STRACE in timer seq={0}", s);
      if (s != timerSeq) {
        Ack();
        return;
      }
      AbandonRange();
    }

    on eSyStart do (a: int) {
      Refuse(a);
    }

    on eSySetRole do (r: Role) {
      SetType(r);
      Ack();
    }
  }

  state QueryingSCIDs {
    on eSyMsg do (m: tMsg) {
      PrintMsg(m);

      // A range reply can't belong to this exchange.
      if (!m.isEnd) {
        Ack();
        return;
      }

      announce eAnCredit, (attempt = attempt, want = qid, got = m.qid);
      if (sizeof(pending) == 0) {
        Disarm();
        Outcome(attempt, COMPLETED);
        Ack();
        goto Idle;
      }

      SendNextBatch();
      Arm(false);
      Ack();
    }

    on eSyTimer do (s: int) {
      print format("STRACE in timer seq={0}", s);
      if (s != timerSeq) {
        Ack();
        return;
      }

      // The peer owes us an end message, which we absorb if it arrives.
      Outcome(attempt, PEERFAULT);
      if (cfg.profile.noDraining) {
        Disarm();
        Ack();
        goto Idle;
      }
      drainRange = false;
      absorbed = 0;
      Arm(true);
      Ack();
      goto Draining;
    }

    on eSyStart do (a: int) {
      Refuse(a);
    }

    on eSySetRole do (r: Role) {
      SetType(r);
      Ack();
    }
  }

  // Draining absorbs the rest of an abandoned exchange, so that no reply
  // can be credited to a later attempt.
  state Draining {
    on eSyMsg do (m: tMsg) {
      var ended: bool;

      PrintMsg(m);

      if (m.isEnd) {
        if (drainRange) {
          Ack();
          return;
        }
        Disarm();
        Ack();
        goto Idle;
      }

      if (!drainRange) {
        Ack();
        return;
      }

      // The abandoned stream ends with a reply that sets complete, or,
      // unless the peer uses the legacy format, covers the last block. The
      // budget bounds a peer that sends neither.
      absorbed = absorbed + 1;
      ended = m.complete;
      if (!IsLegacy(m) || cfg.profile.legacyDrainOnLastBlock) {
        ended = ended || m.last >= NumBlocks() - 1;
      }
      if (ended || absorbed >= cfg.maxReplies) {
        Disarm();
        Ack();
        goto Idle;
      }

      // The drain timer is an inactivity timer, so an absorbed reply
      // re-arms it.
      if (!cfg.profile.fixedDrainDeadline) {
        Arm(true);
      }
      Ack();
    }

    on eSyTimer do (s: int) {
      print format("STRACE in timer seq={0}", s);
      if (s != timerSeq) {
        Ack();
        return;
      }
      Disarm();
      Ack();
      goto Idle;
    }

    on eSyStart do (a: int) {
      Refuse(a);
    }

    on eSySetRole do (r: Role) {
      SetType(r);
      Ack();
    }
  }

  // OnReply folds a range reply into the accumulator, and moves on once the
  // stream is complete.
  fun OnReply(m: tMsg) {
    var legacy: bool;
    var done: bool;
    var c: int;
    var missing: seq[int];

    // A reply that echoes the whole query is in the legacy format, and
    // only its complete flag ends the stream.
    legacy = IsLegacy(m);

    if (!legacy) {
      // A corrupted reply never lines up with the query or the previous
      // reply, since its first height is one past a block boundary.
      if (m.bad) {
        AbandonRange();
        return;
      }

      if (prevLast < 0) {
        if (m.first != 0 && !cfg.profile.noFirstReplyCheck) {
          AbandonRange();
          return;
        }
      } else if (m.first != prevLast + 1) {
        AbandonRange();
        return;
      }
    }

    announce eAnCredit, (attempt = attempt, want = qid, got = m.qid);
    credits += (sizeof(credits), (qid = m.qid, idx = m.idx));
    foreach (c in m.chans) {
      accChans += (sizeof(accChans), c);
    }
    used = used + 1;
    prevLast = m.last;

    if (used >= cfg.maxReplies) {
      done = true;
    } else if (legacy) {
      done = m.complete;
    } else {
      done = m.last >= NumBlocks() - 1;
    }

    if (!done) {
      Arm(false);
      Ack();
      return;
    }

    announce eAnRangeDone, (attempt = attempt, credits = credits);

    if (cfg.lookupErr) {
      Disarm();
      Outcome(attempt, LOCALFAULT);
      Ack();
      goto Idle;
    }

    foreach (c in accChans) {
      if (!(c in cfg.known)) {
        missing += (sizeof(missing), c);
      }
    }

    if (sizeof(missing) == 0) {
      Disarm();
      Outcome(attempt, COMPLETED);
      Ack();
      goto Idle;
    }

    pending = missing;
    SendNextBatch();
    Arm(false);
    Ack();
    goto QueryingSCIDs;
  }

  // AbandonRange fails the attempt and drains the rest of the stream.
  fun AbandonRange() {
    Outcome(attempt, PEERFAULT);
    if (cfg.profile.noDraining) {
      Disarm();
      Ack();
      goto Idle;
    }
    drainRange = true;
    absorbed = 0;
    Arm(true);
    Ack();
    goto Draining;
  }

  // Refuse turns away an attempt while an exchange is in flight.
  fun Refuse(a: int) {
    print format("STRACE in start attempt={0}", a);
    announce eAnStarted, a;
    Outcome(a, BUSY);
    Ack();
  }

  fun SendRange() {
    nextQid = nextQid + 1;
    qid = nextQid;
    print "STRACE out send_range";
    announce eAnQuery, (attempt = attempt, isRange = true,
      scids = default(seq[int]), qid = qid);
    send world, eSyQuery, (isRange = true, scids = default(seq[int]),
      qid = qid);
  }

  // SendNextBatch queries the next batch of pending SCIDs, in stream order.
  fun SendNextBatch() {
    var batch: seq[int];
    var rest: seq[int];
    var i: int;

    i = 0;
    while (i < sizeof(pending)) {
      if (i < cfg.batch) {
        batch += (sizeof(batch), pending[i]);
      } else {
        rest += (sizeof(rest), pending[i]);
      }
      i = i + 1;
    }
    pending = rest;

    nextQid = nextQid + 1;
    qid = nextQid;
    print format("STRACE out send_scids scids={0}", Join(batch));
    announce eAnQuery, (attempt = attempt, isRange = false, scids = batch,
      qid = qid);
    send world, eSyQuery, (isRange = false, scids = batch, qid = qid);
  }

  fun Arm(drain: bool) {
    timerSeq = timerSeq + 1;
    print format("STRACE out arm seq={0} drain={1}", timerSeq, B(drain));
    send world, eSyArm, (timer = timerSeq, drain = drain);
  }

  fun Disarm() {
    print "STRACE out disarm";
    send world, eSyDisarm;
  }

  fun Outcome(a: int, k: Kind) {
    print format("STRACE out outcome attempt={0} kind={1}", a, k to int);
    announce eAnOutcome, (attempt = a, kind = k);
  }

  // SetType changes the role, and sends a timestamp filter if the new role
  // differs from the old in whether it wants gossip.
  fun SetType(r: Role) {
    var wanted: bool;
    var had: bool;

    print format("STRACE in role role={0}", r to int);
    if (r == syncType) {
      return;
    }
    wanted = r != PASSIVE;
    had = syncType != PASSIVE;
    syncType = r;
    if (wanted != had) {
      print format("STRACE out filter wants={0}", B(wanted));
    }
  }

  fun Ack() {
    send world, eSyAck;
  }

  fun PrintMsg(m: tMsg) {
    if (m.isEnd) {
      print "STRACE in end";
      return;
    }
    print format("STRACE in range {0} {1}",
      format("first={0} last={1} chans={2}", m.first, m.last,
        Join(m.chans)),
      format("complete={0} bad={1}", B(m.complete), B(m.bad)));
  }
}

// World is the peer, the link, the reply timer and the manager.
machine World {
  var cfg: tSyncerCfg;
  var syncer: machine;

  // link holds the peer's messages in flight, in order. remaining counts,
  // per query, the messages of its stream still on the link.
  var link: seq[tMsg];
  var remaining: map[int, int];

  var nextAttempt: int;
  var steps: int;
  var dups: int;
  var corrupts: int;

  // pauses counts, per query, the long pauses the peer took in its stream.
  var pauses: map[int, int];

  // The last timer armed, and whether it is still armed.
  var armSeq: int;
  var armDrain: bool;
  var armed: bool;

  start state Init {
    entry (c: tSyncerCfg) {
      cfg = c;
      print format("STRACE begin {0} {1}",
        format("chans={0} known={1}", Join(SortSet(cfg.chans)),
          Join(SortSet(cfg.known))),
        format("batch={0} max_replies={1} lookup_err={2}", cfg.batch,
          cfg.maxReplies, B(cfg.lookupErr)));
      announce eAnConfig, cfg;
      syncer = new PeerSyncer((world = this, cfg = cfg));
      send this, eWStep;
      goto Run;
    }
  }

  state Run {
    on eWStep do {
      Act();
    }
  }

  // Waiting collects the syncer's outbox until it acknowledges the event.
  state Waiting {
    defer eWStep;

    on eSyQuery do (q: (isRange: bool, scids: seq[int], qid: int)) {
      Answer(q.isRange, q.scids, q.qid);
    }

    on eSyArm do (a: (timer: int, drain: bool)) {
      armSeq = a.timer;
      armDrain = a.drain;
      armed = true;
    }

    on eSyDisarm do {
      armed = false;
    }

    on eSyAck do {
      send this, eWStep;
      goto Run;
    }
  }

  // Act performs one step: a random action while the step budget lasts,
  // then the settle phase, which delivers everything and fires every timer
  // until the syncer is idle.
  fun Act() {
    var acts: seq[int];
    var a: int;

    steps = steps + 1;
    if (steps > cfg.steps) {
      Settle();
      return;
    }

    if (nextAttempt < cfg.starts) {
      acts += (sizeof(acts), 0);
    }
    acts += (sizeof(acts), 1);
    if (sizeof(link) > 0) {
      acts += (sizeof(acts), 2);
      acts += (sizeof(acts), 2);
      acts += (sizeof(acts), 2);
      if (cfg.stalls) {
        acts += (sizeof(acts), 3);
      }
      if (cfg.byzantine && dups < 2) {
        acts += (sizeof(acts), 6);
      }
      if (cfg.byzantine && corrupts < 2 && !link[0].isEnd &&
        !link[0].bad) {

        acts += (sizeof(acts), 7);
      }
    }
    if (armed && TimerMayFire()) {
      acts += (sizeof(acts), 4);
    }
    if (armSeq >= 2) {
      acts += (sizeof(acts), 5);
    }

    a = acts[choose(sizeof(acts))];
    if (a == 0) {
      nextAttempt = nextAttempt + 1;
      ToSyncer(eSyStart, nextAttempt);
    } else if (a == 1) {
      if (choose(2) == 0) {
        ToSyncer(eSySetRole, PASSIVE);
      } else if ($) {
        ToSyncer(eSySetRole, ACTIVE);
      } else {
        ToSyncer(eSySetRole, PINNED);
      }
    } else if (a == 2) {
      Deliver();
    } else if (a == 3) {
      Stall();
      send this, eWStep;
    } else if (a == 4) {
      Fire(armSeq);
    } else if (a == 5) {
      ToSyncer(eSyTimer, armSeq - 1);
    } else if (a == 6) {
      dups = dups + 1;
      Duplicate();
      send this, eWStep;
    } else {
      corrupts = corrupts + 1;
      Corrupt();
      send this, eWStep;
    }
  }

  // Settle delivers what is left and fires the timer until the syncer has
  // nothing in flight. Once the link is empty, every timing profile lets
  // the timer fire.
  fun Settle() {
    if (sizeof(link) > 0) {
      Deliver();
      return;
    }
    if (armed) {
      Fire(armSeq);
    }
  }

  // TimerMayFire applies the timing profile to the armed timer. With the
  // link empty it may always fire. Otherwise it fires only if the peer takes
  // a long pause before the message at the head of the link, which the
  // profile bounds per stream.
  fun TimerMayFire(): bool {
    if (sizeof(link) == 0) {
      return true;
    }
    if (armDrain && cfg.profile.fixedDrainDeadline) {
      return true;
    }
    if (cfg.timing == VERYSLOW) {
      return true;
    }
    return cfg.timing == SLOW && pauses[link[0].qid] == 0;
  }

  fun Fire(s: int) {
    if (sizeof(link) > 0) {
      pauses[link[0].qid] = pauses[link[0].qid] + 1;
    }
    armed = false;
    ToSyncer(eSyTimer, s);
  }

  fun ToSyncer(e: event, payload: any) {
    send syncer, e, payload;
    goto Waiting;
  }

  fun Deliver() {
    var m: tMsg;
    m = link[0];
    link -= (0);
    Consumed(m.qid, 1);
    ToSyncer(eSyMsg, m);
  }

  // Stall drops the rest of the stream at the head of the link: the peer
  // stops answering that query.
  fun Stall() {
    var q: int;
    var kept: seq[tMsg];
    var n: int;
    var i: int;

    q = link[0].qid;
    i = 0;
    while (i < sizeof(link)) {
      if (link[i].qid == q) {
        n = n + 1;
      } else {
        kept += (sizeof(kept), link[i]);
      }
      i = i + 1;
    }
    link = kept;
    Consumed(q, n);
  }

  fun Duplicate() {
    link += (0, link[0]);
    remaining[link[0].qid] = remaining[link[0].qid] + 1;
  }

  fun Corrupt() {
    var m: tMsg;
    m = link[0];
    m.bad = true;
    link[0] = m;
  }

  // Consumed records that n messages of query q left the link, and
  // announces the end of its stream when the last one does.
  fun Consumed(q: int, n: int) {
    remaining[q] = remaining[q] - n;
    if (remaining[q] == 0) {
      announce eAnStreamEnded, q;
    }
  }

  // Answer queues the peer's answer to a query.
  fun Answer(isRange: bool, scids: seq[int], q: int) {
    var stream: seq[tMsg];
    var m: tMsg;

    if (isRange) {
      stream = RangeStream(q);
    } else {
      stream += (0, (isEnd = true, first = 0, last = 0,
        chans = default(seq[int]), complete = true, bad = false, qid = q,
        idx = 0, final = true));
    }

    remaining[q] = sizeof(stream);
    pauses[q] = 0;
    foreach (m in stream) {
      link += (sizeof(link), m);
    }
  }

  // RangeStream returns the replies an honest peer sends for the whole
  // chain: our responder's chunking, with at most chunk channels a reply.
  // A legacy peer sends the same chunks, but every reply echoes the whole
  // query.
  fun RangeStream(q: int): seq[tMsg] {
    var out: seq[tMsg];
    var chunk: seq[int];
    var first: int;
    var b: int;

    first = 0;
    b = 0;
    while (b < NumBlocks()) {
      if ((b + 1) in cfg.chans) {
        if (sizeof(chunk) < cfg.chunk) {
          chunk += (sizeof(chunk), b + 1);
        } else {
          out += (sizeof(out), Reply(q, sizeof(out), first, b - 1,
            chunk, false));
          first = b;
          chunk = default(seq[int]);
          chunk += (0, b + 1);
        }
      }
      b = b + 1;
    }
    out += (sizeof(out), Reply(q, sizeof(out), first, NumBlocks() - 1,
      chunk, true));

    return out;
  }

  fun Reply(q: int, idx: int, first: int, last: int, chans: seq[int],
    final: bool): tMsg {

    if (cfg.replyFormat == FMT_LEGACY) {
      first = 0;
      last = NumBlocks() - 1;
    }
    return (isEnd = false, first = first, last = last, chans = chans,
      complete = final, bad = false, qid = q, idx = idx, final = final);
  }
}

// OneOutstandingQuery: we never send a query while a query of the same kind
// is still being answered, which BOLT 7 lets the peer punish by closing the
// connection.
spec OneOutstandingQuery observes eAnQuery, eAnStreamEnded {
  var ranges: set[int];
  var scids: set[int];

  start state Watch {
    on eAnQuery do (q: (attempt: int, isRange: bool, scids: seq[int],
      qid: int)) {

      if (q.isRange) {
        assert sizeof(ranges) == 0,
          format("range query {0} sent while {1} is outstanding", q.qid,
            ranges);
        ranges += (q.qid);
      } else {
        assert sizeof(scids) == 0,
          format("scid query {0} sent while {1} is outstanding", q.qid,
            scids);
        scids += (q.qid);
      }
    }

    on eAnStreamEnded do (q: int) {
      if (q in ranges) {
        ranges -= (q);
      }
      if (q in scids) {
        scids -= (q);
      }
    }
  }
}

// NoCrossAttemptCredit: every reply folded into an attempt answers that
// attempt's own outstanding query.
spec NoCrossAttemptCredit observes eAnCredit {
  start state Watch {
    on eAnCredit do (c: (attempt: int, want: int, got: int)) {
      assert c.want == c.got,
        format("attempt {0} credited a reply to query {1}, its query is {2}",
          c.attempt, c.got, c.want);
    }
  }
}

// WholeStreamCredit: a range phase only completes on one whole stream, the
// replies of a single query from its first reply on, in order.
spec WholeStreamCredit observes eAnRangeDone {
  start state Watch {
    on eAnRangeDone do (d: (attempt: int, credits: seq[tCredit])) {
      var i: int;

      i = 0;
      while (i < sizeof(d.credits)) {
        assert d.credits[i].qid == d.credits[0].qid &&
          d.credits[i].idx == i,
          format("attempt {0} completed its range phase on {1}", d.attempt,
            d.credits);
        i = i + 1;
      }
    }
  }
}

// OutcomeExactlyOnce: every started attempt, refused ones included, ends
// with exactly one outcome. The monitor is hot while an attempt has none,
// and must be cold when the system quiesces.
spec OutcomeExactlyOnce observes eAnStarted, eAnOutcome {
  var open: set[int];
  var done: set[int];

  start cold state Quiet {
    on eAnStarted do (a: int) {
      OpenAttempt(a);
      goto Open;
    }

    on eAnOutcome do (o: (attempt: int, kind: Kind)) {
      CloseAttempt(o.attempt);
    }
  }

  hot state Open {
    on eAnStarted do (a: int) {
      OpenAttempt(a);
    }

    on eAnOutcome do (o: (attempt: int, kind: Kind)) {
      CloseAttempt(o.attempt);
      if (sizeof(open) == 0) {
        goto Quiet;
      }
    }
  }

  fun OpenAttempt(a: int) {
    assert !(a in open) && !(a in done),
      format("attempt {0} started twice", a);
    open += (a);
  }

  fun CloseAttempt(a: int) {
    assert a in open, format("outcome for attempt {0}, which is not open", a);
    open -= (a);
    done += (a);
  }
}

// CompletedQueriedMissing: a completed attempt queried exactly the channels
// the peer has and we lack. It holds for an honest peer whose streams fit in
// the reply budget.
spec CompletedQueriedMissing observes eAnConfig, eAnQuery, eAnOutcome {
  var want: set[int];
  var queried: map[int, set[int]];

  start state Watch {
    on eAnConfig do (c: tSyncerCfg) {
      var ch: int;
      foreach (ch in c.chans) {
        if (!(ch in c.known)) {
          want += (ch);
        }
      }
    }

    on eAnQuery do (q: (attempt: int, isRange: bool, scids: seq[int],
      qid: int)) {

      var ch: int;
      if (!(q.attempt in queried)) {
        queried[q.attempt] = default(set[int]);
      }
      foreach (ch in q.scids) {
        queried[q.attempt] += (ch);
      }
    }

    on eAnOutcome do (o: (attempt: int, kind: Kind)) {
      var got: set[int];

      if (o.kind != COMPLETED) {
        return;
      }
      if (o.attempt in queried) {
        got = queried[o.attempt];
      }
      assert got == want,
        format("attempt {0} completed after querying {1}, not {2}",
          o.attempt, got, want);
    }
  }
}

// PromptPeerNeverFaulted: with a prompt peer that never stalls, no attempt
// ends in a peer fault, so the syncer's own timers never fail an honest
// exchange.
spec PromptPeerNeverFaulted observes eAnOutcome {
  start state Watch {
    on eAnOutcome do (o: (attempt: int, kind: Kind)) {
      assert o.kind != PEERFAULT,
        format("attempt {0} failed with a prompt honest peer", o.attempt);
    }
  }
}
