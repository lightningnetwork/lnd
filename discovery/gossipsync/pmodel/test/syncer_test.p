// Test drivers for the peer syncer contract. Each driver draws a peer graph,
// what we already know of it, the chunking and batching, and whether our
// local lookup fails, then hands World a timing profile and a peer profile.

fun SyncerProduction(): tSyncerProfile {
  return (noDraining = false, noFirstReplyCheck = false,
    legacyDrainOnLastBlock = false, fixedDrainDeadline = false);
}

// DrawSyncerCfg draws a configuration for the given profiles.
fun DrawSyncerCfg(f: ReplyFormat, t: Timing, stalls: bool, byz: bool,
  p: tSyncerProfile): tSyncerCfg {

  var chans: set[int];
  var known: set[int];
  var c: int;
  var maxReplies: int;

  c = 1;
  while (c <= NumBlocks()) {
    if ($) {
      chans += (c);
      if (choose(3) == 0) {
        known += (c);
      }
    }
    c = c + 1;
  }

  // An honest stream has at most one reply per block, so a budget of six
  // never truncates it. A byzantine peer also faces a tight budget.
  maxReplies = 6;
  if (byz && $) {
    maxReplies = 3;
  }

  return (
    chans = chans,
    known = known,
    chunk = 1 + choose(2),
    batch = 1 + choose(2),
    maxReplies = maxReplies,
    lookupErr = choose(6) == 0,
    replyFormat = f,
    timing = t,
    stalls = stalls,
    byzantine = byz,
    profile = p,
    starts = 3,
    steps = 24
  );
}

machine HonestPromptDriver {
  start state Init {
    entry {
      new World(DrawSyncerCfg(FMT_LND, PROMPT, false, false,
        SyncerProduction()));
    }
  }
}

machine HonestLossyDriver {
  start state Init {
    entry {
      new World(DrawSyncerCfg(FMT_LND, SLOW, true, false,
        SyncerProduction()));
    }
  }
}

machine VerySlowDriver {
  start state Init {
    entry {
      new World(DrawSyncerCfg(FMT_LND, VERYSLOW, true, false,
        SyncerProduction()));
    }
  }
}

machine ByzantineDriver {
  start state Init {
    entry {
      new World(DrawSyncerCfg(FMT_LND, SLOW, true, true,
        SyncerProduction()));
    }
  }
}

machine LegacyPromptDriver {
  start state Init {
    entry {
      new World(DrawSyncerCfg(FMT_LEGACY, PROMPT, false, false,
        SyncerProduction()));
    }
  }
}

machine LegacySlowDriver {
  start state Init {
    entry {
      new World(DrawSyncerCfg(FMT_LEGACY, SLOW, true, false,
        SyncerProduction()));
    }
  }
}

machine LegacyDrainDriver {
  start state Init {
    entry {
      new World(DrawSyncerCfg(FMT_LEGACY, SLOW, true, false,
        (noDraining = false, noFirstReplyCheck = false,
        legacyDrainOnLastBlock = true, fixedDrainDeadline = false)));
    }
  }
}

machine FixedDrainDeadlineDriver {
  start state Init {
    entry {
      new World(DrawSyncerCfg(FMT_LND, SLOW, true, false,
        (noDraining = false, noFirstReplyCheck = false,
        legacyDrainOnLastBlock = false, fixedDrainDeadline = true)));
    }
  }
}

machine NoDrainingDriver {
  start state Init {
    entry {
      new World(DrawSyncerCfg(FMT_LND, SLOW, true, false,
        (noDraining = true, noFirstReplyCheck = false,
        legacyDrainOnLastBlock = false, fixedDrainDeadline = false)));
    }
  }
}

machine NoFirstReplyCheckDriver {
  start state Init {
    entry {
      new World(DrawSyncerCfg(FMT_LND, VERYSLOW, true, false,
        (noDraining = false, noFirstReplyCheck = true,
        legacyDrainOnLastBlock = false, fixedDrainDeadline = false)));
    }
  }
}

// Green test cases: no monitor may fire.

