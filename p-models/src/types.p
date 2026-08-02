// Shared types for the trampoline routing lifecycle model.

enum TrampolineRelayState {
    Collecting,
    Ready,
    Paying,
    Recovering,
    Canceling,
    Succeeded,
    Failed,
    Canceled,
    TimedOut
}

enum TrampolineResolution {
    Unresolved,
    Fulfilled,
    Rejected
}

enum DownstreamMode {
    DownstreamSuccess,
    DownstreamRetryThenSuccess,
    DownstreamFailure,
    DownstreamPending,
    DownstreamCancelSettles
}

type TrampolinePart = (
    id: int,
    amount_msat: int,
    cltv: int
);

type TrampolineConfig = (
    expected_total_msat: int,
    downstream_msat: int,
    required_cltv: int,
    max_parts: int,
    max_attempts: int,
    hold_deadline: int,
    recovery_window: int,
    expiry_safety_blocks: int,
    max_reorg_depth: int
);

type RelayInit = (
    controller: machine,
    downstream: machine,
    config: TrampolineConfig
);

type DownstreamInit = (
    recipient: machine,
    mode: DownstreamMode
);

type SenderInit = (
    downstream: machine,
    config: TrampolineConfig,
    parts: seq[TrampolinePart]
);

type ChainInit = (
    relay: machine,
    height: int
);

type DownstreamRequest = (
    relay: machine,
    attempt: int,
    amount_msat: int,
    cltv: int
);

type ReconcileRequest = (
    relay: machine,
    attempt: int
);

type PartResolutionEvent = (
    part_id: int,
    amount_msat: int,
    resolution: TrampolineResolution
);

type RelayStateEvent = (
    status: TrampolineRelayState,
    accepted_parts: int,
    collected_msat: int,
    attempts: int,
    height: int,
    hold_deadline: int,
    recovery_deadline: int,
    max_attempts: int
);

type ForwardEvent = (
    attempt: int,
    collected_msat: int,
    downstream_msat: int,
    incoming_cltv: int,
    required_cltv: int,
    max_attempts: int
);

type ReplayEvent = (
    part_id: int,
    mutated: bool
);

type DispatchEvent = (
    epoch: int,
    attempt: int
);

event eAddPart: TrampolinePart;
event eNotifyReady;
event eBeginAttempt;
event eAttemptSuccess: int;
event eAttemptFailure: (attempt: int, retryable: bool);
event eCancel;
event eObserveHeight: int;
event eRestart;
event eReconcileSuccess: int;
event eReconcileFailure: int;
event eReconcilePending: int;
event eMineBlocks: int;
event eChainReorg: int;

event eDownstreamPay: DownstreamRequest;
event eDownstreamReconcile: ReconcileRequest;
event eDownstreamCancel: ReconcileRequest;
event eRecipientSettled: int;
event ePartResolved: PartResolutionEvent;

event eRelayState: RelayStateEvent;
event ePartAccepted: TrampolinePart;
event ePartReplay: ReplayEvent;
event eForwardStarted: ForwardEvent;
event eDispatchScheduled: DispatchEvent;
event eRelayTerminal: TrampolineRelayState;
event eRecipientSettlement: int;
