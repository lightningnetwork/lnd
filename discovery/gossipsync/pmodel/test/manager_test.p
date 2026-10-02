// Test drivers for the manager contract. Each driver creates a manager with a
// profile and sends it a workload over a small set of peers. The checker
// explores both the workload and every order in which the syncers' outcomes
// interleave with it.

// NumPeers is the number of distinct peers a workload draws from.
fun NumPeers(): int {
  return 3;
}

fun Production(): tProfile {
  return (noSettle = false, legacyTick = false, noLocalBackoff = false,
    noSessionCheck = false, noPinnedRetry = false);
}

// Workload sends a manager steps random inputs: connects, disconnects,
// ticks, rotations, and outcomes for made-up sessions and attempts, which
// are stale unless they happen to name an attempt in flight.
fun Workload(manager: machine, steps: int, pinned: set[int], ticks: bool) {
  var i: int;
  var pub: int;
  var op: int;
  var k: Kind;

  i = 0;
  while (i < steps) {
    op = choose(7);
    pub = choose(NumPeers()) + 1;

    if (op <= 1) {
      send manager, eConnect, (pub = pub, pinned = pub in pinned);
    } else if (op == 2) {
      send manager, eDisconnect, pub;
    } else if (op == 3 && ticks) {
      send manager, eHistTick;
    } else if (op == 4) {
      send manager, eRotateTick;
    } else if (op == 5) {
      k = COMPLETED;
      if ($) {
        k = PEERFAULT;
      }
      send manager, eOutcome, (session = choose(4), attempt = choose(6),
        kind = k);
    } else {
      send manager, eConnect, (pub = pub, pinned = pub in pinned);
    }

    i = i + 1;
  }
}

// DrawPinned draws the set of pinned peers.
fun DrawPinned(): set[int] {
  var pinned: set[int];
  var pub: int;

  pub = 1;
  while (pub <= NumPeers()) {
    if (choose(4) == 0) {
      pinned += (pub);
    }
    pub = pub + 1;
  }

  return pinned;
}

// ProductionDriver runs the production contract with a random quota.
machine ProductionDriver {
  start state Init {
    entry {
      var m: machine;
      m = new Manager((numActive = choose(3), profile = Production(),
        faults = -1));
      Workload(m, 12, DrawPinned(), true);
    }
  }
}

// SinglePeerDriver runs one unpinned peer through many historical ticks,
// which is where the legacy manager stalled for two ticks.
machine SinglePeerDriver {
  start state Init {
    entry {
      var m: machine;
      var i: int;
      m = new Manager((numActive = 1, profile = Production(),
        faults = -1));
      send m, eConnect, (pub = 1, pinned = false);
      i = 0;
      while (i < 6) {
        send m, eHistTick;
        i = i + 1;
      }
    }
  }
}

// LegacyTickDriver runs a random workload with the legacy tick order: pick
// first, then open the epoch, and never fall back to a peer that failed in
// the last epoch. Every safety property still holds, because settle retries
// the peer as soon as the epoch opens, so the tick order is not
// load-bearing.
machine LegacyTickDriver {
  start state Init {
    entry {
      var m: machine;
      var p: tProfile;
      p = Production();
      p.legacyTick = true;
      m = new Manager((numActive = 1 + choose(2), profile = p,
        faults = -1));
      Workload(m, 12, DrawPinned(), true);
    }
  }
}

// SinglePeerLegacyTickDriver is SinglePeerDriver with the legacy tick order.
machine SinglePeerLegacyTickDriver {
  start state Init {
    entry {
      var m: machine;
      var i: int;
      var p: tProfile;
      p = Production();
      p.legacyTick = true;
      m = new Manager((numActive = 1, profile = p, faults = -1));
      send m, eConnect, (pub = 1, pinned = false);
      i = 0;
      while (i < 6) {
        send m, eHistTick;
        i = i + 1;
      }
    }
  }
}

// Ticker stands in for the historical timer under fairness: once the
// workload is queued, it ticks whenever the graph is unsynced and no attempt
// is in flight, up to a bound, and waits while one is.
machine Ticker {
  var manager: machine;
  var ticks: int;

  start state Init {
    entry (m: machine) {
      manager = m;
      send manager, eStatusQuery, this;
    }

    on eStatus do (st: (synced: bool, needTick: bool, working: bool)) {
      if (st.needTick && ticks < 12) {
        ticks = ticks + 1;
        send manager, eHistTick;
        send manager, eStatusQuery, this;
      } else if (st.working && !st.synced) {
        send manager, eStatusQuery, this;
      }
    }
  }
}

// LivenessDriver runs a random workload whose syncers fail at most once per
// session, then lets the ticker keep ticking while the graph is unsynced.
machine LivenessDriver {
  start state Init {
    entry {
      var m: machine;
      m = new Manager((numActive = 1 + choose(2), profile = Production(),
        faults = 1));
      Workload(m, 8, DrawPinned(), true);
      new Ticker(m);
    }
  }
}

// NoTickLivenessDriver runs peers that always complete, and no ticks at all:
// connecting a peer alone must lead to a synced graph.
machine NoTickLivenessDriver {
  start state Init {
    entry {
      NoTickLiveness(Production());
    }
  }
}