// A prompt honest peer: every property, and no attempt ever fails.
test tcSyncerHonestPrompt [main = HonestPromptDriver]:
  assert OneOutstandingQuery, NoCrossAttemptCredit, WholeStreamCredit,
    OutcomeExactlyOnce, CompletedQueriedMissing, PromptPeerNeverFaulted in
  { HonestPromptDriver, World, PeerSyncer };

// A slow, lossy honest peer under A-DRAIN, which pauses past the timeout at
// most once per stream: every pairing property holds, because the drain
// timer is an inactivity timer.
test tcSyncerHonestLossy [main = HonestLossyDriver]:
  assert OneOutstandingQuery, NoCrossAttemptCredit, WholeStreamCredit,
    OutcomeExactlyOnce, CompletedQueriedMissing in
  { HonestLossyDriver, World, PeerSyncer };

// A peer slower than A-DRAIN allows, which pauses past the timeout more than
// once per stream: a whole earlier stream may be credited to a later
// attempt, which is harmless since both queries are the same, but no attempt
// completes on part of a stream.
test tcSyncerVerySlowPeer [main = VerySlowDriver]:
  assert WholeStreamCredit, OutcomeExactlyOnce, CompletedQueriedMissing in
  { VerySlowDriver, World, PeerSyncer };

// A byzantine peer: every attempt still ends with exactly one outcome, and
// the syncer always returns to Idle.
test tcSyncerByzantine [main = ByzantineDriver]:
  assert OutcomeExactlyOnce in
  { ByzantineDriver, World, PeerSyncer };

// A slow, lossy honest peer in the legacy reply format.
test tcSyncerLegacySlow [main = LegacySlowDriver]:
  assert OneOutstandingQuery, NoCrossAttemptCredit, WholeStreamCredit,
    OutcomeExactlyOnce, CompletedQueriedMissing in
  { LegacySlowDriver, World, PeerSyncer };

// A prompt peer that uses the legacy reply format.
test tcSyncerLegacyPrompt [main = LegacyPromptDriver]:
  assert OneOutstandingQuery, NoCrossAttemptCredit, WholeStreamCredit,
    OutcomeExactlyOnce, CompletedQueriedMissing, PromptPeerNeverFaulted in
  { LegacyPromptDriver, World, PeerSyncer };

// Counterexample test cases: each must find the bug its profile introduces.

// Without Draining, a slow peer's abandoned stream is credited to the next
// attempt.
test tcSyncerNoDrainingCounterexample [main = NoDrainingDriver]:
  assert NoCrossAttemptCredit in
  { NoDrainingDriver, World, PeerSyncer };

// Without the first-reply check, the tail of an earlier stream completes a
// new attempt once the drain timer has given up on it.
test tcSyncerNoFirstReplyCheckCounterexample
  [main = NoFirstReplyCheckDriver]:
  assert WholeStreamCredit in
  { NoFirstReplyCheckDriver, World, PeerSyncer };

// Under the old drain rule, Draining ended a legacy peer's stream on its
// first reply, since every legacy reply covers the query's last block. The
// rest of the stream skips the range checks and is credited to the next
// attempt. The model found this independently of the review.
test tcSyncerLegacyDrainCounterexample [main = LegacyDrainDriver]:
  assert NoCrossAttemptCredit in
  { LegacyDrainDriver, World, PeerSyncer };

// With a drain deadline fixed at abandonment, as before the drain timer was
// an inactivity timer, a slow peer's prompt tail outlasts the drain and is
// credited to the next attempt.
test tcSyncerFixedDrainDeadlineCounterexample
  [main = FixedDrainDeadlineDriver]:
  assert NoCrossAttemptCredit in
  { FixedDrainDeadlineDriver, World, PeerSyncer };

// Findings: production profiles that are expected to fail, because they
// document a limit of the current design. See SPEC.md, open questions.

// A peer that pauses past the timeout twice in one stream: the first pause
// fails the attempt, and the second outlasts the drain, so the rest of the
// stream, or all of it if none had arrived, reaches the next attempt. An
// inactivity timer cannot tell that second pause from a peer that stopped.
// Draining is only as good as assumption A-DRAIN, and the first-reply check
// is what keeps the damage to a whole, equivalent stream.
test tcSyncerCrossCreditBeyondDrainFinding [main = VerySlowDriver]:
  assert NoCrossAttemptCredit in
  { VerySlowDriver, World, PeerSyncer };
