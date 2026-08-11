from __future__ import annotations

import threading

import cvxpy as cp
import highspy
import pytest

from ftw_optimizer import shared_highs
from ftw_optimizer.deadline import (
    SolveCancelled,
    SolveDeadline,
    SolveDeadlineExceeded,
)
from ftw_optimizer.direct_highs import (
    DirectHighsError,
    _remaining_time_s,
    _run_optimal,
)
from ftw_optimizer.model import _solver_options, solve


class FakeClock:
    def __init__(self, now: float = 0.0) -> None:
        self.now = now

    def __call__(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds


def test_one_deadline_shrinks_across_cvxpy_and_direct_highs_phases() -> None:
    clock = FakeClock(10.0)
    deadline = SolveDeadline.from_payload(
        {"settings": {"time_limit_s": 1.0}},
        started_at=clock(),
        clock=clock,
    )
    settings = {"time_limit_s": 1.0}

    assert _solver_options(settings, cp.HIGHS, deadline)["time_limit"] == pytest.approx(1.0)
    clock.advance(0.6)
    assert _solver_options(settings, cp.CLARABEL, deadline)["time_limit"] == pytest.approx(0.4)
    assert _remaining_time_s(deadline) == pytest.approx(0.4)

    # The old per-solve 50 ms floor must not extend the request deadline.
    clock.advance(0.39)
    assert _solver_options(settings, cp.HIGHS, deadline)["time_limit"] == pytest.approx(0.01)
    clock.advance(0.02)
    with pytest.raises(SolveDeadlineExceeded, match="deadline exceeded"):
        _solver_options(settings, cp.HIGHS, deadline)
    with pytest.raises(SolveDeadlineExceeded, match="deadline exceeded"):
        _remaining_time_s(deadline)


def test_deadline_error_bypasses_shared_backend_fallback(monkeypatch) -> None:
    deadline = SolveDeadline(1.0, FakeClock())
    direct_calls: list[SolveDeadline] = []

    def fail_direct(
        _payload: dict,
        _started: float,
        received_deadline: SolveDeadline,
    ) -> dict:
        direct_calls.append(received_deadline)
        raise SolveDeadlineExceeded("direct HiGHS solve deadline exceeded")

    monkeypatch.setattr(shared_highs, "solve_shared_highs", fail_direct)

    with pytest.raises(SolveDeadlineExceeded, match="deadline exceeded"):
        solve(
            {
                "settings": {
                    "shared_backend": "auto",
                    "time_limit_s": 10.0,
                },
                "commercial_constraints": {},
                "slots": [{}],
                "storages": [],
            },
            deadline=deadline,
        )

    assert direct_calls == [deadline]


class FakeHighs:
    def __init__(
        self,
        status: highspy.HighsModelStatus,
        run_status: highspy.HighsStatus = highspy.HighsStatus.kOk,
    ) -> None:
        self.status = status
        self.run_status = run_status
        self.HandleUserInterrupt = False

    def run(self) -> highspy.HighsStatus:
        return self.run_status

    def startSolve(self) -> object:
        return object()

    def joinSolve(self, _solver_thread: object) -> highspy.HighsStatus:
        return self.run_status

    def getModelStatus(self) -> highspy.HighsModelStatus:
        return self.status


def test_direct_highs_time_limit_is_a_deadline_not_a_fallback_error() -> None:
    deadline = SolveDeadline(1.0, FakeClock())

    with pytest.raises(SolveDeadlineExceeded, match="service solve deadline exceeded"):
        _run_optimal(
            FakeHighs(
                highspy.HighsModelStatus.kTimeLimit,
                highspy.HighsStatus.kWarning,
            ),
            "service",
            deadline,
        )

    with pytest.raises(DirectHighsError, match="failed with status"):
        _run_optimal(
            FakeHighs(highspy.HighsModelStatus.kInfeasible),
            "service",
            deadline,
        )


class BlockingHighs:
    def __init__(self) -> None:
        self.HandleUserInterrupt = False
        self.start_entered = threading.Event()
        self.allow_start = threading.Event()
        self.cancelled = threading.Event()
        self.cancel_calls = 0

    def startSolve(self) -> object:
        self.start_entered.set()
        if not self.allow_start.wait(timeout=1):
            raise TimeoutError("test did not allow HiGHS to start")
        # HiGHS clears its stop flag in startSolve.
        self.cancelled.clear()
        return object()

    def cancelSolve(self) -> None:
        self.cancel_calls += 1
        self.cancelled.set()

    def joinSolve(self, _solver_thread: object) -> highspy.HighsStatus:
        if not self.cancelled.wait(timeout=1):
            raise TimeoutError("test cancellation did not reach HiGHS")
        return highspy.HighsStatus.kWarning

    def getModelStatus(self) -> highspy.HighsModelStatus:
        return highspy.HighsModelStatus.kInterrupt


class CountingInterruptHighs(FakeHighs):
    def __init__(self) -> None:
        self._handle_user_interrupt = False
        self.interrupt_enable_calls = 0
        super().__init__(highspy.HighsModelStatus.kOptimal)

    @property
    def HandleUserInterrupt(self) -> bool:
        return self._handle_user_interrupt

    @HandleUserInterrupt.setter
    def HandleUserInterrupt(self, enabled: bool) -> None:
        self._handle_user_interrupt = enabled
        if enabled:
            self.interrupt_enable_calls += 1


class RunningHighs:
    def __init__(self) -> None:
        self.HandleUserInterrupt = False
        self.join_entered = threading.Event()
        self.cancelled = threading.Event()
        self.cancel_calls = 0

    def startSolve(self) -> object:
        return object()

    def cancelSolve(self) -> None:
        self.cancel_calls += 1
        self.cancelled.set()

    def joinSolve(self, _solver_thread: object) -> highspy.HighsStatus:
        self.join_entered.set()
        if not self.cancelled.wait(timeout=1):
            raise TimeoutError("test cancellation did not reach HiGHS")
        return highspy.HighsStatus.kWarning

    def getModelStatus(self) -> highspy.HighsModelStatus:
        return highspy.HighsModelStatus.kHighsInterrupt


class RaisingOnCancelHighs(RunningHighs):
    def joinSolve(self, _solver_thread: object) -> highspy.HighsStatus:
        self.join_entered.set()
        if not self.cancelled.wait(timeout=1):
            raise TimeoutError("test cancellation did not reach HiGHS")
        raise RuntimeError("HiGHS join failed during cancellation")


class RaisingAfterDeadlineHighs(FakeHighs):
    def __init__(self, clock: FakeClock) -> None:
        super().__init__(highspy.HighsModelStatus.kSolveError)
        self.clock = clock

    def joinSolve(self, _solver_thread: object) -> highspy.HighsStatus:
        self.clock.advance(2.0)
        raise RuntimeError("HiGHS join failed after the deadline")


def test_direct_highs_enables_interrupt_callbacks_once_per_model() -> None:
    deadline = SolveDeadline(1.0, FakeClock())
    highs = CountingInterruptHighs()

    _run_optimal(highs, "service", deadline)
    _run_optimal(highs, "economic", deadline)

    assert highs.interrupt_enable_calls == 1


def test_cancel_interrupts_an_active_direct_highs_solve() -> None:
    deadline = SolveDeadline(1.0, FakeClock())
    highs = RunningHighs()
    errors: list[BaseException] = []

    def run() -> None:
        try:
            _run_optimal(highs, "service", deadline)
        except BaseException as exc:
            errors.append(exc)

    thread = threading.Thread(target=run)
    thread.start()
    assert highs.join_entered.wait(timeout=1)

    deadline.cancel()
    thread.join(timeout=1)

    assert not thread.is_alive()
    assert len(errors) == 1
    assert isinstance(errors[0], SolveCancelled)
    assert highs.cancel_calls == 1


def test_cancellation_wins_when_highs_join_raises() -> None:
    deadline = SolveDeadline(1.0, FakeClock())
    highs = RaisingOnCancelHighs()
    errors: list[BaseException] = []

    def run() -> None:
        try:
            _run_optimal(highs, "service", deadline)
        except BaseException as exc:
            errors.append(exc)

    thread = threading.Thread(target=run)
    thread.start()
    assert highs.join_entered.wait(timeout=1)

    deadline.cancel()
    thread.join(timeout=1)

    assert not thread.is_alive()
    assert len(errors) == 1
    assert isinstance(errors[0], SolveCancelled)
    assert isinstance(errors[0].__context__, RuntimeError)


def test_deadline_wins_when_highs_join_raises_after_expiry() -> None:
    clock = FakeClock()
    deadline = SolveDeadline(1.0, clock)

    with pytest.raises(SolveDeadlineExceeded, match="deadline exceeded"):
        _run_optimal(RaisingAfterDeadlineHighs(clock), "service", deadline)


def test_direct_highs_repeats_cancel_after_start_resets_the_stop_flag() -> None:
    deadline = SolveDeadline(1.0, FakeClock())
    highs = BlockingHighs()
    errors: list[BaseException] = []
    phase_two_started = threading.Event()

    def run() -> None:
        try:
            _run_optimal(highs, "service", deadline)
            phase_two_started.set()
        except BaseException as exc:
            errors.append(exc)

    thread = threading.Thread(target=run)
    thread.start()
    assert highs.start_entered.wait(timeout=1)

    deadline.cancel()
    highs.allow_start.set()
    thread.join(timeout=1)

    assert not thread.is_alive()
    assert len(errors) == 1
    assert isinstance(errors[0], SolveCancelled)
    assert highs.cancel_calls == 2
    assert not phase_two_started.is_set()


def test_cancelled_direct_highs_does_not_fall_back_to_cvxpy(monkeypatch) -> None:
    deadline = SolveDeadline(1.0, FakeClock())
    direct_calls: list[SolveDeadline] = []

    def cancel_direct(
        _payload: dict,
        _started: float,
        received_deadline: SolveDeadline,
    ) -> dict:
        direct_calls.append(received_deadline)
        raise SolveCancelled("direct HiGHS solve was cancelled")

    monkeypatch.setattr(shared_highs, "solve_shared_highs", cancel_direct)

    with pytest.raises(SolveCancelled, match="cancelled"):
        solve(
            {
                "settings": {
                    "shared_backend": "auto",
                    "time_limit_s": 10.0,
                },
                "commercial_constraints": {},
                "slots": [{}],
                "storages": [],
            },
            deadline=deadline,
        )

    assert direct_calls == [deadline]
