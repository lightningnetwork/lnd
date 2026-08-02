// Executable trampoline relay, sender, downstream, recipient, restart, and
// chain-height model. The transition rules mirror routing/trampoline's Go
// RelayMachine.

machine TrampolineRelay {
    var controller: machine;
    var downstream: machine;
    var config: TrampolineConfig;
    var relay_state: TrampolineRelayState;
    var parts: map[int, TrampolinePart];
    var resolutions: map[int, TrampolineResolution];
    var collected_msat: int;
    var attempts: int;
    var active_attempt: int;
    var height: int;
    var peak_height: int;
    var hold_deadline: int;
    var recovery_deadline: int;
    var attempt_scheduled: bool;
    var dispatch_epoch: int;

    start state Running {
        entry (init: RelayInit) {
            controller = init.controller;
            downstream = init.downstream;
            config = init.config;
            relay_state = Collecting;
            collected_msat = 0;
            attempts = 0;
            active_attempt = 0;
            height = 0;
            peak_height = 0;
            hold_deadline = config.hold_deadline;
            recovery_deadline = 0;
            attempt_scheduled = false;
            dispatch_epoch = 0;

            assert config.expected_total_msat > 0,
                "expected total must be positive";
            assert config.downstream_msat > 0 &&
                config.downstream_msat <= config.expected_total_msat,
                "downstream amount must conserve value";
            assert config.max_parts > 0 && config.max_attempts > 0,
                "resource bounds must be positive";
            assert config.hold_deadline > 0 &&
                config.recovery_window > 0 &&
                config.expiry_safety_blocks > 0,
                "time bounds must be positive";

            AnnounceState();
        }

        on eAddPart do (part: TrampolinePart) {
            if (IsTerminal()) {
                return;
            }

            if (part.id in parts) {
                if (parts[part.id].amount_msat == part.amount_msat &&
                    parts[part.id].cltv == part.cltv) {

                    announce ePartReplay, (
                        part_id = part.id, mutated = false
                    );
                    return;
                }

                announce ePartReplay, (
                    part_id = part.id, mutated = false
                );
                return;
            }

            if (relay_state != Collecting ||
                part.amount_msat <= 0 ||
                part.cltv < config.required_cltv ||
                part.cltv <= config.expiry_safety_blocks ||
                part.cltv - config.expiry_safety_blocks <= height ||
                sizeof(parts) >= config.max_parts ||
                collected_msat + part.amount_msat >
                    config.expected_total_msat) {

                return;
            }

            parts[part.id] = part;
            resolutions[part.id] = Unresolved;
            collected_msat = collected_msat + part.amount_msat;
            if (part.cltv - config.expiry_safety_blocks <
                hold_deadline) {

                hold_deadline =
                    part.cltv - config.expiry_safety_blocks;
            }
            announce ePartAccepted, part;

            if (collected_msat == config.expected_total_msat) {
                relay_state = Ready;
                AnnounceState();
                send this, eNotifyReady;
            }
        }

        on eNotifyReady do {
            if (relay_state != Ready || attempt_scheduled) {
                return;
            }

            attempt_scheduled = true;
            announce eDispatchScheduled, (
                epoch = dispatch_epoch,
                attempt = attempts + 1
            );
            send this, eBeginAttempt;
        }

        on eBeginAttempt do {
            if (!attempt_scheduled || IsTerminal()) {
                return;
            }
            attempt_scheduled = false;
            if (relay_state != Ready) {
                return;
            }
            if (attempts >= config.max_attempts) {
                ResolveAll(Failed);
                return;
            }

            attempts = attempts + 1;
            active_attempt = attempts;
            relay_state = Paying;
            announce eForwardStarted, (
                attempt = active_attempt,
                collected_msat = collected_msat,
                downstream_msat = config.downstream_msat,
                incoming_cltv = config.required_cltv,
                required_cltv = config.required_cltv,
                max_attempts = config.max_attempts
            );
            AnnounceState();
            send downstream, eDownstreamPay, (
                relay = this,
                attempt = active_attempt,
                amount_msat = config.downstream_msat,
                cltv = config.required_cltv
            );
        }

        on eAttemptSuccess do (attempt: int) {
            if (IsTerminal()) {
                return;
            }
            if ((relay_state != Paying &&
                relay_state != Recovering &&
                relay_state != Canceling) ||
                attempt != active_attempt) {

                return;
            }

            ResolveAll(Succeeded);
        }

        on eAttemptFailure do (result: (attempt: int, retryable: bool)) {
            if (IsTerminal()) {
                return;
            }
            if ((relay_state != Paying &&
                relay_state != Recovering &&
                relay_state != Canceling) ||
                result.attempt != active_attempt) {

                return;
            }

            if (relay_state == Canceling) {
                ResolveAll(TimedOut);
                return;
            }

            if (result.retryable &&
                attempts < config.max_attempts &&
                height < hold_deadline) {

                relay_state = Ready;
                active_attempt = 0;
                recovery_deadline = 0;
                AnnounceState();
                send this, eNotifyReady;
                return;
            }

            ResolveAll(Failed);
        }

        on eCancel do {
            if (relay_state == Paying ||
                relay_state == Recovering) {

                BeginCancel();
            } else if (relay_state != Canceling && !IsTerminal()) {
                ResolveAll(Canceled);
            }
        }

        on eRestart do {
            attempt_scheduled = false;
            dispatch_epoch = dispatch_epoch + 1;
            if (relay_state == Ready) {
                send this, eNotifyReady;
                return;
            }
            if (relay_state == Paying) {
                if (height >= hold_deadline) {
                    BeginCancel();
                    send downstream, eDownstreamReconcile, (
                        relay = this,
                        attempt = active_attempt
                    );
                    return;
                }

                relay_state = Recovering;
                recovery_deadline =
                    height + config.recovery_window;
                if (recovery_deadline > hold_deadline) {
                    recovery_deadline = hold_deadline;
                }
                AnnounceState();
                send downstream, eDownstreamReconcile, (
                    relay = this,
                    attempt = active_attempt
                );
            } else if (relay_state == Recovering) {
                send downstream, eDownstreamReconcile, (
                    relay = this,
                    attempt = active_attempt
                );
            } else if (relay_state == Canceling) {
                send downstream, eDownstreamCancel, (
                    relay = this,
                    attempt = active_attempt
                );
                send downstream, eDownstreamReconcile, (
                    relay = this,
                    attempt = active_attempt
                );
            }
        }

        on eReconcileSuccess do (attempt: int) {
            if ((relay_state == Recovering ||
                relay_state == Canceling) &&
                attempt == active_attempt) {

                ResolveAll(Succeeded);
            }
        }

        on eReconcileFailure do (attempt: int) {
            if ((relay_state == Recovering ||
                relay_state == Canceling) &&
                attempt == active_attempt) {

                if (relay_state == Canceling) {
                    ResolveAll(TimedOut);
                    return;
                }

                if (attempts < config.max_attempts &&
                    height < hold_deadline) {

                    relay_state = Ready;
                    active_attempt = 0;
                    recovery_deadline = 0;
                    AnnounceState();
                    send this, eNotifyReady;
                } else {
                    ResolveAll(Failed);
                }
            }
        }

        on eReconcilePending do (attempt: int) {
            if ((relay_state == Recovering ||
                relay_state == Canceling) &&
                attempt == active_attempt) {

                AnnounceState();
            }
        }

        on eObserveHeight do (new_height: int) {
            if (IsTerminal()) {
                return;
            }

            if (new_height < peak_height) {
                if (peak_height - new_height >
                    config.max_reorg_depth) {

                    return;
                }
            }

            height = new_height;
            if (height > peak_height) {
                peak_height = height;
            }

            if (height >= hold_deadline ||
                (relay_state == Recovering &&
                 height >= recovery_deadline)) {

                if (relay_state == Paying ||
                    relay_state == Recovering) {

                    BeginCancel();
                    return;
                } else if (relay_state != Canceling) {
                    ResolveAll(TimedOut);
                    return;
                }
            }

            AnnounceState();
        }
    }

    fun IsTerminal(): bool {
        return relay_state == Succeeded ||
            relay_state == Failed ||
            relay_state == Canceled ||
            relay_state == TimedOut;
    }

    fun BeginCancel() {
        relay_state = Canceling;
        recovery_deadline = 0;
        AnnounceState();
        send downstream, eDownstreamCancel, (
            relay = this,
            attempt = active_attempt
        );
    }

    fun AnnounceState() {
        announce eRelayState, (
            status = relay_state,
            accepted_parts = sizeof(parts),
            collected_msat = collected_msat,
            attempts = attempts,
            height = height,
            hold_deadline = hold_deadline,
            recovery_deadline = recovery_deadline,
            max_attempts = config.max_attempts
        );
    }

    fun ResolveAll(terminal: TrampolineRelayState) {
        var part_id: int;
        var resolution: TrampolineResolution;

        assert terminal == Succeeded ||
            terminal == Failed ||
            terminal == Canceled ||
            terminal == TimedOut,
            "resolution requires a terminal state";

        relay_state = terminal;
        active_attempt = 0;
        recovery_deadline = 0;
        if (terminal == Succeeded) {
            resolution = Fulfilled;
        } else {
            resolution = Rejected;
        }

        foreach (part_id in keys(parts)) {
            if (resolutions[part_id] == Unresolved) {
                resolutions[part_id] = resolution;
                send controller, ePartResolved, (
                    part_id = part_id,
                    amount_msat = parts[part_id].amount_msat,
                    resolution = resolution
                );
            }
        }

        announce eRelayTerminal, terminal;
        AnnounceState();
    }
}

