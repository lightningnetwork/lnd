// Runnable trampoline routing scenarios.

fun TestConfig(): TrampolineConfig {
    return (
        expected_total_msat = 100,
        downstream_msat = 90,
        required_cltv = 20,
        max_parts = 4,
        max_attempts = 3,
        hold_deadline = 40,
        recovery_window = 6,
        expiry_safety_blocks = 1,
        max_reorg_depth = 6
    );
}

fun TestParts(): seq[TrampolinePart] {
    var parts: seq[TrampolinePart];

    parts += (sizeof(parts), (
        id = 2, amount_msat = 40, cltv = 20
    ));
    parts += (sizeof(parts), (
        id = 1, amount_msat = 60, cltv = 20
    ));
    return parts;
}

machine TestTrampolineHappy {
    var relay: TrampolineRelay;
    var downstream: DownstreamPayment;
    var recipient: TrampolineRecipient;

    start state Init {
        entry {
            recipient = new TrampolineRecipient();
            downstream = new DownstreamPayment((
                recipient = recipient,
                mode = DownstreamSuccess
            ));
            relay = new TrampolineRelay((
                controller = this,
                downstream = downstream,
                config = TestConfig()
            ));

            send relay, eAddPart, (
                id = 2, amount_msat = 40, cltv = 20
            );
            send relay, eAddPart, (
                id = 1, amount_msat = 60, cltv = 20
            );
            send relay, eAddPart, (
                id = 1, amount_msat = 60, cltv = 20
            );
            send relay, eBeginAttempt;
        }

        on ePartResolved do {
        }
    }
}

machine TestTrampolineRetry {
    var relay: TrampolineRelay;
    var downstream: DownstreamPayment;
    var recipient: TrampolineRecipient;

    start state Init {
        entry {
            recipient = new TrampolineRecipient();
            downstream = new DownstreamPayment((
                recipient = recipient,
                mode = DownstreamRetryThenSuccess
            ));
            relay = new TrampolineRelay((
                controller = this,
                downstream = downstream,
                config = TestConfig()
            ));
            send relay, eAddPart, (
                id = 1, amount_msat = 100, cltv = 20
            );
            send relay, eBeginAttempt;
        }

        on ePartResolved do {
        }
    }
}

machine TestTrampolineFailure {
    var relay: TrampolineRelay;
    var downstream: DownstreamPayment;
    var recipient: TrampolineRecipient;

    start state Init {
        entry {
            recipient = new TrampolineRecipient();
            downstream = new DownstreamPayment((
                recipient = recipient,
                mode = DownstreamFailure
            ));
            relay = new TrampolineRelay((
                controller = this,
                downstream = downstream,
                config = TestConfig()
            ));
            send relay, eAddPart, (
                id = 1, amount_msat = 100, cltv = 20
            );
            send relay, eBeginAttempt;
        }

        on ePartResolved do {
        }
    }
}

machine TestTrampolineCancel {
    var relay: TrampolineRelay;
    var downstream: DownstreamPayment;
    var recipient: TrampolineRecipient;

    start state Init {
        entry {
            recipient = new TrampolineRecipient();
            downstream = new DownstreamPayment((
                recipient = recipient,
                mode = DownstreamPending
            ));
            relay = new TrampolineRelay((
                controller = this,
                downstream = downstream,
                config = TestConfig()
            ));
            send relay, eAddPart, (
                id = 1, amount_msat = 100, cltv = 20
            );
            send relay, eBeginAttempt;
            send relay, eCancel;
        }

        on ePartResolved do {
        }
    }
}

machine TestTrampolineRestartTimeout {
    var relay: TrampolineRelay;
    var downstream: DownstreamPayment;
    var recipient: TrampolineRecipient;

    start state Init {
        entry {
            recipient = new TrampolineRecipient();
            downstream = new DownstreamPayment((
                recipient = recipient,
                mode = DownstreamPending
            ));
            relay = new TrampolineRelay((
                controller = this,
                downstream = downstream,
                config = TestConfig()
            ));
            send relay, eAddPart, (
                id = 1, amount_msat = 100, cltv = 20
            );
            send relay, eBeginAttempt;
            send relay, eRestart;
            send relay, eObserveHeight, 6;
        }

        on ePartResolved do {
        }
    }
}

