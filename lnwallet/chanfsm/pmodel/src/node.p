// node.p states the contract of one side of a channel's BOLT 2 commitment
// protocol, as lnwallet/chanfsm runs it: the ledger (ledger.go) and the
// state machine around it (states.go).
//
// A Node holds the ledger: its own update log and its peer's, and its own
// commitment chain and its peer's, each a tail and at most one pending
// commitment. Updates are abstract: no amounts, scripts or signatures. A
// commitment is the pair of log indexes up to which it includes each log,
// and a height. The Node checks every command and peer message against the
// ledger before it applies it, exactly as the Go state machine checks an
// event before it authorizes the channel operation, and it fails the channel
// on any protocol violation.
//
// Signatures and revocation secrets are abstract too. A commitment_signed
// carries the height and the indexes of the commitment the signer built, and
// the commitment point it built it with, as ghost fields; the receiver
// accepts it only if they are the commitment it builds itself, which is what
// a valid signature means. A revoke_and_ack carries the height whose secret
// it reveals and the height of the next point. The receiver tracks the
// heights of the peer's current and next points, and accepts a secret only
// for the current one, which is lnd's check against RemoteCurrentRevocation.
//
// The profile selects the production node or a variant that a
// counterexample test case must catch.

enum Kind { K_ADD, K_SETTLE, K_FAIL, K_MALFORMED, K_FEE }

enum MsgKind {
  M_ADD, M_SETTLE, M_FAIL, M_MALFORMED, M_FEE, M_SIG, M_RAA, M_REEST
}

enum CmdKind { C_ADD, C_SETTLE, C_FAIL, C_MALFORMED, C_FEE, C_SIGN }

// tEntry is an update log entry. hL and hR are the heights of the first
// commitment on our chain and on the peer's that include it, or 0.
type tEntry = (
  logIndex: int,
  kind: Kind,
  htlc: int,
  parent: int,
  hL: int,
  hR: int
);

type tLog = (
  logIndex: int,
  htlcCounter: int,
  entries: seq[tEntry],
  modified: set[int]
);

// tCommit is a commitment: its height, and the log indexes below which it
// includes our updates (idxL) and the peer's (idxR), from the owner's point
// of view of the ledger holding it.
type tCommit = (height: int, idxL: int, idxR: int);

// tMsg is a message between the nodes. id names the HTLC of an add or a
// removal. badOnion is the BADONION bit of a malformed fail. A signature
// carries the commitment it signs in the receiver's terms (height, the
// receiver's own updates below own, the sender's below other) and the
// point used. A revocation carries the height it revokes and the height of
// the next point. A channel_reestablish carries next_commitment_number in
// height, and the height of the receiver's current commitment as the sender
// knows it in revoked. forged marks a message a byzantine peer made up.
// tContent is what a commitment holds, as a ghost the signature stands
// for: the fee update that sets its rate (by log index, -1 for the initial
// rate), and the HTLCs on it that each side offered. own is the receiver's,
// other the sender's.
type tContent = (fee: int, own: set[int], other: set[int]);

type tMsg = (
  kind: MsgKind,
  id: int,
  badOnion: bool,
  height: int,
  own: int,
  other: int,
  point: int,
  revoked: int,
  next: int,
  forged: bool,
  content: tContent
);

// tProfile selects variants of the node.
//
//   legacyRevocation: a revoke_and_ack with no commitment pending passes
//     the point check, rotates the peer's points, then fails, as
//     LightningChannel.ReceiveRevocation did without its guard.
//   forwardOnCommit: remote updates are forwarded as soon as our own
//     commitment includes them, before the peer revokes.
//   noFreshness: a revocation forwards every locked-in remote update, not
//     just the ones it locked in.
//   earlyLocalRemoval: we remove an HTLC once our own commitment includes
//     it, without waiting for it to be irrevocably committed.
//   signAllRemote: we sign every remote update we've received, including
//     ones our current commitment doesn't include yet.
//   noResend: channel_reestablish never resends a pending commitment the
//     peer missed.
//   revokeFirst: channel_reestablish always resends our revocation before
//     a pending commitment, whichever we sent first.
//   unpersistedPeerAcked: a restart loses our updates the peer acked but
//     hasn't signed for, as lnd's AdvanceCommitChainTail did before we
//     first revoked a commitment.
type tProfile = (
  legacyRevocation: bool,
  forwardOnCommit: bool,
  noFreshness: bool,
  earlyLocalRemoval: bool,
  signAllRemote: bool,
  noResend: bool,
  revokeFirst: bool,
  unpersistedPeerAcked: bool
);

// tStatus is what a node reports to World after each event.
type tStatus = (failed: bool, remotePending: bool, localTail: int);

