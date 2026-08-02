// Safety and liveness monitors for trampoline routing.

spec NoDuplicatePartResolution observes ePartResolved {
    var resolved: map[int, TrampolineResolution];

    start state Monitoring {
        on ePartResolved do (result: PartResolutionEvent) {
            assert !(result.part_id in resolved),
                "an upstream part was resolved more than once";
            resolved[result.part_id] = result.resolution;
        }
    }
}

spec NoValueCreation observes ePartAccepted, eForwardStarted,
    ePartResolved, eRecipientSettlement {

    var accepted_msat: int;
    var fulfilled_msat: int;

    start state Monitoring {
        entry {
            accepted_msat = 0;
            fulfilled_msat = 0;
        }

        on ePartAccepted do (part: TrampolinePart) {
            accepted_msat = accepted_msat + part.amount_msat;
        }

        on eForwardStarted do (forward: ForwardEvent) {
            assert forward.downstream_msat <= forward.collected_msat,
                "downstream payment created value";
            assert forward.collected_msat == accepted_msat,
                "forwarding omitted or invented an accepted part";
        }

        on ePartResolved do (result: PartResolutionEvent) {
            if (result.resolution == Fulfilled) {
                fulfilled_msat =
                    fulfilled_msat + result.amount_msat;
                assert fulfilled_msat <= accepted_msat,
                    "fulfilled value exceeded accepted value";
            }
        }

        on eRecipientSettlement do (amount: int) {
            assert amount <= accepted_msat,
                "recipient settlement exceeded accepted value";
        }
    }
}

spec FeeAndCLTVSafety observes ePartAccepted, eForwardStarted {
    start state Monitoring {
        on ePartAccepted do (part: TrampolinePart) {
            assert part.amount_msat > 0,
                "accepted part has non-positive amount";
        }

        on eForwardStarted do (forward: ForwardEvent) {
            assert forward.collected_msat >=
                forward.downstream_msat,
                "relay forwarded more value than it collected";
            assert forward.incoming_cltv >=
                forward.required_cltv,
                "relay forwarded without its CLTV budget";
            assert forward.attempt > 0 &&
                forward.attempt <= forward.max_attempts,
                "relay exceeded its retry bound";
        }
    }
}

spec BoundedHoldsAndRetries observes eRelayState {
    start state Monitoring {
        on eRelayState do (update: RelayStateEvent) {
            assert update.attempts <= update.max_attempts,
                "relay exceeded its configured attempt bound";
            if (update.status == Recovering) {
                assert update.recovery_deadline <=
                    update.hold_deadline,
                    "recovery outlived the group hold deadline";
            }
            if (update.status != Succeeded &&
                update.status != Failed &&
                update.status != Canceled &&
                update.status != TimedOut &&
                update.status != Canceling) {

                assert update.height < update.hold_deadline,
                    "non-terminal relay outlived its hold deadline";
            }
        }
    }
}

spec NoForwardAfterTerminal observes eForwardStarted, eRelayTerminal {
    var terminal: bool;

    start state Monitoring {
        entry {
            terminal = false;
        }

        on eForwardStarted do {
            assert !terminal,
                "downstream forwarding occurred after terminal state";
        }

        on eRelayTerminal do {
            assert !terminal, "relay became terminal more than once";
            terminal = true;
        }
    }
}

spec NoDuplicateForwardAttempt observes eForwardStarted {
    var started: map[int, bool];

    start state Monitoring {
        on eForwardStarted do (forward: ForwardEvent) {
            assert !(forward.attempt in started),
                "a downstream attempt was started more than once";
            started[forward.attempt] = true;
        }
    }
}

spec NoDuplicateDispatchSchedule observes eDispatchScheduled {
    var scheduled: map[DispatchEvent, bool];

    start state Monitoring {
        on eDispatchScheduled do (dispatch: DispatchEvent) {
            assert !(dispatch in scheduled),
                "a ready attempt was scheduled twice in one process epoch";
            scheduled[dispatch] = true;
        }
    }
}

spec ReplayIsIdempotent observes ePartReplay {
    start state Monitoring {
        on ePartReplay do (replay: ReplayEvent) {
            assert !replay.mutated,
                "a replay mutated the MPP aggregation state";
        }
    }
}

spec TerminalResolvesAcceptedParts observes ePartAccepted,
    ePartResolved, eRelayTerminal {

    var accepted: map[int, bool];
    var resolved: map[int, bool];

    start state Monitoring {
        on ePartAccepted do (part: TrampolinePart) {
            accepted[part.id] = true;
        }

        on ePartResolved do (result: PartResolutionEvent) {
            resolved[result.part_id] = true;
        }

        on eRelayTerminal do {
            var part_id: int;
            foreach (part_id in keys(accepted)) {
                assert part_id in resolved,
                    "terminal relay left an accepted part unresolved";
            }
        }
    }
}

spec TerminalLiveness observes eRelayState {
    start state Idle {
        on eRelayState do (update: RelayStateEvent) {
            if (update.status == Ready ||
                update.status == Paying ||
                update.status == Recovering ||
                update.status == Canceling) {

                goto AwaitingTerminal;
            }
        }
    }

    hot state AwaitingTerminal {
        on eRelayState do (update: RelayStateEvent) {
            if (update.status == Succeeded ||
                update.status == Failed ||
                update.status == Canceled ||
                update.status == TimedOut) {

                goto Idle;
            }
        }
    }
}
