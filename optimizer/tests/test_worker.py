from __future__ import annotations

import io
import json
import threading

import pytest

from ftw_optimizer import worker
from ftw_optimizer.deadline import SolveDeadline


@pytest.fixture(autouse=True)
def reset_active_requests(monkeypatch) -> None:
    monkeypatch.setattr(worker, "ACTIVE_REQUESTS", worker._ActiveRequests())


def test_health_stays_responsive_without_cleaning_memory_during_solve(
    monkeypatch,
) -> None:
    solve_started = threading.Event()
    finish_solve = threading.Event()
    cleanup_calls: list[bool] = []
    thread_errors: list[BaseException] = []
    monkeypatch.setattr(worker, "SOLVE_LOCK", threading.Lock())

    def fake_handle(_raw: object, **_kwargs: object) -> dict[str, object]:
        solve_started.set()
        if not finish_solve.wait(timeout=2):
            raise TimeoutError("test did not release solve")
        return {"ok": True}

    def fake_cleanup() -> None:
        cleanup_calls.append(worker.SOLVE_LOCK.locked())

    monkeypatch.setattr(worker, "handle", fake_handle)
    monkeypatch.setattr(worker, "release_unused_memory", fake_cleanup)

    solve_output = io.StringIO()

    def run_solve() -> None:
        try:
            worker.process_stream(
                io.StringIO(
                    '{"schema_version":1,"request_id":"test","slots":[{}]}\n'
                ),
                solve_output,
            )
        except BaseException as exc:
            thread_errors.append(exc)

    solve_thread = threading.Thread(target=run_solve)
    solve_thread.start()
    assert solve_started.wait(timeout=1)

    health_output = io.StringIO()
    health_thread = threading.Thread(
        target=worker.process_stream,
        args=(
            io.StringIO('{"type":"handshake","protocol_version":1}\n'),
            health_output,
        ),
    )
    health_thread.start()
    health_thread.join(timeout=1)
    assert not health_thread.is_alive()
    health = json.loads(health_output.getvalue())
    assert health["name"] == "ftw-optimizer"
    assert "cancel_request" in health["features"]
    assert cleanup_calls == []

    finish_solve.set()
    solve_thread.join(timeout=1)
    assert not solve_thread.is_alive()
    assert thread_errors == []
    assert cleanup_calls == [True]


class FakeClock:
    def __init__(self) -> None:
        self.value = 0.0
        self.condition = threading.Condition()

    def __call__(self) -> float:
        with self.condition:
            return self.value

    def advance(self, seconds: float) -> None:
        with self.condition:
            self.value += seconds
            self.condition.notify_all()


class FakeSolveLock:
    def __init__(self, clock: FakeClock) -> None:
        self.clock = clock
        self.held = False
        self.queued = threading.Event()

    def acquire(self, blocking: bool = True, timeout: float = -1) -> bool:
        with self.clock.condition:
            if not self.held:
                self.held = True
                return True
            if not blocking:
                return False
            self.queued.set()
            expires_at = self.clock.value + timeout
            while self.held:
                if timeout >= 0 and self.clock.value >= expires_at:
                    return False
                self.clock.condition.wait()
            self.held = True
            return True

    def acquire_until(self, deadline: SolveDeadline) -> bool:
        with self.clock.condition:
            while self.held:
                self.queued.set()
                deadline.check("optimizer queue")
                self.clock.condition.wait()
            deadline.check("optimizer queue")
            self.held = True
            return True

    def release(self) -> None:
        with self.clock.condition:
            self.held = False
            self.clock.condition.notify_all()

    def locked(self) -> bool:
        with self.clock.condition:
            return self.held

    def notify_waiters(self) -> None:
        with self.clock.condition:
            self.clock.condition.notify_all()


