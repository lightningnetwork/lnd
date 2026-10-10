// world.p is everything around the two nodes of a channel: the links
// between them, and the local commands each node's owner issues. World owns
// every nondeterministic choice. Each link delivers in order, as a BOLT 8
// connection does. World waits for a node to handle each event before it
// acts again, because the channel actor handles one message at a time and
// carries out its outbox at once; the asynchrony that matters is the
// messages in flight on the links.
//
// A byzantine run lets Bob, besides running the honest node, inject messages
// straight at Alice that his node would never send. Once either node fails
// the channel, or Bob injects a message, the run ends. Otherwise, after its
// step budget, World drains the links and has both nodes sign until the
// channel is quiescent, which is when the liveness monitor judges it.

// Injections a byzantine Bob can make.
//
//   I_PREMATURE_RAA: a revoke_and_ack while Alice has no commitment
//     outstanding for Bob, revealing Bob's current secret.
//   I_REPLAY_RAA: the last revoke_and_ack Alice received, again.
//   I_BAD_ADD: an add with an HTLC ID out of sequence.
//   I_SETTLE_UNKNOWN: a settle of an HTLC Alice never offered.
//   I_SETTLE_LATEST: a settle of Alice's newest HTLC, which may not be in
//     her current commitment yet.
//   I_FEE: a fee update, which Bob as the non-initiator may not send.
//   I_NO_BADONION: a malformed fail without the BADONION bit.
//   I_BAD_SIG: a commitment_signed that signs the wrong commitment.
enum Injection {
  I_PREMATURE_RAA, I_REPLAY_RAA, I_BAD_ADD, I_SETTLE_UNKNOWN,
  I_SETTLE_LATEST, I_FEE, I_NO_BADONION, I_BAD_SIG
}

type tWorldCfg = (
  steps: int,
  maxAdds: int,
  byzantine: bool,
  injections: seq[Injection],
  profile: tProfile,
  disconnects: int
);

event eStep;