fun NoTickLiveness(p: tProfile) {
  var m: machine;
  var i: int;
  var pub: int;

  m = new Manager((numActive = 1 + choose(2), profile = p, faults = 0));

  i = 0;
  while (i < 6) {
    pub = choose(NumPeers()) + 1;
    if (choose(3) == 0) {
      send m, eDisconnect, pub;
    } else {
      send m, eConnect, (pub = pub, pinned = false);
    }
    i = i + 1;
  }
}

// NoSettleDriver runs a random workload with no settle step.
machine NoSettleDriver {
  start state Init {
    entry {
      var m: machine;
      var p: tProfile;
      p = Production();
      p.noSettle = true;
      m = new Manager((numActive = 1 + choose(2), profile = p,
        faults = -1));
      Workload(m, 12, DrawPinned(), true);
    }
  }
}

// NoSettleNoTickDriver is NoTickLivenessDriver with no settle step.
machine NoSettleNoTickDriver {
  start state Init {
    entry {
      var p: tProfile;
      p = Production();
      p.noSettle = true;
      NoTickLiveness(p);
    }
  }
}

// NoLocalBackoffDriver runs a random workload in which a local fault is not
// backed off.
machine NoLocalBackoffDriver {
  start state Init {
    entry {
      var m: machine;
      var p: tProfile;
      p = Production();
      p.noLocalBackoff = true;
      m = new Manager((numActive = 1 + choose(2), profile = p,
        faults = -1));
      Workload(m, 12, DrawPinned(), true);
    }
  }
}

// NoSessionCheckDriver runs a random workload in which an outcome counts
// whichever session reports it.
machine NoSessionCheckDriver {
  start state Init {
    entry {
      var m: machine;
      var p: tProfile;
      p = Production();
      p.noSessionCheck = true;
      m = new Manager((numActive = 1 + choose(2), profile = p,
        faults = -1));
      Workload(m, 12, DrawPinned(), true);
    }
  }
}

// PinnedOnlyDriver connects one or two pinned peers whose syncers fail once,
// under either quota, and lets the ticker keep ticking.
machine PinnedOnlyDriver {
  start state Init {
    entry {
      PinnedOnly(Production());
    }
  }
}

// NoPinnedRetryDriver is PinnedOnlyDriver with no pinned retry.
machine NoPinnedRetryDriver {
  start state Init {
    entry {
      var p: tProfile;
      p = Production();
      p.noPinnedRetry = true;
      PinnedOnly(p);
    }
  }
}

fun PinnedOnly(p: tProfile) {
  var m: machine;
  m = new Manager((numActive = choose(2), profile = p, faults = 1));
  send m, eConnect, (pub = 1, pinned = true);
  if ($) {
    send m, eConnect, (pub = 2, pinned = true);
  }
  new Ticker(m);
}

// Green test cases: no monitor may fire.
test tcManagerProduction [main = ProductionDriver]:
  assert ManagerContract in
  { ProductionDriver, Manager, Syncer };

test tcManagerOnePeer [main = SinglePeerDriver]:
  assert ManagerContract in
  { SinglePeerDriver, Manager, Syncer };

test tcManagerLegacyTick [main = LegacyTickDriver]:
  assert ManagerContract in
  { LegacyTickDriver, Manager, Syncer };

test tcManagerSinglePeerLegacyTick [main = SinglePeerLegacyTickDriver]:
  assert ManagerContract in
  { SinglePeerLegacyTickDriver, Manager, Syncer };

test tcManagerLiveness [main = LivenessDriver]:
  assert ManagerContract, GraphEventuallySynced,
    GraphEventuallySyncedWithPinned in
  { LivenessDriver, Manager, Syncer, Ticker };

test tcManagerNoTickLiveness [main = NoTickLivenessDriver]:
  assert ManagerContract, GraphEventuallySynced in
  { NoTickLivenessDriver, Manager, Syncer };

// Only pinned peers, whose attempts fail once: the tick retry syncs the
// graph under either quota.
test tcManagerPinnedOnly [main = PinnedOnlyDriver]:
  assert ManagerContract, GraphEventuallySyncedWithPinned in
  { PinnedOnlyDriver, Manager, Syncer, Ticker };

// Counterexample test cases: each must find the bug its profile introduces.
test tcManagerNoSettleCounterexample [main = NoSettleDriver]:
  assert ManagerContract in
  { NoSettleDriver, Manager, Syncer };

test tcManagerNoSettleStarvesCounterexample [main = NoSettleNoTickDriver]:
  assert GraphEventuallySynced in
  { NoSettleNoTickDriver, Manager, Syncer };

test tcManagerNoLocalBackoffCounterexample [main = NoLocalBackoffDriver]:
  assert ManagerContract in
  { NoLocalBackoffDriver, Manager, Syncer };

test tcManagerNoSessionCheckCounterexample [main = NoSessionCheckDriver]:
  assert ManagerContract in
  { NoSessionCheckDriver, Manager, Syncer };

// Without the pinned retry, a pinned peer's failed attempt is never retried
// while it stays connected, so a node with only pinned peers stays unsynced
// however many ticks pass.
test tcManagerNoPinnedRetryCounterexample [main = NoPinnedRetryDriver]:
  assert GraphEventuallySyncedWithPinned in
  { NoPinnedRetryDriver, Manager, Syncer, Ticker };