machine TrampolineSender {
    var relay: TrampolineRelay;
    var parts: seq[TrampolinePart];
    var resolutions: map[int, TrampolineResolution];

    start state Sending {
        entry (init: SenderInit) {
            var i: int;
            parts = init.parts;
            relay = new TrampolineRelay((
                controller = this,
                downstream = init.downstream,
                config = init.config
            ));
            i = 0;
            while (i < sizeof(parts)) {
                send relay, eAddPart, parts[i];
                i = i + 1;
            }
        }

        on ePartResolved do (result: PartResolutionEvent) {
            if (!(result.part_id in resolutions)) {
                resolutions[result.part_id] = result.resolution;
            }
        }
    }
}

machine ChainEnvironment {
    var relay: machine;
    var height: int;

    start state Running {
        entry (init: ChainInit) {
            relay = init.relay;
            height = init.height;
        }

        on eMineBlocks do (count: int) {
            var i: int;

            assert count >= 0, "block count cannot be negative";
            i = 0;
            while (i < count) {
                height = height + 1;
                send relay, eObserveHeight, height;
                i = i + 1;
            }
        }

        on eChainReorg do (depth: int) {
            assert depth >= 0 && depth <= height,
                "reorg depth must fit the observed chain";
            height = height - depth;
            send relay, eObserveHeight, height;
        }
    }
}