// tFingerprint is everything a message could change in a node.
type tFingerprint = (
  lLog: tLog,
  rLog: tLog,
  lTail: tCommit,
  lHas: bool,
  lPend: tCommit,
  rTail: tCommit,
  rHas: bool,
  rPend: tCommit,
  peerCur: int,
  peerNext: int
);

// Events from World to a node, then the node's outbox and acknowledgement.
event eCmd: (cmd: CmdKind, id: int);
event eMsg: tMsg;
event eSend: (from: machine, msg: tMsg);
event eAck: (from: machine, st: tStatus);

// Announcements the spec monitors observe. Each names the node by its
// index: 0 for Alice, 1 for Bob.
event eAnConfig: (initiator: int);
event eAnProposed: (node: int, logIndex: int, isAdd: bool, htlc: int);
event eAnTail: (node: int, commit: tCommit);
event eAnForward: (node: int, kind: Kind, id: int, logIndex: int);
event eAnSendRemoval: (node: int, htlc: int);
event eAnVerify: (node: int, forged: bool, agree: bool);
event eAnRevocation: (node: int, hadPending: bool, refused: bool,
  changed: bool);
event eAnRefused: (node: int, changed: bool);
event eAnFailed: (node: int);
event eAnDropped: (node: int, lo: int);

// eRestart tells a node its connection dropped: it reloads its channel from
// disk and sends its channel_reestablish.
event eRestart;