machine World {
  var cfg: tWorldCfg;
  var nodes: seq[machine];
  var status: seq[tStatus];

  // inbox[i] holds the messages in flight to node i, in order.
  var inbox: seq[seq[tMsg]];

  // known[i] holds the peer's HTLC IDs node i has received, which its owner
  // may try to remove. lastAdd[i] is the ID of node i's newest HTLC.
  var known: seq[set[int]];
  var lastAdd: seq[int];

  var lastRAA: tMsg;
  var hasRAA: bool;

  var steps: int;
  var adds: int;
  var drainSigns: int;
  var injected: bool;

  // disconnects counts the disconnections so far, and restarting the nodes
  // still to restart for the current one.
  var disconnects: int;
  var restarting: int;

  start state Init {
    entry (c: tWorldCfg) {
      cfg = c;
      announce eAnConfig, (initiator = 0,);
      nodes += (0, new Node((world = this, me = 0, initiator = true,
        profile = cfg.profile)));
      nodes += (1, new Node((world = this, me = 1, initiator = false,
        profile = Production())));
      status += (0, (failed = false, remotePending = false,
        localTail = 0));
      status += (1, (failed = false, remotePending = false,
        localTail = 0));
      inbox += (0, default(seq[tMsg]));
      inbox += (1, default(seq[tMsg]));
      known += (0, default(set[int]));
      known += (1, default(set[int]));
      lastAdd += (0, -1);
      lastAdd += (1, -1);
      send this, eStep;
      goto Run;
    }
  }

  state Run {
    on eStep do {
      Act();
    }
  }

  // Waiting collects a node's outbox until it acknowledges the event.
  state Waiting {
    defer eStep;

    on eSend do (p: (from: machine, msg: tMsg)) {
      var i: int;

      i = Index(p.from);
      inbox[1 - i] += (sizeof(inbox[1 - i]), p.msg);
      if (p.msg.kind == M_ADD) {
        lastAdd[i] = p.msg.id;
      }
    }

    on eAck do (p: (from: machine, st: tStatus)) {
      status[Index(p.from)] = p.st;
      send this, eStep;
      goto Run;
    }
  }

  state Stopped {
    ignore eStep;
  }

  fun Index(m: machine): int {
    if (m == nodes[0]) {
      return 0;
    }
    return 1;
  }

  fun Act() {
    var acts: seq[int];
    var a: int;

    // An injected message the node accepts leaves the two nodes with
    // different histories, so nothing after it is an honest exchange: the
    // run ends with the injection's outcome.
    if (status[0].failed || status[1].failed || injected) {
      goto Stopped;
      return;
    }

    // A disconnection restarts both nodes, one at a time.
    if (restarting > 0) {
      restarting = restarting - 1;
      ToNode(1 - restarting, eRestart, null);
      return;
    }

    steps = steps + 1;
    if (steps > cfg.steps) {
      Drain();
      return;
    }

    // A disconnection loses every message in flight.
    if (disconnects < cfg.disconnects && choose(8) == 0) {
      disconnects = disconnects + 1;
      inbox[0] = default(seq[tMsg]);
      inbox[1] = default(seq[tMsg]);
      restarting = 1;
      ToNode(0, eRestart, null);
      return;
    }

    acts += (sizeof(acts), 0);
    acts += (sizeof(acts), 1);
    if (sizeof(inbox[0]) > 0) {
      acts += (sizeof(acts), 2);
      acts += (sizeof(acts), 2);
    }
    if (sizeof(inbox[1]) > 0) {
      acts += (sizeof(acts), 3);
      acts += (sizeof(acts), 3);
    }
    if (cfg.byzantine && !injected && steps > cfg.steps / 3) {
      acts += (sizeof(acts), 4);
    }

    a = acts[choose(sizeof(acts))];
    if (a == 0 || a == 1) {
      Command(a);
    } else if (a == 2) {
      Deliver(0);
    } else if (a == 3) {
      Deliver(1);
    } else {
      Inject(cfg.injections[choose(sizeof(cfg.injections))]);
    }
  }

  // Command has node i's owner issue a random command.
  fun Command(i: int) {
    var c: int;
    var ids: seq[int];
    var id: int;

    c = choose(6);
    if (c == 0 && adds < cfg.maxAdds) {
      adds = adds + 1;
      ToNode(i, eCmd, (cmd = C_ADD, id = 0));
      return;
    }
    if (c >= 1 && c <= 3 && sizeof(known[i]) > 0) {
      foreach (id in known[i]) {
        ids += (sizeof(ids), id);
      }
      id = ids[choose(sizeof(ids))];
      if (c == 1) {
        ToNode(i, eCmd, (cmd = C_SETTLE, id = id));
      } else if (c == 2) {
        ToNode(i, eCmd, (cmd = C_FAIL, id = id));
      } else {
        ToNode(i, eCmd, (cmd = C_MALFORMED, id = id));
      }
      return;
    }
    if (c == 4 && i == 0) {
      ToNode(i, eCmd, (cmd = C_FEE, id = 0));
      return;
    }
    ToNode(i, eCmd, (cmd = C_SIGN, id = 0));
  }

  // Deliver hands node i the next message in flight to it.
  fun Deliver(i: int) {
    var m: tMsg;

    m = inbox[i][0];
    inbox[i] -= (0);
    if (m.kind == M_ADD) {
      known[i] += (m.id);
    }
    if (i == 0 && m.kind == M_RAA) {
      lastRAA = m;
      hasRAA = true;
    }
    ToNode(i, eMsg, m);
  }

  // Inject has a byzantine Bob send Alice a message of the given kind, if
  // the channel's state allows one, and otherwise takes a normal step.
  fun Inject(k: Injection) {
    var m: tMsg;

    m = (kind = M_ADD, id = 0, badOnion = true, height = 0, own = 0,
      other = 0, point = 0, revoked = 0, next = 0, forged = true,
      content = default(tContent));

    if (k == I_PREMATURE_RAA) {
      if (status[0].remotePending) {
        send this, eStep;
        return;
      }
      m.kind = M_RAA;
      m.revoked = status[1].localTail;
      m.next = status[1].localTail + 2;
    } else if (k == I_REPLAY_RAA) {
      if (!hasRAA) {
        send this, eStep;
        return;
      }
      m = lastRAA;
      m.forged = true;
    } else if (k == I_BAD_ADD) {
      m.id = 1000;
    } else if (k == I_SETTLE_UNKNOWN) {
      m.kind = M_SETTLE;
      m.id = 1000;
    } else if (k == I_SETTLE_LATEST) {
      if (lastAdd[0] < 0) {
        send this, eStep;
        return;
      }
      m.kind = M_SETTLE;
      m.id = lastAdd[0];
    } else if (k == I_FEE) {
      m.kind = M_FEE;
    } else if (k == I_NO_BADONION) {
      if (lastAdd[0] < 0) {
        send this, eStep;
        return;
      }
      m.kind = M_MALFORMED;
      m.id = lastAdd[0];
      m.badOnion = false;
    } else {
      m.kind = M_SIG;
      m.height = status[0].localTail + 1;
      m.own = 1000;
    }

    injected = true;
    ToNode(0, eMsg, m);
  }

  // Drain delivers everything in flight, then has both nodes sign in turn
  // until neither link carries anything, which leaves the channel
  // quiescent.
  fun Drain() {
    if (sizeof(inbox[0]) > 0) {
      Deliver(0);
      return;
    }
    if (sizeof(inbox[1]) > 0) {
      Deliver(1);
      return;
    }
    if (drainSigns < 6) {
      drainSigns = drainSigns + 1;
      ToNode(drainSigns % 2, eCmd, (cmd = C_SIGN, id = 0));
      return;
    }
    goto Stopped;
  }

  fun ToNode(i: int, e: event, payload: any) {
    if (e == eRestart) {
      send nodes[i], eRestart;
    } else {
      send nodes[i], e, payload;
    }
    goto Waiting;
  }
}

fun Production(): tProfile {
  return (legacyRevocation = false, forwardOnCommit = false,
    noFreshness = false, earlyLocalRemoval = false, signAllRemote = false,
    noResend = false, revokeFirst = false, unpersistedPeerAcked = false);
}