def test_expired_request_leaves_solve_queue_without_running(
    monkeypatch,
) -> None:
    clock = FakeClock()
    solve_lock = FakeSolveLock(clock)
    first_started = threading.Event()
    finish_first = threading.Event()
    solve_calls: list[str] = []
    thread_errors: list[BaseException] = []
    monkeypatch.setattr(worker, "SOLVE_LOCK", solve_lock)
    monkeypatch.setattr(worker, "release_unused_memory", lambda: None)

    def fake_solve(payload: dict, **_kwargs: object) -> dict[str, object]:
        request_id = str(payload["request_id"])
        solve_calls.append(request_id)
        if request_id == "first":
            first_started.set()
            if not finish_first.wait(timeout=2):
                raise TimeoutError("test did not release first solve")
        return {"ok": True, "request_id": request_id}

    monkeypatch.setattr(worker, "solve", fake_solve)

    def request(request_id: str, budget_s: float) -> io.StringIO:
        return io.StringIO(
            json.dumps(
                {
                    "schema_version": 1,
                    "request_id": request_id,
                    "settings": {"time_limit_s": budget_s},
                    "slots": [{}],
                }
            )
            + "\n"
        )

    def run(request_id: str, budget_s: float, output: io.StringIO) -> None:
        try:
            worker.process_stream(
                request(request_id, budget_s),
                output,
                clock=clock,
            )
        except BaseException as exc:
            thread_errors.append(exc)

    first_output = io.StringIO()
    first = threading.Thread(target=run, args=("first", 10.0, first_output))
    first.start()
    assert first_started.wait(timeout=1)

    expired_output = io.StringIO()
    expired = threading.Thread(target=run, args=("expired", 1.0, expired_output))
    expired.start()
    assert solve_lock.queued.wait(timeout=1)
    clock.advance(2.0)
    expired.join(timeout=1)
    assert not expired.is_alive()
    assert solve_lock.locked()
    assert json.loads(expired_output.getvalue())["error"]["code"] == "deadline_exceeded"
    assert solve_calls == ["first"]

    finish_first.set()
    first.join(timeout=1)
    assert not first.is_alive()

    fresh_output = io.StringIO()
    fresh = threading.Thread(target=run, args=("fresh", 1.0, fresh_output))
    fresh.start()
    fresh.join(timeout=1)
    assert not fresh.is_alive()
    assert json.loads(fresh_output.getvalue())["ok"] is True
    assert solve_calls == ["first", "fresh"]
    assert thread_errors == []


def test_handle_rejects_a_result_that_finishes_after_its_deadline(
    monkeypatch,
) -> None:
    clock = FakeClock()
    solve_calls = 0

    def fake_solve(_payload: dict, **_kwargs: object) -> dict[str, object]:
        nonlocal solve_calls
        solve_calls += 1
        clock.advance(2.0)
        return {"ok": True}

    monkeypatch.setattr(worker, "solve", fake_solve)
    response = worker.handle(
        {
            "schema_version": 1,
            "request_id": "late",
            "settings": {"time_limit_s": 1.0},
            "slots": [{}],
        },
        clock=clock,
    )

    assert solve_calls == 1
    assert response["error"]["code"] == "deadline_exceeded"


def request_stream(request_id: str, budget_s: float = 10.0) -> io.StringIO:
    return io.StringIO(
        json.dumps(
            {
                "schema_version": 1,
                "request_id": request_id,
                "settings": {"time_limit_s": budget_s},
                "slots": [{}],
            }
        )
        + "\n"
    )


def cancel_stream(request_id: str) -> io.StringIO:
    return io.StringIO(
        json.dumps(
            {
                "type": "cancel_request",
                "protocol_version": 1,
                "request_id": request_id,
            }
        )
        + "\n"
    )


