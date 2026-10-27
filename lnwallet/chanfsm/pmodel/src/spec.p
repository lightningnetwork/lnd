// spec.p holds the properties of the channel. Each monitor states one
// property over both nodes' announcements, so it can judge a node against
// the peer's actual state rather than against the node's own view of it.
//
// A node announces its current commitment (eAnTail) every time it revokes
// the previous one. From the owner's point of view, idxL counts its own
// updates the commitment includes and idxR the peer's. Log indexes agree
// across the two nodes: the update at index i of Bob's own log is the one at
// index i of Alice's copy of it.

// ForwardOnlyLockedIn: a node forwards a peer update only once it is
// irrevocably committed, i.e. both nodes' current commitments include it,
// each having revoked the one before. This is BOLT 2's condition for
// offering the outgoing HTLC of a forward, or failing or settling back the
// incoming one.
spec ForwardOnlyLockedIn observes eAnTail, eAnForward {
  var tails: map[int, tCommit];

  start state Watch {
    entry {
      tails[0] = (height = 0, idxL = 0, idxR = 0);
      tails[1] = (height = 0, idxL = 0, idxR = 0);
    }

    on eAnTail do (p: (node: int, commit: tCommit)) {
      tails[p.node] = p.commit;
    }

    on eAnForward do (p: (node: int, kind: Kind, id: int, logIndex: int)) {
      assert tails[p.node].idxR > p.logIndex &&
        tails[1 - p.node].idxL > p.logIndex,
        format("node {0} forwarded peer update {1} before it was irrevocably committed", p.node, p.logIndex);
    }
  }
}

// ForwardAtMostOnce: a node forwards each peer update at most once.
spec ForwardAtMostOnce observes eAnForward {
  var seen: map[int, set[int]];

  start state Watch {
    entry {
      seen[0] = default(set[int]);
      seen[1] = default(set[int]);
    }

    on eAnForward do (p: (node: int, kind: Kind, id: int, logIndex: int)) {
      assert !(p.logIndex in seen[p.node]),
        format("node {0} forwarded peer update {1} twice", p.node,
          p.logIndex);
      seen[p.node] += (p.logIndex);
    }
  }
}

// RemoveOnlyLockedIn: a node proposes removing an HTLC only once the HTLC
// is irrevocably committed, BOLT 2's rule for senders of update_fulfill_htlc
// and update_fail_htlc.
spec RemoveOnlyLockedIn observes eAnTail, eAnProposed, eAnSendRemoval {
  var tails: map[int, tCommit];
  var addAt: map[int, map[int, int]];

  start state Watch {
    entry {
      tails[0] = (height = 0, idxL = 0, idxR = 0);
      tails[1] = (height = 0, idxL = 0, idxR = 0);
      addAt[0] = default(map[int, int]);
      addAt[1] = default(map[int, int]);
    }

    on eAnTail do (p: (node: int, commit: tCommit)) {
      tails[p.node] = p.commit;
    }

    on eAnProposed do (p: (node: int, logIndex: int, isAdd: bool,
      htlc: int)) {

      if (p.isAdd) {
        addAt[p.node][p.htlc] = p.logIndex;
      }
    }

    on eAnSendRemoval do (p: (node: int, htlc: int)) {
      var owner: int;
      var j: int;

      owner = 1 - p.node;
      assert p.htlc in addAt[owner],
        format("node {0} removed unknown HTLC {1}", p.node, p.htlc);
      j = addAt[owner][p.htlc];
      assert tails[p.node].idxR > j && tails[owner].idxL > j,
        format("node {0} removed HTLC {1} before it was irrevocably committed", p.node, p.htlc);
    }
  }
}

// CommitmentsAgree: every commitment_signed an honest node sends signs
// exactly the commitment its peer builds for that height, with the point
// the peer expects. A difference is a signature the peer can't verify,
// which fails the channel for no fault of either side.
spec CommitmentsAgree observes eAnVerify {
  start state Watch {
    on eAnVerify do (p: (node: int, forged: bool, agree: bool)) {
      if (!p.forged) {
        assert p.agree, format("node {0} received a signature for a commitment it doesn't build", p.node);
      }
    }
  }
}