machine DownstreamPayment {
    var recipient: machine;
    var mode: DownstreamMode;
    var requests: int;
    var settled: bool;
    var amount_msat: int;

    start state Serving {
        entry (init: DownstreamInit) {
            recipient = init.recipient;
            mode = init.mode;
            requests = 0;
            settled = false;
            amount_msat = 0;
        }

        on eDownstreamPay do (request: DownstreamRequest) {
            requests = requests + 1;
            amount_msat = request.amount_msat;

            if (mode == DownstreamSuccess ||
                (mode == DownstreamRetryThenSuccess &&
                 requests > 1)) {

                if (!settled) {
                    settled = true;
                    send recipient, eRecipientSettled,
                        request.amount_msat;
                }
                send request.relay, eAttemptSuccess,
                    request.attempt;
            } else if (mode == DownstreamRetryThenSuccess) {
                send request.relay, eAttemptFailure, (
                    attempt = request.attempt,
                    retryable = true
                );
            } else if (mode == DownstreamFailure) {
                send request.relay, eAttemptFailure, (
                    attempt = request.attempt,
                    retryable = true
                );
            }
        }

        on eDownstreamReconcile do (request: ReconcileRequest) {
            if (settled) {
                send request.relay, eReconcileSuccess,
                    request.attempt;
            } else if (mode == DownstreamFailure) {
                send request.relay, eReconcileFailure,
                    request.attempt;
            } else {
                send request.relay, eReconcilePending,
                    request.attempt;
            }
        }

        on eDownstreamCancel do (request: ReconcileRequest) {
            if (mode == DownstreamCancelSettles) {
                if (!settled) {
                    settled = true;
                    send recipient, eRecipientSettled,
                        amount_msat;
                }
                send request.relay, eAttemptSuccess,
                    request.attempt;
            } else if (settled) {
                send request.relay, eAttemptSuccess,
                    request.attempt;
            } else {
                send request.relay, eAttemptFailure, (
                    attempt = request.attempt,
                    retryable = false
                );
            }
        }
    }
}

machine TrampolineRecipient {
    var settled: bool;
    var amount_msat: int;

    start state Receiving {
        entry {
            settled = false;
            amount_msat = 0;
        }

        on eRecipientSettled do (amount: int) {
            assert !settled, "recipient settled more than once";
            settled = true;
            amount_msat = amount;
            announce eRecipientSettlement, amount;
        }
    }
}