machine Node {
  var world: machine;
  var me: int;
  var name: string;
  var initiator: bool;
  var profile: tProfile;
  var failed: bool;

  var lLog: tLog;
  var rLog: tLog;
  var lTail: tCommit;
  var lHas: bool;
  var lPend: tCommit;
  var rTail: tCommit;
  var rHas: bool;
  var rPend: tCommit;

  // The heights of the peer's current and next commitment points.
  var peerCur: int;
  var peerNext: int;

  // Whether the last commitment message we sent was a revoke_and_ack.
  var lastWasRevoke: bool;

  // feeHist is the ghost history of the initiator's fee updates, by log
  // index. It survives a restart only for the updates lnd persists.
  var feeHist: set[int];

  start state Init {
    entry (p: (world: machine, me: int, initiator: bool,
      profile: tProfile)) {

      world = p.world;
      me = p.me;
      initiator = p.initiator;
      profile = p.profile;
      name = "alice";
      if (me == 1) {
        name = "bob";
      }
      peerCur = 0;
      peerNext = 1;
      print format("CTRACE {0} begin initiator={1}", name, B(initiator));
      goto Synced;
    }
  }

  // Synced: the peer has no commitment from us to revoke its current one
  // for. A revoke_and_ack is a protocol violation, which fails the channel
  // before anything is applied.
  state Synced {
    on eRestart do {
      OnRestart();
    }

    on eCmd do (c: (cmd: CmdKind, id: int)) {
      OnCmd(c.cmd, c.id);
    }

    on eMsg do (m: tMsg) {
      var before: tFingerprint;

      PrintMsg(m);
      if (m.kind != M_RAA) {
        OnMsg(m);
        return;
      }

      before = Fingerprint();
      if (profile.legacyRevocation && m.revoked == peerCur) {
        peerCur = peerNext;
        peerNext = m.next;
      }
      announce eAnRevocation, (node = me, hadPending = false,
        refused = true, changed = before != Fingerprint());
      Fail();
    }
  }

  // AwaitingRevocation: the peer holds a commitment we signed, and its
  // next revoke_and_ack completes it.
  state AwaitingRevocation {
    on eRestart do {
      OnRestart();
    }

    on eCmd do (c: (cmd: CmdKind, id: int)) {
      OnCmd(c.cmd, c.id);
    }

    on eMsg do (m: tMsg) {
      PrintMsg(m);
      if (m.kind != M_RAA) {
        OnMsg(m);
        return;
      }

      // The channel checks the secret against the peer's current point.
      if (m.revoked != peerCur) {
        announce eAnRevocation, (node = me, hadPending = true,
          refused = true, changed = false);
        Fail();
        return;
      }
      announce eAnRevocation, (node = me, hadPending = true,
        refused = false, changed = true);
      peerCur = peerNext;
      peerNext = m.next;
      ReceiveRevocation();
      SignIfOwed();
      Done();
    }
  }

  // Reestablishing: the channel was reloaded from disk and sent its
  // channel_reestablish. Only the peer's channel_reestablish moves it on.
  state Reestablishing {
    on eRestart do {
      OnRestart();
    }

    // A command waits until the channel is reestablished: refused, and the
    // node stays here.
    on eCmd do (c: (cmd: CmdKind, id: int)) {
      print format("CTRACE {0} in {1}", name, CmdName(c.cmd, c.id));
      print format("CTRACE {0} out reply err", name);
      send world, eAck, (from = this, st = (failed = false,
        remotePending = rHas, localTail = lTail.height));
    }

    on eMsg do (m: tMsg) {
      PrintMsg(m);
      if (m.kind != M_REEST) {
        Refuse(Fingerprint());
        return;
      }
      OnReestablish(m);
    }
  }

  state Failed {
    on eRestart do {
      print format("CTRACE {0} in restart", name);
      Done();
    }

    on eCmd do (c: (cmd: CmdKind, id: int)) {
      print format("CTRACE {0} in {1}", name, CmdName(c.cmd, c.id));
      print format("CTRACE {0} out reply err", name);
      Done();
    }

    on eMsg do (m: tMsg) {
      PrintMsg(m);
      Done();
    }
  }

  // Done reports the node's status to World, and rests in the state the
  // ledger calls for.
  fun Done() {
    send world, eAck, (from = this, st = (failed = failed,
      remotePending = rHas, localTail = lTail.height));
    if (failed) {
      goto Failed;
    } else if (rHas) {
      goto AwaitingRevocation;
    } else {
      goto Synced;
    }
  }

  fun Fail() {
    announce eAnFailed, (node = me,);
    failed = true;
    print format("CTRACE {0} out fail", name);
    Done();
  }

  // Refuse fails the channel for a peer message the ledger doesn't allow.
  fun Refuse(before: tFingerprint) {
    announce eAnRefused, (node = me, changed = before != Fingerprint());
    Fail();
  }

  fun OnCmd(cmd: CmdKind, id: int) {
    var err: bool;

    print format("CTRACE {0} in {1}", name, CmdName(cmd, id));
    if (cmd == C_ADD) {
      AppendAdd(true);
      SendMsg(M_ADD, lLog.htlcCounter - 1);
      Reply(true);
    } else if (cmd == C_SIGN) {
      if (!rHas && Owe()) {
        SignRemote();
      }
      Reply(true);
    } else if (cmd == C_FEE) {
      if (!initiator) {
        Reply(false);
        return;
      }
      UpdateFee(true);
      SendMsg(M_FEE, 0);
      Reply(true);
    } else {
      err = RemovalErr(true, id);
      if (err) {
        Reply(false);
        return;
      }
      announce eAnSendRemoval, (node = me, htlc = id);
      if (cmd == C_SETTLE) {
        AppendRemoval(true, K_SETTLE, id);
        SendMsg(M_SETTLE, id);
      } else if (cmd == C_FAIL) {
        AppendRemoval(true, K_FAIL, id);
        SendMsg(M_FAIL, id);
      } else {
        AppendRemoval(true, K_MALFORMED, id);
        SendMsg(M_MALFORMED, id);
      }
      Reply(true);
    }
  }

  fun Reply(ok: bool) {
    if (ok) {
      print format("CTRACE {0} out reply ok", name);
    } else {
      print format("CTRACE {0} out reply err", name);
    }
    Done();
  }

  fun OnMsg(m: tMsg) {
    var before: tFingerprint;

    before = Fingerprint();
    if (m.kind == M_REEST) {
      Refuse(before);
      return;
    } else if (m.kind == M_ADD) {
      if (m.id != rLog.htlcCounter) {
        Refuse(before);
        return;
      }
      AppendAdd(false);
    } else if (m.kind == M_FEE) {
      if (initiator) {
        Refuse(before);
        return;
      }
      UpdateFee(false);
    } else if (m.kind == M_SIG) {
      ReceiveCommitment(m);
      return;
    } else {
      if (m.kind == M_MALFORMED && !m.badOnion) {
        Refuse(before);
        return;
      }
      if (RemovalErr(false, m.id)) {
        Refuse(before);
        return;
      }

      // lnd records the peer's malformed fail as a plain fail.
      if (m.kind == M_SETTLE) {
        AppendRemoval(false, K_SETTLE, m.id);
      } else {
        AppendRemoval(false, K_FAIL, m.id);
      }
    }
    Done();
  }

  // ReceiveCommitment builds the commitment the peer should have signed,
  // checks the signature against it, then revokes our previous one and
  // signs back if we owe, as the link does.
  fun ReceiveCommitment(m: tMsg) {
    var c: tCommit;
    var agree: bool;

    c = (height = lTail.height + 1, idxL = rTail.idxL,
      idxR = rLog.logIndex);
    agree = m.height == c.height && m.own == c.idxL &&
      m.other == c.idxR && m.point == c.height &&
      (m.forged || m.content == Content(c));
    announce eAnVerify, (node = me, forged = m.forged, agree = agree);
    if (!agree) {
      Fail();
      return;
    }

    Extend(true, c);
    ForwardOnCommit(c.height);
    lTail = lPend;
    lastWasRevoke = true;
    lHas = false;
    announce eAnTail, (node = me, commit = lTail);
    SendRAA(c.height - 1, c.height + 1);
    SignIfOwed();
    Done();
  }

  fun SignIfOwed() {
    if (!rHas && Owe()) {
      SignRemote();
    }
  }

  // SignRemote signs a new commitment for the peer: all of our updates, and
  // the peer's updates our current commitment includes.
  fun SignRemote() {
    var c: tCommit;

    lastWasRevoke = false;
    c = (height = rTail.height + 1, idxL = lLog.logIndex,
      idxR = lTail.idxR);
    if (profile.signAllRemote) {
      c.idxR = rLog.logIndex;
    }
    Extend(false, c);
    print format("CTRACE {0} out send sig", name);
    send world, eSend, (from = this, msg = (kind = M_SIG, id = 0,
      badOnion = false, height = c.height, own = c.idxR, other = c.idxL,
      point = peerNext, revoked = 0, next = 0, forged = false,
      content = Theirs(Content(c))));
  }

  // Extend stamps every entry the new commitment newly includes with its
  // height, and makes it the chain's pending commitment.
  fun Extend(local: bool, c: tCommit) {
    lLog = Stamp(lLog, local, c.height, c.idxL);
    rLog = Stamp(rLog, local, c.height, c.idxR);
    if (local) {
      lPend = c;
      lHas = true;
    } else {
      rPend = c;
      rHas = true;
    }
  }

  fun Stamp(log: tLog, local: bool, h: int, below: int): tLog {
    var i: int;
    var e: tEntry;

    i = 0;
    while (i < sizeof(log.entries)) {
      e = log.entries[i];
      if (e.logIndex < below) {
        if (local && e.hL == 0) {
          e.hL = h;
        }
        if (!local && e.hR == 0) {
          e.hR = h;
        }
        log.entries[i] = e;
      }
      i = i + 1;
    }

    return log;
  }

  // ReceiveRevocation advances the peer's chain, forwards the peer's updates
  // it locks in, and compacts the logs.
  fun ReceiveRevocation() {
    var i: int;
    var e: tEntry;
    var fresh: bool;

    rTail = rPend;
    rHas = false;

    i = 0;
    while (i < sizeof(rLog.entries)) {
      e = rLog.entries[i];
      fresh = e.hR == rTail.height;
      if (profile.noFreshness) {
        fresh = e.hR != 0 && e.hR <= rTail.height;
      }
      if (profile.forwardOnCommit) {
        fresh = false;
      }
      if (e.kind != K_FEE && fresh && Committed(e, true)) {
        Forward(e);
      }
      i = i + 1;
    }

    Compact();
  }

  // ForwardOnCommit is the forwardOnCommit variant's early forwarding.
  fun ForwardOnCommit(h: int) {
    var i: int;
    var e: tEntry;

    if (!profile.forwardOnCommit) {
      return;
    }
    i = 0;
    while (i < sizeof(rLog.entries)) {
      e = rLog.entries[i];
      if (e.kind != K_FEE && e.hL == h) {
        Forward(e);
      }
      i = i + 1;
    }
  }

  fun Forward(e: tEntry) {
    var id: int;

    id = e.htlc;
    if (e.kind != K_ADD) {
      id = e.parent;
    }
    print format("CTRACE {0} out fwd {1} id={2}", name, KindName(e.kind),
      id);
    announce eAnForward, (node = me, kind = e.kind, id = id,
      logIndex = e.logIndex);
  }

  // Compact drops the removals and fee updates both current commitments
  // include, and the adds those removals remove.
  fun Compact() {
    var gone: set[int];
    var goneR: set[int];

    gone = Done1(lLog);
    goneR = Done1(rLog);
    lLog = DropDone(lLog);
    rLog = DropDone(rLog);
    rLog = DropAdds(rLog, gone);
    lLog = DropAdds(lLog, goneR);
  }

  // Done1 returns the parents of the log's compactable removals.
  fun Done1(log: tLog): set[int] {
    var i: int;
    var e: tEntry;
    var gone: set[int];

    i = 0;
    while (i < sizeof(log.entries)) {
      e = log.entries[i];
      if (e.kind != K_ADD && e.kind != K_FEE && LockedIn(e)) {
        gone += (e.parent);
      }
      i = i + 1;
    }

    return gone;
  }

  fun DropDone(log: tLog): tLog {
    var i: int;
    var e: tEntry;

    i = sizeof(log.entries) - 1;
    while (i >= 0) {
      e = log.entries[i];
      if (e.kind != K_ADD && LockedIn(e)) {
        log.entries -= (i);
      }
      i = i - 1;
    }

    return log;
  }

  fun DropAdds(log: tLog, gone: set[int]): tLog {
    var i: int;
    var e: tEntry;

    i = sizeof(log.entries) - 1;
    while (i >= 0) {
      e = log.entries[i];
      if (e.kind == K_ADD && e.htlc in gone) {
        log.entries -= (i);
        if (e.htlc in log.modified) {
          log.modified -= (e.htlc);
        }
      }
      i = i - 1;
    }

    return log;
  }

  fun Committed(e: tEntry, local: bool): bool {
    if (local) {
      return e.hL != 0 && e.hL <= lTail.height;
    }

    return e.hR != 0 && e.hR <= rTail.height;
  }

  fun LockedIn(e: tEntry): bool {
    return Committed(e, true) && Committed(e, false);
  }

  // RemovalErr reports whether a removal of the given HTLC is refused. Our
  // own removals need the HTLC irrevocably committed, the peer's need it in
  // our current commitment (BOLT 2's sender and receiver rules).
  fun RemovalErr(local: bool, id: int): bool {
    var log: tLog;
    var i: int;
    var e: tEntry;

    log = lLog;
    if (local) {
      log = rLog;
    }
    if (id in log.modified) {
      return true;
    }

    i = 0;
    while (i < sizeof(log.entries)) {
      e = log.entries[i];
      if (e.kind == K_ADD && e.htlc == id) {
        if (!local) {
          return !Committed(e, true);
        }
        if (profile.earlyLocalRemoval) {
          return !Committed(e, true);
        }
        return !LockedIn(e);
      }
      i = i + 1;
    }

    return true;
  }

  fun AppendAdd(local: bool) {
    if (local) {
      announce eAnProposed, (node = me, logIndex = lLog.logIndex,
        isAdd = true, htlc = lLog.htlcCounter);
      lLog.entries += (sizeof(lLog.entries), (logIndex = lLog.logIndex,
        kind = K_ADD, htlc = lLog.htlcCounter, parent = 0, hL = 0,
        hR = 0));
      lLog.logIndex = lLog.logIndex + 1;
      lLog.htlcCounter = lLog.htlcCounter + 1;
    } else {
      rLog.entries += (sizeof(rLog.entries), (logIndex = rLog.logIndex,
        kind = K_ADD, htlc = rLog.htlcCounter, parent = 0, hL = 0,
        hR = 0));
      rLog.logIndex = rLog.logIndex + 1;
      rLog.htlcCounter = rLog.htlcCounter + 1;
    }
  }

  fun AppendRemoval(local: bool, k: Kind, id: int) {
    if (local) {
      announce eAnProposed, (node = me, logIndex = lLog.logIndex,
        isAdd = false, htlc = id);
      lLog.entries += (sizeof(lLog.entries), (logIndex = lLog.logIndex,
        kind = k, htlc = 0, parent = id, hL = 0, hR = 0));
      lLog.logIndex = lLog.logIndex + 1;
      rLog.modified += (id);
    } else {
      rLog.entries += (sizeof(rLog.entries), (logIndex = rLog.logIndex,
        kind = k, htlc = 0, parent = id, hL = 0, hR = 0));
      rLog.logIndex = rLog.logIndex + 1;
      lLog.modified += (id);
    }
  }

  // UpdateFee appends a fee update, unless the log's latest fee update is
  // in no commitment yet, which it replaces, as lnd does.
  fun UpdateFee(local: bool) {
    var log: tLog;
    var i: int;
    var e: tEntry;

    log = rLog;
    if (local) {
      log = lLog;
    }
    i = sizeof(log.entries) - 1;
    while (i >= 0) {
      e = log.entries[i];
      if (e.kind == K_FEE) {
        if (e.hL == 0 && e.hR == 0) {
          return;
        }
        i = -1;
      }
      i = i - 1;
    }

    if (local) {
      announce eAnProposed, (node = me, logIndex = lLog.logIndex,
        isAdd = false, htlc = -1);
    }
    feeHist += (log.logIndex);
    log.entries += (sizeof(log.entries), (logIndex = log.logIndex,
      kind = K_FEE, htlc = 0, parent = 0, hL = 0, hR = 0));
    log.logIndex = log.logIndex + 1;
    if (local) {
      lLog = log;
    } else {
      rLog = log;
    }
  }

  // Owe mirrors lnd's oweCommitment for our side.
  fun Owe(): bool {
    var lTip: tCommit;
    var rTip: tCommit;

    lTip = lTail;
    if (lHas) {
      lTip = lPend;
    }
    rTip = rTail;
    if (rHas) {
      rTip = rPend;
    }

    return lLog.logIndex != rTip.idxL || lTip.idxR != rTip.idxR;
  }

  fun SendMsg(k: MsgKind, id: int) {
    print format("CTRACE {0} out send {1} id={2}", name, MsgName(k), id);
    send world, eSend, (from = this, msg = (kind = k, id = id,
      badOnion = true, height = 0, own = 0, other = 0, point = 0,
      revoked = 0, next = 0, forged = false,
      content = default(tContent)));
  }

  fun SendRAA(revoked: int, next: int) {
    print format("CTRACE {0} out send raa", name);
    send world, eSend, (from = this, msg = (kind = M_RAA, id = 0,
      badOnion = false, height = 0, own = 0, other = 0, point = 0,
      revoked = revoked, next = next, forged = false,
      content = default(tContent)));
  }

  // OnRestart reloads the channel from disk, dropping everything that was
  // only in memory, and sends our channel_reestablish.
  fun OnRestart() {
    print format("CTRACE {0} in restart", name);
    Restore();
    announce eAnDropped, (node = me, lo = lLog.logIndex);
    print format("CTRACE {0} out send reest next={1} tail={2}", name,
      lTail.height + 1, rTail.height);
    send world, eSend, (from = this, msg = (kind = M_REEST, id = 0,
      badOnion = false, height = lTail.height + 1, own = 0, other = 0,
      point = 0, revoked = rTail.height, next = 0, forged = false,
      content = default(tContent)));
    send world, eAck, (from = this, st = (failed = false,
      remotePending = rHas, localTail = lTail.height));
    goto Reestablishing;
  }

  // Restore rebuilds the ledger lnd loads from disk: both current
  // commitments, the pending remote commitment and the updates of ours it
  // added, the peer's updates we acknowledged but haven't signed for, and
  // ours the peer acknowledged but hasn't signed for. Each entry's heights
  // are rebuilt from the commitments that include it. It mirrors
  // Ledger.Restore in restore.go.
  fun Restore() {
    var peerUnsigned: set[int];
    var ackedUnsigned: set[int];
    var i: int;
    var e: tEntry;
    var nl: tLog;
    var nr: tLog;
    var tip: tCommit;

    tip = rTail;
    if (rHas) {
      tip = rPend;
    }
    if (!profile.unpersistedPeerAcked) {
      peerUnsigned = Parents(lLog, lTail.idxL, rTail.idxL);
    }
    ackedUnsigned = Parents(rLog, rTail.idxR, lTail.idxR);

    nr.logIndex = lTail.idxR;
    nr.htlcCounter = CounterBelow(rLog, lTail.idxR);
    i = 0;
    while (i < sizeof(rLog.entries)) {
      e = rLog.entries[i];
      if (e.kind == K_ADD && OnCommit(false, e, lTail)) {
        e.hL = lTail.height;
        if (OnCommit(false, e, rTail) || e.htlc in peerUnsigned) {
          e.hR = rTail.height;
        } else if (rHas && OnCommit(false, e, rPend)) {
          e.hR = rPend.height;
        } else {
          e.hR = 0;
        }
        nr.entries += (sizeof(nr.entries), e);
      } else if (e.kind != K_ADD && e.logIndex >= rTail.idxR &&
        e.logIndex < lTail.idxR) {

        e.hL = lTail.height;
        e.hR = 0;
        if (rHas && e.logIndex < rPend.idxR) {
          e.hR = rPend.height;
        }
        if (e.kind != K_FEE) {
          nl.modified += (e.parent);
        }
        nr.entries += (sizeof(nr.entries), e);
      }
      i = i + 1;
    }

    nl.logIndex = tip.idxL;
    nl.htlcCounter = CounterBelow(lLog, tip.idxL);
    i = 0;
    while (i < sizeof(lLog.entries)) {
      e = lLog.entries[i];
      if (e.kind == K_ADD && OnCommit(true, e, rTail)) {
        e.hR = rTail.height;
        e.hL = 0;
        if (OnCommit(true, e, lTail) || e.htlc in ackedUnsigned) {
          e.hL = lTail.height;
        }
        nl.entries += (sizeof(nl.entries), e);
      } else if (rHas && e.logIndex >= rTail.idxL &&
        e.logIndex < rPend.idxL) {

        e.hL = 0;
        e.hR = rPend.height;
        if (e.kind != K_ADD && e.kind != K_FEE) {
          nr.modified += (e.parent);
        }
        nl.entries += (sizeof(nl.entries), e);
      } else if (!profile.unpersistedPeerAcked && e.kind != K_ADD &&
        e.logIndex >= lTail.idxL && e.logIndex < rTail.idxL) {

        e.hL = 0;
        e.hR = rTail.height;
        if (e.kind != K_FEE) {
          nr.modified += (e.parent);
        }
        nl.entries += (sizeof(nl.entries), e);
      }
      i = i + 1;
    }

    lLog = nl;
    rLog = nr;
    lHas = false;
    RestoreFeeHist();
  }

  // RestoreFeeHist keeps the fee updates a restart kept: those every
  // unrevoked commitment includes, which compaction may have dropped, and
  // those in the restored initiator log.
  fun RestoreFeeHist() {
    var kept: set[int];
    var f: int;
    var log: tLog;
    var bound: int;
    var i: int;

    if (initiator) {
      log = lLog;
      bound = Min(lTail.idxL, rTail.idxL);
    } else {
      log = rLog;
      bound = Min(lTail.idxR, rTail.idxR);
    }
    foreach (f in feeHist) {
      if (f < bound) {
        kept += (f);
      }
    }
    i = 0;
    while (i < sizeof(log.entries)) {
      if (log.entries[i].kind == K_FEE) {
        kept += (log.entries[i].logIndex);
      }
      i = i + 1;
    }
    feeHist = kept;
  }

  // Content returns what a commitment with the given indexes holds, from
  // our side: own is our HTLCs, other the peer's.
  fun Content(c: tCommit): tContent {
    var out: tContent;
    var i: int;
    var e: tEntry;
    var bound: int;
    var f: int;

    i = 0;
    while (i < sizeof(lLog.entries)) {
      e = lLog.entries[i];
      if (e.kind == K_ADD && OnCommit(true, e, c)) {
        out.own += (e.htlc);
      }
      i = i + 1;
    }
    i = 0;
    while (i < sizeof(rLog.entries)) {
      e = rLog.entries[i];
      if (e.kind == K_ADD && OnCommit(false, e, c)) {
        out.other += (e.htlc);
      }
      i = i + 1;
    }

    bound = c.idxR;
    if (initiator) {
      bound = c.idxL;
    }
    out.fee = -1;
    foreach (f in feeHist) {
      if (f < bound && f > out.fee) {
        out.fee = f;
      }
    }

    return out;
  }

  // OnCommit reports whether a commitment holds an add of our log (ours)
  // or the peer's: included, and not removed by a removal it includes.
  fun OnCommit(ours: bool, e: tEntry, c: tCommit): bool {
    var i: int;
    var r: tEntry;
    var remover: tLog;
    var below: int;

    if (ours) {
      if (e.logIndex >= c.idxL) {
        return false;
      }
      remover = rLog;
      below = c.idxR;
    } else {
      if (e.logIndex >= c.idxR) {
        return false;
      }
      remover = lLog;
      below = c.idxL;
    }

    i = 0;
    while (i < sizeof(remover.entries)) {
      r = remover.entries[i];
      if (r.kind != K_ADD && r.kind != K_FEE && r.parent == e.htlc &&
        r.logIndex < below) {

        return false;
      }
      i = i + 1;
    }

    return true;
  }

  // Parents returns the HTLCs the removals in a range of a log remove.
  fun Parents(log: tLog, lo: int, hi: int): set[int] {
    var i: int;
    var e: tEntry;
    var ids: set[int];

    i = 0;
    while (i < sizeof(log.entries)) {
      e = log.entries[i];
      if (e.kind != K_ADD && e.kind != K_FEE && e.logIndex >= lo &&
        e.logIndex < hi) {

        ids += (e.parent);
      }
      i = i + 1;
    }

    return ids;
  }

  // CounterBelow returns the HTLC counter a log had at the given index.
  fun CounterBelow(log: tLog, idx: int): int {
    var i: int;
    var e: tEntry;

    i = 0;
    while (i < sizeof(log.entries)) {
      e = log.entries[i];
      if (e.kind == K_ADD && e.logIndex >= idx) {
        return e.htlc;
      }
      i = i + 1;
    }

    return log.htlcCounter;
  }

  // OnReestablish answers the peer's channel_reestablish: it resends our
  // last revoke_and_ack if the peer missed it, and the pending commitment
  // with its updates if the peer missed that, in the order we first sent
  // them, and signs if it owes a commitment and may. It mirrors
  // RestoredLedger.Reestablish in reestablish.go.
  fun OnReestablish(m: tMsg) {
    var owesRevocation: bool;
    var resend: bool;
    var tipHeight: int;

    tipHeight = rTail.height;
    if (rHas) {
      tipHeight = rPend.height;
    }

    if (m.revoked > lTail.height || m.revoked + 1 < lTail.height) {
      Fail();
      return;
    }
    owesRevocation = m.revoked + 1 == lTail.height;

    if (m.height > tipHeight + 1 || m.height <= rTail.height) {
      Fail();
      return;
    }
    resend = m.height == tipHeight && !profile.noResend;

    if (owesRevocation && resend) {
      if (lastWasRevoke && !profile.revokeFirst) {
        ResendPending();
        SendRAA(lTail.height - 1, lTail.height + 1);
      } else {
        SendRAA(lTail.height - 1, lTail.height + 1);
        ResendPending();
      }
    } else if (resend) {
      ResendPending();
    } else if (owesRevocation) {
      SendRAA(lTail.height - 1, lTail.height + 1);
      SignIfOwed();
    }
    Done();
  }

  // ResendPending resends the updates the pending remote commitment added,
  // then its commitment_signed.
  fun ResendPending() {
    var i: int;
    var e: tEntry;

    i = 0;
    while (i < sizeof(lLog.entries)) {
      e = lLog.entries[i];
      if (e.logIndex >= rTail.idxL && e.logIndex < rPend.idxL) {
        if (e.kind == K_ADD) {
          SendMsg(M_ADD, e.htlc);
        } else if (e.kind == K_SETTLE) {
          SendMsg(M_SETTLE, e.parent);
        } else if (e.kind == K_FAIL) {
          SendMsg(M_FAIL, e.parent);
        } else if (e.kind == K_MALFORMED) {
          SendMsg(M_MALFORMED, e.parent);
        } else {
          SendMsg(M_FEE, 0);
        }
      }
      i = i + 1;
    }

    print format("CTRACE {0} out send sig", name);
    send world, eSend, (from = this, msg = (kind = M_SIG, id = 0,
      badOnion = false, height = rPend.height, own = rPend.idxR,
      other = rPend.idxL, point = peerNext, revoked = 0, next = 0,
      forged = false, content = Theirs(Content(rPend))));
  }

  fun Fingerprint(): tFingerprint {
    return (lLog = lLog, rLog = rLog, lTail = lTail, lHas = lHas,
      lPend = lPend, rTail = rTail, rHas = rHas, rPend = rPend,
      peerCur = peerCur, peerNext = peerNext);
  }

  // PrintMsg records a peer message in the trace. For a signature and a
  // revocation it also records whether the channel would accept it, which
  // the Go bridge needs to decide the channel operation's outcome.
  fun PrintMsg(m: tMsg) {
    var ok: bool;

    if (m.kind == M_SIG) {
      ok = m.height == lTail.height + 1 && m.own == rTail.idxL &&
        m.other == rLog.logIndex && m.point == lTail.height + 1;
      print format("CTRACE {0} in msg sig ok={1}", name, B(ok));
    } else if (m.kind == M_REEST) {
      print format("CTRACE {0} in msg reest next={1} tail={2}", name,
        m.height, m.revoked);
    } else if (m.kind == M_RAA) {
      print format("CTRACE {0} in msg raa ok={1}", name,
        B(m.revoked == peerCur));
    } else if (m.kind == M_MALFORMED) {
      print format("CTRACE {0} in msg malformed id={1} badonion={2}", name,
        m.id, B(m.badOnion));
    } else {
      print format("CTRACE {0} in msg {1} id={2}", name, MsgName(m.kind),
        m.id);
    }
  }
}