// RevocationNeedsPending: a revoke_and_ack that doesn't complete a
// commitment we signed is refused, and refusing it changes nothing.
spec RevocationNeedsPending observes eAnRevocation {
  start state Watch {
    on eAnRevocation do (p: (node: int, hadPending: bool, refused: bool,
      changed: bool)) {

      if (!p.hadPending) {
        assert p.refused, format("node {0} accepted a revocation with no commitment pending", p.node);
        assert !p.changed, format("node {0} changed its state refusing a revocation with no commitment pending", p.node);
      }
    }
  }
}

// RefusalChangesNothing: a peer message the ledger refuses fails the
// channel without changing it.
spec RefusalChangesNothing observes eAnRefused {
  start state Watch {
    on eAnRefused do (p: (node: int, changed: bool)) {
      assert !p.changed, format("node {0} changed its state refusing a message", p.node);
    }
  }
}

// EventuallyLockedInAndForwarded: in an honest run whose links drain and
// whose nodes keep signing, every update either node proposes ends up in
// both nodes' current commitments, and every proposed add and removal is
// forwarded by the peer. It is hot while any is outstanding.
spec EventuallyLockedInAndForwarded observes eAnProposed, eAnTail,
  eAnForward, eAnDropped {

  var tails: map[int, tCommit];
  var unlocked: map[int, set[int]];
  var unforwarded: map[int, set[int]];

  start cold state Quiet {
    entry {
      tails[0] = (height = 0, idxL = 0, idxR = 0);
      tails[1] = (height = 0, idxL = 0, idxR = 0);
      unlocked[0] = default(set[int]);
      unlocked[1] = default(set[int]);
      unforwarded[0] = default(set[int]);
      unforwarded[1] = default(set[int]);
    }

    on eAnDropped do (p: (node: int, lo: int)) {
      Drop(p.node, p.lo);
    }

    on eAnProposed do (p: (node: int, logIndex: int, isAdd: bool,
      htlc: int)) {

      Propose(p.node, p.logIndex, p.htlc);
      goto Outstanding;
    }

    on eAnTail do (p: (node: int, commit: tCommit)) {
      tails[p.node] = p.commit;
    }

    on eAnForward do (p: (node: int, kind: Kind, id: int, logIndex: int)) {
      unforwarded[1 - p.node] -= (p.logIndex);
    }
  }

  hot state Outstanding {
    on eAnDropped do (p: (node: int, lo: int)) {
      Drop(p.node, p.lo);
      Settle();
    }

    on eAnProposed do (p: (node: int, logIndex: int, isAdd: bool,
      htlc: int)) {

      Propose(p.node, p.logIndex, p.htlc);
    }

    on eAnTail do (p: (node: int, commit: tCommit)) {
      tails[p.node] = p.commit;
      Settle();
    }

    on eAnForward do (p: (node: int, kind: Kind, id: int, logIndex: int)) {
      unforwarded[1 - p.node] -= (p.logIndex);
      Settle();
    }
  }

  // Drop forgets a node's updates a restart lost: those no commitment
  // included yet, which the node never had to see through.
  fun Drop(n: int, lo: int) {
    var i: int;
    var gone: set[int];

    foreach (i in unlocked[n]) {
      if (i >= lo) {
        gone += (i);
      }
    }
    foreach (i in gone) {
      unlocked[n] -= (i);
      if (i in unforwarded[n]) {
        unforwarded[n] -= (i);
      }
    }
  }

  fun Propose(n: int, i: int, htlc: int) {
    unlocked[n] += (i);

    // A fee update is proposed with htlc -1 and is never forwarded.
    if (htlc >= 0) {
      unforwarded[n] += (i);
    }
  }

  fun Settle() {
    var n: int;
    var i: int;
    var done: set[int];

    n = 0;
    while (n < 2) {
      done = default(set[int]);
      foreach (i in unlocked[n]) {
        if (tails[n].idxL > i && tails[1 - n].idxR > i) {
          done += (i);
        }
      }
      foreach (i in done) {
        unlocked[n] -= (i);
      }
      n = n + 1;
    }

    if (sizeof(unlocked[0]) + sizeof(unlocked[1]) +
      sizeof(unforwarded[0]) + sizeof(unforwarded[1]) == 0) {

      goto Quiet;
    }
  }
}

// HonestNeverFails: in a run where neither node is byzantine, no node ever
// fails the channel, disconnections and restarts included.
spec HonestNeverFails observes eAnFailed {
  start state Watch {
    on eAnFailed do (p: (node: int)) {
      assert false, format("node {0} failed the channel in an honest run", p.node);
    }
  }
}
