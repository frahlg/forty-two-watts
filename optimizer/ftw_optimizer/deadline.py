from __future__ import annotations

import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from .protocol import positive_number, require_dict


class SolveDeadlineExceeded(RuntimeError):
    """The request's one worker-side time budget has been spent."""


class SolveCancelled(SolveDeadlineExceeded):
    """The caller cancelled the request before it could publish a result."""


@dataclass
class _CancellationState:
    cancelled: threading.Event = field(default_factory=threading.Event)
    lock: threading.Lock = field(default_factory=threading.Lock)
    active_highs: Any | None = None


@dataclass(frozen=True)
class SolveDeadline:
    expires_at: float
    clock: Callable[[], float] = field(
        default=time.perf_counter,
        repr=False,
        compare=False,
    )
    _cancellation: _CancellationState = field(
        default_factory=_CancellationState,
        repr=False,
        compare=False,
    )

    @classmethod
    def from_payload(
        cls,
        payload: dict[str, Any],
        *,
        started_at: float | None = None,
        clock: Callable[[], float] = time.perf_counter,
    ) -> SolveDeadline:
        settings = require_dict(payload.get("settings", {}), "settings")
        budget_s = positive_number(
            settings.get("time_limit_s", 2.0),
            "settings.time_limit_s",
        )
        if started_at is None:
            started_at = clock()
        return cls(started_at + budget_s, clock)

    def remaining_s(self, phase: str = "optimizer request") -> float:
        if self.is_cancelled():
            raise SolveCancelled(f"{phase} was cancelled")
        remaining = self.expires_at - self.clock()
        if remaining <= 0.0:
            raise SolveDeadlineExceeded(f"{phase} deadline exceeded")
        return remaining

    def check(self, phase: str = "optimizer request") -> None:
        self.remaining_s(phase)

    def cancel(self) -> None:
        self._cancellation.cancelled.set()
        with self._cancellation.lock:
            highs = self._cancellation.active_highs
        if highs is not None:
            highs.cancelSolve()

    def is_cancelled(self) -> bool:
        return self._cancellation.cancelled.is_set()

    def attach_highs(self, highs: Any) -> None:
        with self._cancellation.lock:
            if self._cancellation.active_highs is not None:
                raise RuntimeError("a HiGHS solve is already attached")
            self._cancellation.active_highs = highs

    def detach_highs(self, highs: Any) -> None:
        with self._cancellation.lock:
            if self._cancellation.active_highs is highs:
                self._cancellation.active_highs = None