// Theirs returns a commitment's content from the other side.
fun Theirs(c: tContent): tContent {
  return (fee = c.fee, own = c.other, other = c.own);
}

fun Min(a: int, b: int): int {
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

fun KindName(k: Kind): string {
  if (k == K_ADD) {
    return "add";
  } else if (k == K_SETTLE) {
    return "settle";
  } else if (k == K_FAIL) {
    return "fail";
  } else if (k == K_MALFORMED) {
    return "malformed";
  }
  return "fee";
}

fun MsgName(k: MsgKind): string {
  if (k == M_ADD) {
    return "add";
  } else if (k == M_SETTLE) {
    return "settle";
  } else if (k == M_FAIL) {
    return "fail";
  } else if (k == M_MALFORMED) {
    return "malformed";
  } else if (k == M_FEE) {
    return "fee";
  } else if (k == M_SIG) {
    return "sig";
  }
  if (k == M_RAA) {
    return "raa";
  }
  return "reest";
}

fun CmdName(c: CmdKind, id: int): string {
  if (c == C_ADD) {
    return "cmd add";
  } else if (c == C_SETTLE) {
    return format("cmd settle id={0}", id);
  } else if (c == C_FAIL) {
    return format("cmd fail id={0}", id);
  } else if (c == C_MALFORMED) {
    return format("cmd malformed id={0}", id);
  } else if (c == C_FEE) {
    return "cmd fee";
  }
  return "cmd sign";
}
