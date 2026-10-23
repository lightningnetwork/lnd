// Test drivers for the channel contract. Each draws a step budget and hands
// World a node profile and, for a byzantine run, the injections Bob may
// make.

fun AllInjections(): seq[Injection] {
  var s: seq[Injection];

  s += (0, I_PREMATURE_RAA);
  s += (1, I_REPLAY_RAA);
  s += (2, I_BAD_ADD);
  s += (3, I_SETTLE_UNKNOWN);
  s += (4, I_SETTLE_LATEST);
  s += (5, I_FEE);
  s += (6, I_NO_BADONION);
  s += (7, I_BAD_SIG);

  return s;
}

fun Cfg(byz: bool, inj: seq[Injection], p: tProfile): tWorldCfg {
  return (steps = 20 + choose(30), maxAdds = 6, byzantine = byz,
    injections = inj, profile = p, disconnects = 0);
}

// Reconnecting is Cfg with up to three disconnections.
fun Reconnecting(p: tProfile): tWorldCfg {
  var c: tWorldCfg;
  c = Cfg(false, default(seq[Injection]), p);
  c.disconnects = 3;
  return c;
}

machine HonestDriver {
  start state Init {
    entry {
      new World(Cfg(false, default(seq[Injection]), Production()));
    }
  }
}

machine ReconnectDriver {
  start state Init {
    entry {
      new World(Reconnecting(Production()));
    }
  }
}

machine NoResendDriver {
  start state Init {
    entry {
      var p: tProfile;
      p = Production();
      p.noResend = true;
      new World(Reconnecting(p));
    }
  }
}

machine RevokeFirstDriver {
  start state Init {
    entry {
      var p: tProfile;
      p = Production();
      p.revokeFirst = true;
      new World(Reconnecting(p));
    }
  }
}

machine UnpersistedPeerAckedDriver {
  start state Init {
    entry {
      var p: tProfile;
      p = Production();
      p.unpersistedPeerAcked = true;
      new World(Reconnecting(p));
    }
  }
}

machine ByzantineDriver {
  start state Init {
    entry {
      new World(Cfg(true, AllInjections(), Production()));
    }
  }
}

// PrematureRAADriver is a byzantine run whose only injection is the
// premature revocation.
machine PrematureRAADriver {
  start state Init {
    entry {
      var inj: seq[Injection];
      inj += (0, I_PREMATURE_RAA);
      new World(Cfg(true, inj, Production()));
    }
  }
}

machine LegacyRevocationDriver {
  start state Init {
    entry {
      var inj: seq[Injection];
      var p: tProfile;
      inj += (0, I_PREMATURE_RAA);
      p = Production();
      p.legacyRevocation = true;
      new World(Cfg(true, inj, p));
    }
  }
}

machine ForwardOnCommitDriver {
  start state Init {
    entry {
      var p: tProfile;
      p = Production();
      p.forwardOnCommit = true;
      new World(Cfg(false, default(seq[Injection]), p));
    }
  }
}

machine NoFreshnessDriver {
  start state Init {
    entry {
      var p: tProfile;
      p = Production();
      p.noFreshness = true;
      new World(Cfg(false, default(seq[Injection]), p));
    }
  }
}

machine EarlyLocalRemovalDriver {
  start state Init {
    entry {
      var p: tProfile;
      p = Production();
      p.earlyLocalRemoval = true;
      new World(Cfg(false, default(seq[Injection]), p));
    }
  }
}

machine SignAllRemoteDriver {
  start state Init {
    entry {
      var p: tProfile;
      p = Production();
      p.signAllRemote = true;
      new World(Cfg(false, default(seq[Injection]), p));
    }
  }
}

// The production node under honest peers: every safety property, and
// liveness once the links drain.
test tcHonest [main = HonestDriver]:
  assert ForwardOnlyLockedIn, ForwardAtMostOnce, RemoveOnlyLockedIn,
    CommitmentsAgree, RevocationNeedsPending, RefusalChangesNothing,
    HonestNeverFails, EventuallyLockedInAndForwarded in
  { HonestDriver, World, Node };

// The production node across disconnections and restarts, with
// channel_reestablish: every safety property, no failure, and liveness.
test tcReconnect [main = ReconnectDriver]:
  assert ForwardOnlyLockedIn, ForwardAtMostOnce, RemoveOnlyLockedIn,
    CommitmentsAgree, RevocationNeedsPending, RefusalChangesNothing,
    HonestNeverFails, EventuallyLockedInAndForwarded in
  { ReconnectDriver, World, Node };

// The production node against a byzantine Bob: every safety property.
test tcByzantine [main = ByzantineDriver]:
  assert ForwardOnlyLockedIn, ForwardAtMostOnce, RemoveOnlyLockedIn,
    CommitmentsAgree, RevocationNeedsPending, RefusalChangesNothing in
  { ByzantineDriver, World, Node };

// The production node against the premature revocation alone, so every
// run that can reach it does.
test tcPrematureRevocation [main = PrematureRAADriver]:
  assert RevocationNeedsPending, RefusalChangesNothing in
  { PrematureRAADriver, World, Node };

// Counterexamples: each removes one rule, and must be caught.
test tcLegacyRevocationCounterexample [main = LegacyRevocationDriver]:
  assert RevocationNeedsPending in
  { LegacyRevocationDriver, World, Node };

test tcForwardOnCommitCounterexample [main = ForwardOnCommitDriver]:
  assert ForwardOnlyLockedIn in
  { ForwardOnCommitDriver, World, Node };

test tcNoFreshnessCounterexample [main = NoFreshnessDriver]:
  assert ForwardAtMostOnce in
  { NoFreshnessDriver, World, Node };

test tcEarlyLocalRemovalCounterexample [main = EarlyLocalRemovalDriver]:
  assert RemoveOnlyLockedIn in
  { EarlyLocalRemovalDriver, World, Node };

test tcSignAllRemoteCounterexample [main = SignAllRemoteDriver]:
  assert CommitmentsAgree in
  { SignAllRemoteDriver, World, Node };

test tcNoResendCounterexample [main = NoResendDriver]:
  assert EventuallyLockedInAndForwarded in
  { NoResendDriver, World, Node };

test tcRevokeFirstCounterexample [main = RevokeFirstDriver]:
  assert HonestNeverFails in
  { RevokeFirstDriver, World, Node };

test tcUnpersistedPeerAckedCounterexample [main = UnpersistedPeerAckedDriver]:
  assert HonestNeverFails in
  { UnpersistedPeerAckedDriver, World, Node };