def test_active_cancel_releases_the_next_request_before_the_old_deadline(
    monkeypatch,
) -> None:
    clock = FakeClock()
    solve_lock = FakeSolveLock(clock)
    first_started = threading.Event()
    let_cancelled_solve_check_token = threading.Event()
    second_started = threading.Event()
    solve_calls: list[str] = []
    thread_errors: list[BaseException] = []
    monkeypatch.setattr(worker, "SOLVE_LOCK", solve_lock)
    monkeypatch.setattr(worker, "release_unused_memory", lambda: None)

    def fake_solve(
        payload: dict,
        *,
        deadline: SolveDeadline,
    ) -> dict[str, object]:
        request_id = str(payload["request_id"])
        solve_calls.append(request_id)
        if request_id == "first":
            first_started.set()
            if not let_cancelled_solve_check_token.wait(timeout=1):
                raise TimeoutError("test did not finish cancellation")
            deadline.check("fake active solve")
        else:
            assert clock() == 0.0
            second_started.set()
        return {"ok": True, "request_id": request_id}

    monkeypatch.setattr(worker, "solve", fake_solve)

    def run(request_id: str, output: io.StringIO) -> None:
        try:
            worker.process_stream(request_stream(request_id), output, clock=clock)
        except BaseException as exc:
            thread_errors.append(exc)

    first_output = io.StringIO()
    first = threading.Thread(target=run, args=("first", first_output))
    first.start()
    assert first_started.wait(timeout=1)

    second_output = io.StringIO()
    second = threading.Thread(target=run, args=("second", second_output))
    second.start()
    assert solve_lock.queued.wait(timeout=1)

    cancel_output = io.StringIO()
    worker.process_stream(cancel_stream("first"), cancel_output, clock=clock)
    let_cancelled_solve_check_token.set()

    first.join(timeout=1)
    second.join(timeout=1)
    assert not first.is_alive()
    assert not second.is_alive()
    assert second_started.is_set()
    assert first_output.getvalue() == ""
    assert json.loads(second_output.getvalue())["request_id"] == "second"
    assert json.loads(cancel_output.getvalue())["active"] is True
    assert solve_calls == ["first", "second"]
    assert clock() == 0.0
    assert thread_errors == []


def test_queued_cancel_leaves_without_waiting_for_the_active_request(
    monkeypatch,
) -> None:
    clock = FakeClock()
    solve_lock = FakeSolveLock(clock)
    active_started = threading.Event()
    finish_active = threading.Event()
    solve_calls: list[str] = []
    thread_errors: list[BaseException] = []
    monkeypatch.setattr(worker, "SOLVE_LOCK", solve_lock)
    monkeypatch.setattr(worker, "release_unused_memory", lambda: None)

    def fake_solve(payload: dict, **_kwargs: object) -> dict[str, object]:
        request_id = str(payload["request_id"])
        solve_calls.append(request_id)
        if request_id == "active":
            active_started.set()
            if not finish_active.wait(timeout=1):
                raise TimeoutError("test did not release active request")
        return {"ok": True, "request_id": request_id}

    monkeypatch.setattr(worker, "solve", fake_solve)

    def run(request_id: str, output: io.StringIO) -> None:
        try:
            worker.process_stream(request_stream(request_id), output, clock=clock)
        except BaseException as exc:
            thread_errors.append(exc)

    active_output = io.StringIO()
    active = threading.Thread(target=run, args=("active", active_output))
    active.start()
    assert active_started.wait(timeout=1)

    queued_output = io.StringIO()
    queued = threading.Thread(target=run, args=("queued", queued_output))
    queued.start()
    assert solve_lock.queued.wait(timeout=1)

    cancel_output = io.StringIO()
    worker.process_stream(cancel_stream("queued"), cancel_output, clock=clock)
    queued.join(timeout=1)

    assert not queued.is_alive()
    assert active.is_alive()
    assert queued_output.getvalue() == ""
    assert json.loads(cancel_output.getvalue())["active"] is True
    assert solve_calls == ["active"]

    finish_active.set()
    active.join(timeout=1)
    assert not active.is_alive()
    assert json.loads(active_output.getvalue())["request_id"] == "active"
    assert thread_errors == []


def test_early_cancel_prevents_the_request_from_entering_the_solver(
    monkeypatch,
) -> None:
    solve_calls: list[str] = []
    monkeypatch.setattr(worker, "SOLVE_LOCK", _TestSolveLock())
    monkeypatch.setattr(worker, "release_unused_memory", lambda: None)
    monkeypatch.setattr(
        worker,
        "solve",
        lambda payload, **_kwargs: solve_calls.append(str(payload["request_id"])),
    )

    cancel_output = io.StringIO()
    worker.process_stream(cancel_stream("early"), cancel_output)
    request_output = io.StringIO()
    worker.process_stream(request_stream("early"), request_output)

    assert json.loads(cancel_output.getvalue())["active"] is False
    assert request_output.getvalue() == ""
    assert solve_calls == []


