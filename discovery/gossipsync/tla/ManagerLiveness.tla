--------------------------- MODULE ManagerLiveness ---------------------------
(***************************************************************************)
(* ManagerLiveness states the contract of the gossip sync manager in       *)
(* discovery/gossipsync (ManagerState in manager_state.go). It is the TLA+ *)
(* sibling of pmodel/src/manager.p, and SPEC.md section 6 is the prose     *)
(* version of what it checks.                                              *)
(*                                                                         *)
(* This module is plain TLA+, not PlusCal. In the Go code and in the       *)
(* contract, every input to the manager is handled and then settled in     *)
(* one transition, and the settle step makes choices of its own. PlusCal   *)
(* would split that into several labeled steps, and TLC would then explore *)
(* states halfway through a transition that no one can observe. So each    *)
(* input is written as a pure operator from a manager state to the set of  *)
(* states the contract allows next, like a protofsm ProcessEvent that      *)
(* returns every allowed outcome at once, and one TLA+ action applies the  *)
(* input and settle together.                                              *)
(*                                                                         *)
(* The headline of the module is liveness. SPEC.md marks GSM-016 and       *)
(* GSM-019, "the graph eventually syncs", as verified only under an        *)
(* informal assumption A-FAIR, which the P models encode as test drivers   *)
(* that fail each syncer at most once. Here A-FAIR is a TLA+ fairness      *)
(* formula, Fairness below, and TLC checks the liveness properties against *)
(* every fair behavior at the chosen scope. The configurations that drop   *)
(* one conjunct of Fairness at a time show which parts of it are needed.   *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

(***************************************************************************)
(*   Peers        the public keys that may connect.                        *)
(*   MaxSessions  how many connections may happen in a behavior. Every     *)
(*                connection is a new session, so this bounds the churn.   *)
(*   MaxQuota     the largest NumActiveSyncers TLC tries.                  *)
(*   Faults       whether a syncer may fail an attempt at all.             *)
(*                                                                         *)
(* The last three constants each remove one rule of the production         *)
(* manager, for the configurations that must produce a counterexample:     *)
(*                                                                         *)
(*   NoSettle        no settle step runs after an input.                   *)
(*   NoPinnedRetry   a pinned peer whose attempt failed is never retried.  *)
(*   NoLocalBackoff  a local fault doesn't record a failure epoch.         *)
(***************************************************************************)
CONSTANTS Peers, MaxSessions, MaxQuota, Faults,
          NoSettle, NoPinnedRetry, NoLocalBackoff

SessionIds == 1..MaxSessions

(***************************************************************************)
(* Attempt IDs are unbounded in the Go code, and so would be the state     *)
(* space, since historical ticks start attempts forever. But the manager   *)
(* only ever compares an attempt ID for equality with the IDs it has in    *)
(* flight. So the model reuses IDs: a new attempt gets the smallest ID     *)
(* that nothing in the system still refers to, which the manager can't     *)
(* tell apart from a brand new one. Every session holds at most one live   *)
(* ID, so MaxSessions + 1 IDs are enough, and IdsSuffice checks it.        *)
(***************************************************************************)
AttemptIds == 1..(MaxSessions + 1)

(***************************************************************************)
(* Failure epochs are unbounded counters too, but the contract only reads  *)
(* whether a peer failed in the current epoch or the one just closed. The  *)
(* Go code prunes older ones for the same reason. So an epoch is recorded  *)
(* as an age: "now", "last", or "none". A historical tick ages every entry *)
(* by one.                                                                 *)
(***************************************************************************)
Ages == {"now", "last", "none"}
Older(x) == IF x = "now" THEN "last" ELSE "none"

Kinds == {"completed", "peerFault", "localFault", "busy"}

Min(S) == CHOOSE x \in S : \A y \in S : x <= y
MinOf(a, b) == IF a < b THEN a ELSE b

\* A function with an empty domain. f @@ g and x :> y, from the TLC module,
\* merge two functions and build a one-element function.
EmptyFn == [x \in {} |-> 0]
Drop(f, S) == [x \in DOMAIN f \ S |-> f[x]]

(***************************************************************************)
(* The variables. quota and pinned never change: TLC picks them in the     *)
(* initial state, so one run covers every quota up to MaxQuota and every   *)
(* choice of pinned peers.                                                 *)
(*                                                                         *)
(*   mgr     the manager's state, a record:                                *)
(*             members   session -> [pub, role, retry], the connections.   *)
(*             inFlight  attempt -> session, the attempts in flight.       *)
(*             tracked   the tracked attempt, or 0.                        *)
(*             failedAt  peer -> age of its last failure.                  *)
(*             synced    whether the graph is synced.                      *)
(*             next      the next session ID.                              *)
(*   sync    session -> the abstract syncer of that session.               *)
(*   startq  the <<session, attempt>> starts sent to syncers and not yet   *)
(*           taken.                                                        *)
(*   msgs    the outcomes sent to the manager and not yet handled.         *)
(*   ledger  a ghost copy of the failure ages, kept by the environment     *)
(*           from the outcomes alone, so the properties don't trust the    *)
(*           manager's own bookkeeping.                                    *)
(***************************************************************************)
VARIABLES quota, pinned, mgr, sync, startq, msgs, ledger

vars == <<quota, pinned, mgr, sync, startq, msgs, ledger>>

-----------------------------------------------------------------------------
(***************************************************************************)
(* Helpers over a manager state m. They take m as an argument, rather than *)
(* reading mgr, so that the handlers below can chain them on intermediate  *)
(* states within one transition.                                           *)
(***************************************************************************)
Sessions(m) == DOMAIN m.members
PubOf(m, s) == m.members[s].pub
Connected(m) == {PubOf(m, s) : s \in Sessions(m)}
SessionOf(m, p) == CHOOSE s \in Sessions(m) : PubOf(m, s) = p
IsPinned(m, s) == PubOf(m, s) \in pinned
NonPinned(m) == {s \in Sessions(m) : ~IsPinned(m, s)}
WithRole(m, r) == {s \in NonPinned(m) : m.members[s].role = r}
Busy(m, s) == \E a \in DOMAIN m.inFlight : m.inFlight[a] = s

\* Eligible is the set of sessions a historical sync may run on, other than
\* the excluded one. Session IDs start at 1, so 0 excludes nothing.
Eligible(m, ex) ==
    {s \in NonPinned(m) :
        ~Busy(m, s) /\ m.failedAt[PubOf(m, s)] /= "now" /\ s /= ex}

\* The attempt IDs something outside the manager still refers to.
LiveIds ==
    {x[2] : x \in startq} \cup {o.a : o \in msgs} \cup
    {sync[s].att : s \in SessionIds}

Fresh(m) == Min(AttemptIds \ (LiveIds \cup DOMAIN m.inFlight \cup {0}))

\* StartAttempt starts a historical sync on s. A tracked attempt is the one
\* the manager fails over while the graph is unsynced.
StartAttempt(m, s, isTracked) ==
    LET a == Fresh(m) IN
    [m EXCEPT !.inFlight = @ @@ (a :> s),
              !.tracked = IF isTracked THEN a ELSE @]

-----------------------------------------------------------------------------
(***************************************************************************)
(* Settle restores the manager's two goals after every input. It returns a *)
(* set, since the contract lets it choose: which eligible peer starts the  *)
(* tracked attempt, and which passive peers are promoted. The Go code      *)
(* promotes one random peer at a time until the quota is met; the set of   *)
(* all subsets of the right size is every outcome of those picks.          *)
(***************************************************************************)
Promote(m, P) ==
    [m EXCEPT !.members =
        [s \in DOMAIN m.members |->
            IF s \in P THEN [m.members[s] EXCEPT !.role = "active"]
                       ELSE m.members[s]]]

Settle(m) ==
    IF NoSettle THEN {m}
    ELSE IF m.synced
    THEN LET passive == WithRole(m, "passive")
             want == quota - Cardinality(WithRole(m, "active"))
             need == MinOf(IF want > 0 THEN want ELSE 0, Cardinality(passive))
         IN  {Promote(m, P) : P \in {P \in SUBSET passive :
                                        Cardinality(P) = need}}
    ELSE IF m.tracked = 0 /\ quota > 0 /\ Eligible(m, 0) /= {}
    THEN {StartAttempt(m, s, TRUE) : s \in Eligible(m, 0)}
    ELSE {m}

(***************************************************************************)
(* The input handlers. Each only records what the input means, and leaves  *)
(* the goals to Settle, except for the two starts that depend on the peer  *)
(* itself: a pinned peer's own sync, and the resync by the first peer back *)
(* after the node lost every peer (GSM-006 and GSM-007).                   *)
(***************************************************************************)
OnConnect(m, p) ==
    LET s == m.next
        hadNoPeers == NonPinned(m) = {}
        m1 == [m EXCEPT
                !.members = @ @@ (s :> [pub   |-> p,
                                        role  |-> IF p \in pinned
                                                  THEN "pinned"
                                                  ELSE "passive",
                                        retry |-> FALSE]),
                !.next = @ + 1]
    IN  IF p \in pinned THEN StartAttempt(m1, s, FALSE)
        ELSE IF m.synced /\ hadNoPeers /\ quota > 0 /\ m.failedAt[p] /= "now"
        THEN StartAttempt(m1, s, FALSE)
        ELSE m1

OnDisconnect(m, s) ==
    LET gone == {a \in DOMAIN m.inFlight : m.inFlight[a] = s} IN
    [m EXCEPT !.members = Drop(@, {s}),
              !.inFlight = Drop(@, gone),
              !.tracked = IF @ \in gone THEN 0 ELSE @]

\* An outcome counts only if its attempt is in flight on the session that
\* reports it (GSM-011).
Counts(m, o) == o.a \in DOMAIN m.inFlight /\ m.inFlight[o.a] = o.s

OnOutcome(m, o) ==
    IF ~Counts(m, o) THEN m
    ELSE LET m1 == [m EXCEPT !.inFlight = Drop(@, {o.a}),
                             !.tracked = IF @ = o.a THEN 0 ELSE @]
             p == PubOf(m, o.s)
         IN  CASE o.k = "completed" ->
                    IF m1.synced THEN m1
                    ELSE [m1 EXCEPT !.synced = TRUE, !.tracked = 0]
               [] o.k = "localFault" /\ NoLocalBackoff -> m1
               [] p \in pinned ->
                    IF NoPinnedRetry THEN m1
                    ELSE [m1 EXCEPT !.members[o.s].retry = TRUE]
               [] OTHER -> [m1 EXCEPT !.failedAt[p] = "now"]

(***************************************************************************)
(* RetryPinned starts an untracked attempt on every pinned session marked  *)
(* to retry that has none in flight. It is a fold over a set, written with *)
(* RECURSIVE, in ascending session order. The order is not a choice the    *)
(* contract makes: any order starts the same attempts.                     *)
(***************************************************************************)
RECURSIVE RetryAll(_, _)
RetryAll(m, S) ==
    IF S = {} THEN m
    ELSE LET s == Min(S) IN
         RetryAll(StartAttempt([m EXCEPT !.members[s].retry = FALSE], s, FALSE),
                  S \ {s})

RetryPinned(m) ==
    RetryAll(m, {s \in Sessions(m) :
                    IsPinned(m, s) /\ m.members[s].retry /\ ~Busy(m, s)})

\* A historical tick opens a new epoch, retries the pinned peers, and starts
\* an attempt on an eligible peer other than the tracked attempt's,
\* preferring peers that didn't fail in the epoch just closed (GSM-015).
OnTick(m) ==
    LET m1 == RetryPinned([m EXCEPT !.failedAt = [p \in Peers |-> Older(@[p])]])
        ex == IF m1.tracked = 0 THEN 0 ELSE m1.inFlight[m1.tracked]
        c == Eligible(m1, ex)
        pref == {s \in c : m1.failedAt[PubOf(m1, s)] /= "last"}
        pick == IF pref /= {} THEN pref ELSE c
    IN  IF quota = 0 \/ pick = {} THEN {m1}
        ELSE {StartAttempt(m1, s, ~m1.synced) : s \in pick}

\* A rotation swaps any active peer for any passive one.
OnRotate(m) ==
    LET A == WithRole(m, "active")
        P == WithRole(m, "passive")
    IN  IF A = {} \/ P = {} THEN {m}
        ELSE {[m EXCEPT !.members[a].role = "passive",
                        !.members[b].role = "active"] : a \in A, b \in P}

-----------------------------------------------------------------------------
(***************************************************************************)
(* The initial state and the actions. An action is a formula relating the  *)
(* current state (unprimed variables) to the next (primed ones). It is     *)
(* enabled in a state if some next state satisfies it, and every variable  *)
(* it doesn't mention must be left UNCHANGED explicitly.                   *)
(***************************************************************************)
Init ==
    /\ quota \in 0..MaxQuota
    /\ pinned \in SUBSET Peers
    /\ mgr = [members  |-> EmptyFn,
              inFlight |-> EmptyFn,
              tracked  |-> 0,
              failedAt |-> [p \in Peers |-> "none"],
              synced   |-> FALSE,
              next     |-> 1]
    /\ sync = [s \in SessionIds |-> [st |-> "none", att |-> 0]]
    /\ startq = {}
    /\ msgs = {}
    /\ ledger = [p \in Peers |-> "none"]

\* Apply commits the manager's next state, and sends a start to the syncer
\* of every attempt it started. Starts for sessions that are gone are
\* dropped, since their syncers were stopped.
Apply(m) ==
    /\ mgr' = m
    /\ startq' = {x \in startq : x[1] \in Sessions(m)} \cup
                 {<<m.inFlight[a], a>> : a \in DOMAIN m.inFlight \ DOMAIN mgr.inFlight}

Connect(p) ==
    /\ p \notin Connected(mgr)
    /\ mgr.next <= MaxSessions
    /\ \E m \in Settle(OnConnect(mgr, p)) : Apply(m)
    /\ sync' = [sync EXCEPT ![mgr.next] = [st |-> "idle", att |-> 0]]
    /\ UNCHANGED <<quota, pinned, msgs, ledger>>

Disconnect(p) ==
    /\ p \in Connected(mgr)
    /\ LET s == SessionOf(mgr, p) IN
       /\ \E m \in Settle(OnDisconnect(mgr, s)) : Apply(m)
       /\ sync' = [sync EXCEPT ![s] = [st |-> "stopped", att |-> 0]]
    /\ UNCHANGED <<quota, pinned, msgs, ledger>>

\* The manager handles outcome o. The ledger records a failure of a peer
\* that isn't pinned, whatever the manager itself does with it.
Handle(o) ==
    /\ o \in msgs
    /\ \E m \in Settle(OnOutcome(mgr, o)) : Apply(m)
    /\ msgs' = msgs \ {o}
    /\ ledger' = IF Counts(mgr, o) /\ o.k /= "completed" /\ ~IsPinned(mgr, o.s)
                 THEN [ledger EXCEPT ![PubOf(mgr, o.s)] = "now"]
                 ELSE ledger
    /\ UNCHANGED <<quota, pinned, sync>>

HistTick ==
    /\ \E m1 \in OnTick(mgr) : \E m \in Settle(m1) : Apply(m)
    /\ ledger' = [p \in Peers |-> Older(ledger[p])]
    /\ UNCHANGED <<quota, pinned, sync, msgs>>

Rotate ==
    /\ \E m1 \in OnRotate(mgr) : \E m \in Settle(m1) : Apply(m)
    /\ UNCHANGED <<quota, pinned, sync, msgs, ledger>>

(***************************************************************************)
(* The abstract syncer of session s. It takes a start and works on it, or  *)
(* refuses it as busy while it is working or draining. A working syncer    *)
(* completes, or fails with a peer fault, after which it drains for a      *)
(* while, or with a local fault. The syncer's own contract is the other    *)
(* spec, SyncerPairing; the two meet only at these outcome kinds.          *)
(***************************************************************************)
Take(s) ==
    \E x \in startq :
        /\ x[1] = s
        /\ sync[s].st \in {"idle", "working", "draining"}
        /\ startq' = startq \ {x}
        /\ IF sync[s].st = "idle"
           THEN /\ sync' = [sync EXCEPT ![s] = [st |-> "working", att |-> x[2]]]
                /\ UNCHANGED msgs
           ELSE /\ msgs' = msgs \cup {[s |-> s, a |-> x[2], k |-> "busy"]}
                /\ UNCHANGED sync
        /\ UNCHANGED <<quota, pinned, mgr, ledger>>

Complete(s) ==
    /\ sync[s].st = "working"
    /\ msgs' = msgs \cup {[s |-> s, a |-> sync[s].att, k |-> "completed"]}
    /\ sync' = [sync EXCEPT ![s] = [st |-> "idle", att |-> 0]]
    /\ UNCHANGED <<quota, pinned, mgr, startq, ledger>>

Fail(s) ==
    /\ Faults
    /\ sync[s].st = "working"
    /\ \E k \in {"peerFault", "localFault"} :
        /\ msgs' = msgs \cup {[s |-> s, a |-> sync[s].att, k |-> k]}
        /\ sync' = [sync EXCEPT ![s] =
                        [st |-> IF k = "peerFault" THEN "draining" ELSE "idle",
                         att |-> 0]]
    /\ UNCHANGED <<quota, pinned, mgr, startq, ledger>>

Report(s) == Complete(s) \/ Fail(s)

FinishDrain(s) ==
    /\ sync[s].st = "draining"
    /\ sync' = [sync EXCEPT ![s] = [st |-> "idle", att |-> 0]]
    /\ UNCHANGED <<quota, pinned, mgr, startq, msgs, ledger>>

Next ==
    \/ \E p \in Peers : Connect(p) \/ Disconnect(p)
    \/ \E o \in msgs : Handle(o)
    \/ HistTick
    \/ Rotate
    \/ \E s \in SessionIds : Take(s) \/ Report(s) \/ FinishDrain(s)

-----------------------------------------------------------------------------
(***************************************************************************)
(* Fairness. [][Next]_vars only says what a step may do; it allows a       *)
(* behavior that stops, or that takes rotations forever and never delivers *)
(* an outcome. Fairness rules those out. For an action A:                  *)
(*                                                                         *)
(*   WF_vars(A), weak fairness: if A is enabled forever from some point    *)
(*   on, an A step eventually happens.                                     *)
(*                                                                         *)
(*   SF_vars(A), strong fairness: if A is enabled again and again, even if *)
(*   it is disabled in between, an A step eventually happens.              *)
(*                                                                         *)
(* Fairness is A-FAIR stated precisely, one conjunct per assumption about  *)
(* the world:                                                              *)
(*                                                                         *)
(*   TakeFairness        a syncer takes the start it was sent.             *)
(*   DrainFairness       a draining syncer eventually goes idle.           *)
(*   DeliveryFairness    the manager eventually handles every session's    *)
(*                       outcome. Each session has at most one outcome     *)
(*                       pending at a time, so this is per outcome.        *)
(*   TickFairness        historical ticks keep coming. A tick that would   *)
(*                       change nothing is not enabled as a step, so this  *)
(*                       asks for ticks only while they matter.            *)
(*   CompletionFairness  a peer that is given attempts again and again     *)
(*                       eventually completes one. Complete(s) is enabled  *)
(*                       only while s is working, and each failure         *)
(*                       disables it, so weak fairness would not be        *)
(*                       enough; ManagerWeakCompletion shows it.           *)
(*                                                                         *)
(* No conjunct says that a working syncer eventually reports. It would be  *)
(* WF_vars(Report(s)), and it is implied: Report(s) and Complete(s) are    *)
(* enabled in exactly the same states, so if Report(s) is enabled forever, *)
(* so is Complete(s), and CompletionFairness makes it happen. Every other  *)
(* conjunct is needed, and each has a configuration that drops it and      *)
(* must fail.                                                              *)
(*                                                                         *)
(* This is weaker than the P drivers' version of A-FAIR, which lets each   *)
(* syncer fail at most once: here a peer may fail any number of times, as  *)
(* long as it doesn't fail forever.                                        *)
(***************************************************************************)
TakeFairness == \A s \in SessionIds : WF_vars(Take(s))

DrainFairness == \A s \in SessionIds : WF_vars(FinishDrain(s))

DeliveryFairness ==
    \A s \in SessionIds : WF_vars(\E o \in msgs : o.s = s /\ Handle(o))

TickFairness == WF_vars(HistTick)

CompletionFairness == \A s \in SessionIds : SF_vars(Complete(s))

WeakCompletionFairness == \A s \in SessionIds : WF_vars(Complete(s))

Fairness ==
    /\ TakeFairness
    /\ DrainFairness
    /\ DeliveryFairness
    /\ TickFairness
    /\ CompletionFairness

\* The specifications. A .cfg picks one with SPECIFICATION. SafetySpec has
\* no fairness, which safety properties never need. Each of the others
\* drops or weakens one conjunct of Fairness.
SafetySpec == Init /\ [][Next]_vars

Spec == Init /\ [][Next]_vars /\ Fairness

SpecNoTakeFairness ==
    Init /\ [][Next]_vars
         /\ DrainFairness /\ DeliveryFairness /\ TickFairness
         /\ CompletionFairness

SpecNoDrainFairness ==
    Init /\ [][Next]_vars
         /\ TakeFairness /\ DeliveryFairness /\ TickFairness
         /\ CompletionFairness

SpecNoDeliveryFairness ==
    Init /\ [][Next]_vars
         /\ TakeFairness /\ DrainFairness /\ TickFairness
         /\ CompletionFairness

SpecNoTickFairness ==
    Init /\ [][Next]_vars
         /\ TakeFairness /\ DrainFairness /\ DeliveryFairness
         /\ CompletionFairness

SpecWeakCompletion ==
    Init /\ [][Next]_vars
         /\ TakeFairness /\ DrainFairness /\ DeliveryFairness
         /\ TickFairness /\ WeakCompletionFairness

-----------------------------------------------------------------------------
(***************************************************************************)
(* Safety: the goals (a) to (h) of the P ManagerContract monitor. Goals    *)
(* about a single state are invariants. Goals about what a step may do are *)
(* action properties, written [][A]_vars: every step either satisfies A    *)
(* or leaves vars unchanged. In an action property, mgr' is the manager    *)
(* after the step and mgr the manager before it.                           *)
(***************************************************************************)
TypeOK ==
    /\ quota \in 0..MaxQuota
    /\ pinned \subseteq Peers
    /\ DOMAIN mgr.inFlight \subseteq AttemptIds
    /\ mgr.tracked \in {0} \cup DOMAIN mgr.inFlight
    /\ \A s \in Sessions(mgr) :
           mgr.members[s].role \in {"passive", "active", "pinned"}
    /\ \A o \in msgs : o.k \in Kinds

\* Every session holds at most one live attempt ID, so the reused IDs never
\* run out.
IdsSuffice ==
    Cardinality((LiveIds \cup DOMAIN mgr.inFlight) \ {0})
        < Cardinality(AttemptIds)

\* The sessions eligible by the ledger, not by the manager's own records.
LedgerEligible ==
    {s \in NonPinned(mgr) : ~Busy(mgr, s) /\ ledger[PubOf(mgr, s)] /= "now"}

ActiveCount == Cardinality(WithRole(mgr, "active"))

\* (a) GSM-001: while unsynced with a quota, if some peer is eligible, a
\* tracked attempt is in flight.
GoalA ==
    ~mgr.synced /\ quota > 0 /\ LedgerEligible /= {} => mgr.tracked /= 0

\* (b) GSM-002: once synced, the active quota is as full as it can be.
GoalB ==
    mgr.synced => ActiveCount = MinOf(quota, Cardinality(NonPinned(mgr)))

\* (c) GSM-003: the active count never exceeds the quota.
GoalC == ActiveCount <= quota

\* (d) GSM-005: pinned peers are pinned, and no other peer is.
GoalD ==
    \A s \in Sessions(mgr) :
        (mgr.members[s].role = "pinned") <=> IsPinned(mgr, s)

\* (e) GSM-010: at most one attempt is in flight per session.
GoalE ==
    \A a, b \in DOMAIN mgr.inFlight : a /= b => mgr.inFlight[a] /= mgr.inFlight[b]

\* (f) GSM-011: a stale outcome changes nothing observable. The step that
\* handles o is the one that removes it from msgs.
Observable == <<mgr.members, mgr.inFlight, mgr.tracked, mgr.synced>>

GoalF ==
    [][\A o \in msgs :
          (o \notin msgs' /\ ~Counts(mgr, o)) => Observable' = Observable]_vars

\* (g) GSM-012: synced never goes back, and only a Completed outcome that
\* counts sets it.
GoalG ==
    [][/\ mgr.synced => mgr'.synced
       /\ (~mgr.synced /\ mgr'.synced) =>
              \E o \in msgs : o \notin msgs' /\ o.k = "completed" /\ Counts(mgr, o)
      ]_vars

\* (h) GSM-013: a peer that isn't pinned is never started in the epoch in
\* which it failed, by the ledger's account.
GoalH ==
    [][\A a \in DOMAIN mgr'.inFlight \ DOMAIN mgr.inFlight :
          LET p == PubOf(mgr', mgr'.inFlight[a]) IN
          p \notin pinned => ledger'[p] /= "now"]_vars

(***************************************************************************)
(* Liveness. <>P means P holds eventually, and []P that it holds from now  *)
(* on, so <>[]P means P holds from some point on. Connections are bounded  *)
(* by MaxSessions, so every behavior has a last connect or disconnect, and *)
(* after it the set of connected peers never changes.                      *)
(*                                                                         *)
(* GSM-016: with a non-zero quota, if a non-pinned peer stays connected    *)
(* from some point on, the graph eventually syncs.                         *)
(*                                                                         *)
(* GSM-019: whatever the quota, if a pinned peer stays connected from some *)
(* point on, the graph eventually syncs.                                   *)
(***************************************************************************)
HasNonPinned == NonPinned(mgr) /= {}
HasPinned == \E s \in Sessions(mgr) : IsPinned(mgr, s)

GraphEventuallySynced ==
    quota > 0 /\ <>[]HasNonPinned => <>mgr.synced

GraphEventuallySyncedWithPinned ==
    <>[]HasPinned => <>mgr.synced

=============================================================================