machine TestTrampolineCancelSettles {
    var relay: TrampolineRelay;
    var downstream: DownstreamPayment;
    var recipient: TrampolineRecipient;

    start state Init {
        entry {
            recipient = new TrampolineRecipient();
            downstream = new DownstreamPayment((
                recipient = recipient,
                mode = DownstreamCancelSettles
            ));
            relay = new TrampolineRelay((
                controller = this,
                downstream = downstream,
                config = TestConfig()
            ));
            send relay, eAddPart, (
                id = 1, amount_msat = 100, cltv = 20
            );
            send relay, eBeginAttempt;
            send relay, eObserveHeight, 40;
        }

        on ePartResolved do {
        }
    }
}

machine TestTrampolineReorgTimeout {
    var relay: TrampolineRelay;
    var downstream: DownstreamPayment;
    var recipient: TrampolineRecipient;
    var chain: ChainEnvironment;

    start state Init {
        entry {
            recipient = new TrampolineRecipient();
            downstream = new DownstreamPayment((
                recipient = recipient,
                mode = DownstreamPending
            ));
            relay = new TrampolineRelay((
                controller = this,
                downstream = downstream,
                config = TestConfig()
            ));
            send relay, eAddPart, (
                id = 1, amount_msat = 100, cltv = 20
            );
            chain = new ChainEnvironment((
                relay = relay, height = 0
            ));
            send chain, eMineBlocks, 10;
            send chain, eChainReorg, 6;
            send chain, eMineBlocks, 36;
        }

        on ePartResolved do {
        }
    }
}

machine TestTrampolineSenderFlow {
    var sender: TrampolineSender;
    var downstream: DownstreamPayment;
    var recipient: TrampolineRecipient;

    start state Init {
        entry {
            recipient = new TrampolineRecipient();
            downstream = new DownstreamPayment((
                recipient = recipient,
                mode = DownstreamSuccess
            ));
            sender = new TrampolineSender((
                downstream = downstream,
                config = TestConfig(),
                parts = TestParts()
            ));
        }
    }
}

machine TestTrampolineReadyReplay {
    var relay: TrampolineRelay;
    var downstream: DownstreamPayment;
    var recipient: TrampolineRecipient;

    start state Init {
        entry {
            recipient = new TrampolineRecipient();
            downstream = new DownstreamPayment((
                recipient = recipient,
                mode = DownstreamSuccess
            ));
            relay = new TrampolineRelay((
                controller = this,
                downstream = downstream,
                config = TestConfig()
            ));
            send relay, eAddPart, (
                id = 1, amount_msat = 100, cltv = 20
            );
            send relay, eNotifyReady;
            send relay, eNotifyReady;
        }

        on ePartResolved do {
        }
    }
}

test tcTrampolineHappy [main=TestTrampolineHappy]:
    { TrampolineRelay, DownstreamPayment, TrampolineRecipient,
      TestTrampolineHappy };

test tcTrampolineRetry [main=TestTrampolineRetry]:
    { TrampolineRelay, DownstreamPayment, TrampolineRecipient,
      TestTrampolineRetry };

test tcTrampolineFailure [main=TestTrampolineFailure]:
    { TrampolineRelay, DownstreamPayment, TrampolineRecipient,
      TestTrampolineFailure };

test tcTrampolineCancelPending [main=TestTrampolineCancel]:
    { TrampolineRelay, DownstreamPayment, TrampolineRecipient,
      TestTrampolineCancel };

test tcTrampolineRestartTimeout [main=TestTrampolineRestartTimeout]:
    { TrampolineRelay, DownstreamPayment, TrampolineRecipient,
      TestTrampolineRestartTimeout };

test tcTrampolineCancelSettles [main=TestTrampolineCancelSettles]:
    { TrampolineRelay, DownstreamPayment, TrampolineRecipient,
      TestTrampolineCancelSettles };

test tcTrampolineReorgTimeout [main=TestTrampolineReorgTimeout]:
    { TrampolineRelay, DownstreamPayment, TrampolineRecipient,
      ChainEnvironment, TestTrampolineReorgTimeout };

test tcTrampolineSenderFlow [main=TestTrampolineSenderFlow]:
    { TrampolineRelay, TrampolineSender, DownstreamPayment,
      TrampolineRecipient, TestTrampolineSenderFlow };

test tcTrampolineReadyReplay [main=TestTrampolineReadyReplay]:
    { TrampolineRelay, DownstreamPayment, TrampolineRecipient,
      TestTrampolineReadyReplay };