def test_cancel_accepts_an_older_protocol_version_in_the_worker_window(
    monkeypatch,
) -> None:
    monkeypatch.setattr(worker, "PROTOCOL_VERSION", 2)
    output = io.StringIO()

    worker.process_stream(cancel_stream("future-worker"), output)

    response = json.loads(output.getvalue())
    assert response["ok"] is True
    assert response["protocol_version"] == 2


@pytest.mark.parametrize("protocol_version", [True, 0, 2, "1"])
def test_cancel_rejects_a_protocol_version_outside_the_worker_window(
    protocol_version: object,
) -> None:
    output = io.StringIO()
    request = cancel_stream("invalid-version")
    raw = json.loads(request.getvalue())
    raw["protocol_version"] = protocol_version

    worker.process_stream(io.StringIO(json.dumps(raw) + "\n"), output)

    assert json.loads(output.getvalue())["error"]["code"] == "invalid_request"


def test_wrong_request_id_does_not_cancel_the_active_request(monkeypatch) -> None:
    active_started = threading.Event()
    finish_active = threading.Event()
    active_deadline: list[SolveDeadline] = []
    thread_errors: list[BaseException] = []
    monkeypatch.setattr(worker, "SOLVE_LOCK", _TestSolveLock())
    monkeypatch.setattr(worker, "release_unused_memory", lambda: None)

    def fake_solve(
        payload: dict,
        *,
        deadline: SolveDeadline,
    ) -> dict[str, object]:
        active_deadline.append(deadline)
        active_started.set()
        if not finish_active.wait(timeout=1):
            raise TimeoutError("test did not release active request")
        deadline.check("fake active solve")
        return {"ok": True, "request_id": str(payload["request_id"])}

    monkeypatch.setattr(worker, "solve", fake_solve)
    output = io.StringIO()

    def run() -> None:
        try:
            worker.process_stream(request_stream("active"), output)
        except BaseException as exc:
            thread_errors.append(exc)

    thread = threading.Thread(target=run)
    thread.start()
    assert active_started.wait(timeout=1)

    cancel_output = io.StringIO()
    worker.process_stream(cancel_stream("different"), cancel_output)
    assert json.loads(cancel_output.getvalue())["active"] is False
    assert not active_deadline[0].is_cancelled()

    finish_active.set()
    thread.join(timeout=1)
    assert not thread.is_alive()
    assert json.loads(output.getvalue())["ok"] is True
    assert thread_errors == []


def test_pending_cancel_registry_has_a_fixed_bound() -> None:
    registry = worker._ActiveRequests(max_pending_cancels=2)
    registry.cancel("evicted")
    registry.cancel("kept-one")
    registry.cancel("kept-two")
    evicted = SolveDeadline(1.0, FakeClock())
    kept_one = SolveDeadline(1.0, FakeClock())
    kept_two = SolveDeadline(1.0, FakeClock())

    registry.register("evicted", evicted)
    registry.register("kept-one", kept_one)
    registry.register("kept-two", kept_two)

    assert not evicted.is_cancelled()
    assert kept_one.is_cancelled()
    assert kept_two.is_cancelled()


class _TestSolveLock:
    def __init__(self) -> None:
        self.lock = threading.Lock()

    def acquire_until(self, deadline: SolveDeadline) -> bool:
        deadline.check("optimizer queue")
        return self.lock.acquire()

    def release(self) -> None:
        self.lock.release()

    def locked(self) -> bool:
        return self.lock.locked()

    def notify_waiters(self) -> None:
        return None


class _FailingCancelHighs:
    def cancelSolve(self) -> None:
        raise RuntimeError("cancel failed")


def test_cancel_frame_survives_a_highs_cancel_failure(monkeypatch, capsys) -> None:
    monkeypatch.setattr(worker, "SOLVE_LOCK", _TestSolveLock())
    deadline = SolveDeadline(1.0, FakeClock())
    highs = _FailingCancelHighs()
    deadline.attach_highs(highs)
    worker.ACTIVE_REQUESTS.register("failing", deadline)
    output = io.StringIO()

    worker.process_stream(cancel_stream("failing"), output)

    assert json.loads(output.getvalue())["active"] is True
    assert deadline.is_cancelled()
    assert "cancel failed" in capsys.readouterr().err
    deadline.detach_highs(highs)
    worker.ACTIVE_REQUESTS.unregister("failing", deadline)
