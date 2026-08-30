from __future__ import annotations

import argparse
import csv
import json
import math
import sys
import time
import urllib.parse
import urllib.request
import uuid
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any, Iterable

from .worker import handle


DATASET_SCHEMA_VERSION = 1


class SnapshotSkip(ValueError):
    pass


def _get_json(url: str, timeout_s: float) -> dict[str, Any]:
    request = urllib.request.Request(
        url,
        headers={"Accept": "application/json", "User-Agent": "ftw-optimizer-backtest/1"},
        method="GET",
    )
    with urllib.request.urlopen(request, timeout=timeout_s) as response:
        payload = json.load(response)
    if not isinstance(payload, dict):
        raise RuntimeError(f"{url} returned a non-object JSON payload")
    return payload


def _evenly_spaced(items: list[dict[str, Any]], count: int) -> list[dict[str, Any]]:
    if count <= 0 or not items:
        return []
    if count >= len(items):
        return list(items)
    if count == 1:
        return [items[len(items) // 2]]
    indexes = {round(i * (len(items) - 1) / (count - 1)) for i in range(count)}
    return [items[i] for i in sorted(indexes)]


def select_summaries(
    summaries: Iterable[dict[str, Any]], sample_count: int, per_reason: int = 3
) -> list[dict[str, Any]]:
    ordered = sorted(summaries, key=lambda row: int(row["ts_ms"]))
    if sample_count <= 0 or sample_count >= len(ordered):
        return ordered

    by_reason: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for row in ordered:
        by_reason[str(row.get("reason", "unknown"))].append(row)

    selected: dict[int, dict[str, Any]] = {}
    for group in by_reason.values():
        for row in _evenly_spaced(group, min(per_reason, len(group))):
            selected[int(row["ts_ms"])] = row

    remaining = sample_count - len(selected)
    if remaining > 0:
        candidates = [row for row in ordered if int(row["ts_ms"]) not in selected]
        for row in _evenly_spaced(candidates, remaining):
            selected[int(row["ts_ms"])] = row

    if len(selected) < sample_count:
        for row in ordered:
            selected.setdefault(int(row["ts_ms"]), row)
            if len(selected) >= sample_count:
                break
    return sorted(selected.values(), key=lambda row: int(row["ts_ms"]))[:sample_count]


def export_dataset(
    api_base: str,
    output: Path,
    days: int,
    samples: int,
    timeout_s: float,
) -> dict[str, Any]:
    base = api_base.rstrip("/")
    until_ms = int(time.time() * 1000)
    since_ms = until_ms - days * 24 * 60 * 60 * 1000
    cursor = until_ms
    summaries_by_ts: dict[int, dict[str, Any]] = {}

    while cursor >= since_ms:
        query = urllib.parse.urlencode(
            {"since": since_ms, "until": cursor, "limit": 5000}
        )
        payload = _get_json(f"{base}/api/mpc/diagnose/history?{query}", timeout_s)
        page = payload.get("snapshots", [])
        if not isinstance(page, list):
            raise RuntimeError("diagnostic history response has no snapshots array")
        for row in page:
            if isinstance(row, dict) and int(row.get("ts_ms", 0)) > 0:
                summaries_by_ts[int(row["ts_ms"])] = row
        if len(page) < 5000:
            break
        oldest = min(int(row["ts_ms"]) for row in page if isinstance(row, dict))
        if oldest <= since_ms or oldest >= cursor:
            break
        cursor = oldest - 1

    selected = select_summaries(summaries_by_ts.values(), samples)
    metadata = {
        "type": "metadata",
        "schema_version": DATASET_SCHEMA_VERSION,
        "exported_at_ms": int(time.time() * 1000),
        "source": base,
        "since_ms": since_ms,
        "until_ms": until_ms,
        "index_count": len(summaries_by_ts),
        "sample_count": len(selected),
    }

    output.parent.mkdir(parents=True, exist_ok=True)
    temporary = output.with_suffix(output.suffix + ".tmp")
    with temporary.open("w", encoding="utf-8") as target:
        target.write(json.dumps(metadata, separators=(",", ":")) + "\n")
        for position, summary in enumerate(selected, start=1):
            query = urllib.parse.urlencode({"ts": int(summary["ts_ms"])})
            payload = _get_json(f"{base}/api/mpc/diagnose/at?{query}", timeout_s)
            snapshot = payload.get("snapshot")
            if not isinstance(snapshot, dict) or not isinstance(snapshot.get("diagnostic"), dict):
                continue
            record = {
                "type": "snapshot",
                "summary": summary,
                "diagnostic": snapshot["diagnostic"],
            }
            target.write(json.dumps(record, separators=(",", ":"), allow_nan=False) + "\n")
            print(f"exported {position}/{len(selected)}", file=sys.stderr)
    temporary.replace(output)
    return metadata


def load_dataset(path: Path) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    metadata: dict[str, Any] | None = None
    snapshots: list[dict[str, Any]] = []
    with path.open("r", encoding="utf-8") as source:
        for line_number, line in enumerate(source, start=1):
            if not line.strip():
                continue
            record = json.loads(line)
            if not isinstance(record, dict):
                raise ValueError(f"dataset line {line_number} is not an object")
            if record.get("type") == "metadata":
                metadata = record
            elif record.get("type") == "snapshot":
                snapshots.append(record)
    if metadata is None or metadata.get("schema_version") != DATASET_SCHEMA_VERSION:
        raise ValueError("unsupported or missing backtest dataset metadata")
    return metadata, snapshots


def request_from_diagnostic(
    diagnostic: dict[str, Any],
    *,
    solver: str,
    formulation: str,
    time_limit_s: float,
    max_import_w: float,
    max_export_w: float,
    min_arbitrage_spread_ore_kwh: float,
) -> dict[str, Any]:
    if diagnostic.get("loadpoint_id"):
        raise SnapshotSkip("historical loadpoint contract is not persisted")
    params = diagnostic.get("params")
    slots = diagnostic.get("slots")
    if not isinstance(params, dict) or not isinstance(slots, list) or not slots:
        raise SnapshotSkip("diagnostic has no reconstructable params/slots")

    capacity_wh = float(params.get("capacity_wh", 0))
    if not math.isfinite(capacity_wh) or capacity_wh <= 0:
        raise SnapshotSkip("diagnostic has no battery capacity")
    initial_soc_pct = float(params.get("initial_soc_pct", 0))
    min_soc_pct = float(params.get("soc_min_pct", 0))
    max_soc_pct = float(params.get("soc_max_pct", 100))

    request_slots = []
    for slot in slots:
        if not isinstance(slot, dict):
            raise SnapshotSkip("diagnostic contains an invalid slot")
        request_slots.append(
            {
                "start_ms": int(slot["slot_start_ms"]),
                "len_min": int(slot["len_min"]),
                "price_ore": float(slot["price_ore"]),
                "spot_ore": float(slot.get("spot_ore", slot["price_ore"])),
                "confidence": float(slot.get("confidence", 1)),
                "pv_w": float(slot.get("pv_w", 0)),
                "load_w": float(slot.get("load_w", 0)),
                "max_import_w": max_import_w,
                "max_export_w": max_export_w,
            }
        )

    return {
        "schema_version": 1,
        "request_id": f"backtest-{uuid.uuid4()}",
        "settings": {
            "mode": str(params.get("mode", "self_consumption")),
            "solver": solver,
            "formulation": formulation,
            "time_limit_s": time_limit_s,
            "mip_rel_gap": 0.005,
            "export_bonus_ore_kwh": float(params.get("export_bonus_ore_kwh", 0)),
            "export_fee_ore_kwh": float(params.get("export_fee_ore_kwh", 0)),
            "export_floor_ore_kwh": params.get("export_floor_ore_kwh"),
            "min_arbitrage_spread_ore_kwh": min_arbitrage_spread_ore_kwh,
            "pv_charge_bonus_ore_kwh": float(params.get("pv_charge_bonus_ore_kwh", 0)),
            "cvar_weight": 0,
            "cvar_alpha": 0.9,
        },
        "slots": request_slots,
        "storages": [
            {
                "id": "historical-fleet",
                "capacity_wh": capacity_wh,
                "initial_energy_wh": capacity_wh * initial_soc_pct / 100,
                "min_energy_wh": capacity_wh * min_soc_pct / 100,
                "max_energy_wh": capacity_wh * max_soc_pct / 100,
                "max_charge_w": float(params.get("max_charge_w", 0)),
                "max_discharge_w": float(params.get("max_discharge_w", 0)),
                "charge_efficiency": float(params.get("charge_efficiency", 0.95)),
                "discharge_efficiency": float(params.get("discharge_efficiency", 0.95)),
                "terminal_price_ore_kwh": float(params.get("terminal_soc_price_ore_kwh", 0)),
                "cycle_cost_ore_kwh": 0,
            }
        ],
        "flex_loads": [],
        "thermal_loads": [],
    }


def _percentile(values: list[float], fraction: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    position = fraction * (len(ordered) - 1)
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def load_realized_csv(path: Path | None) -> dict[int, dict[str, float]]:
    if path is None:
        return {}
    rows: dict[int, dict[str, float]] = {}
    with path.open("r", encoding="utf-8", newline="") as source:
        for raw in csv.DictReader(source):
            try:
                start_ms = int(raw["bucket_start_ms"])
                rows[start_ms] = {
                    key: float(raw[key]) if raw.get(key, "") != "" else math.nan
                    for key in (
                        "bucket_end_ms",
                        "pv_w",
                        "ev_w",
                        "v2x_w",
                        "house_load_w",
                        "total_ore_kwh",
                        "spot_ore_kwh",
                    )
                }
            except (KeyError, TypeError, ValueError):
                continue
    return rows


def _record_timestamp_ms(record: dict[str, Any]) -> int:
    diagnostic = record.get("diagnostic", {})
    summary = record.get("summary", {})
    if not isinstance(diagnostic, dict):
        diagnostic = {}
    if not isinstance(summary, dict):
        summary = {}
    return int(summary.get("ts_ms", diagnostic.get("computed_at_ms", 0)))


def _first_slot(record: dict[str, Any]) -> dict[str, Any] | None:
    diagnostic = record.get("diagnostic", {})
    if not isinstance(diagnostic, dict):
        return None
    slots = diagnostic.get("slots", [])
    if not isinstance(slots, list) or not slots or not isinstance(slots[0], dict):
        return None
    return slots[0]


def select_causal_first_steps(
    snapshots: Iterable[dict[str, Any]],
    realized: dict[int, dict[str, float]],
) -> tuple[list[dict[str, Any]], Counter[str]]:
    """Pick at most one decision made by each realized interval's start.

    A diagnostic written after the interval starts cannot describe the action
    available at that decision cutoff. Among diagnostics available by the
    cutoff, the newest one wins. Realized intervals must also be valid and
    non-overlapping before their costs can be added.
    """

    by_interval: dict[int, dict[str, Any]] = {}
    exclusions: Counter[str] = Counter()
    for record in snapshots:
        slot = _first_slot(record)
        if slot is None:
            exclusions["missing first action"] += 1
            continue
        start_ms = int(slot.get("slot_start_ms", 0))
        actual = realized.get(start_ms)
        if actual is None:
            exclusions["missing realized interval"] += 1
            continue
        decision_ms = _record_timestamp_ms(record)
        if decision_ms <= 0:
            exclusions["missing decision timestamp"] += 1
            continue
        if decision_ms > start_ms:
            exclusions["diagnostic after decision cutoff"] += 1
            continue
        previous = by_interval.get(start_ms)
        if previous is None or decision_ms > _record_timestamp_ms(previous):
            if previous is not None:
                exclusions["superseded before decision cutoff"] += 1
            by_interval[start_ms] = record
        else:
            exclusions["superseded before decision cutoff"] += 1

    selected: list[dict[str, Any]] = []
    previous_end_ms = -math.inf
    for start_ms, record in sorted(by_interval.items()):
        actual = realized[start_ms]
        end_ms = actual.get("bucket_end_ms", math.nan)
        if not math.isfinite(end_ms) or end_ms <= start_ms:
            exclusions["invalid realized interval"] += 1
            continue
        if start_ms < previous_end_ms:
            exclusions["overlapping realized interval"] += 1
            continue
        selected.append(record)
        previous_end_ms = end_ms
    return selected, exclusions


def _interval_cost(
    grid_w: float,
    dt_h: float,
    import_ore_kwh: float,
    export_ore_kwh: float,
) -> float:
    grid_kwh = grid_w * dt_h / 1000.0
    return import_ore_kwh * max(grid_kwh, 0.0) - export_ore_kwh * max(-grid_kwh, 0.0)


def _forecast_balance_residual_w(
    slot: dict[str, Any], action: dict[str, Any]
) -> float | None:
    if action.get("grid_w") is None:
        return None
    grid_w = float(action["grid_w"])
    expected_grid_w = (
        float(slot.get("load_w", 0))
        + float(slot.get("pv_w", 0))
        + float(action.get("battery_w", 0))
    )
    if not math.isfinite(grid_w) or not math.isfinite(expected_grid_w):
        return None
    return grid_w - expected_grid_w


def dp_evaluation_reference(
    diagnostic: dict[str, Any],
) -> tuple[float, dict[str, Any]]:
    shadow = diagnostic.get("dp_evaluation_shadow")
    if isinstance(shadow, dict) and isinstance(shadow.get("first_action"), dict):
        return float(shadow.get("total_cost_ore", 0)), shadow["first_action"]

    solver = diagnostic.get("solver")
    engine = str(solver.get("engine", "")) if isinstance(solver, dict) else ""
    slots = diagnostic.get("slots", [])
    # "core" is the current label for the in-process DP; "go-dp" is what
    # snapshots written before #1020 carry.
    if engine in {"", "core", "go-dp"} and not diagnostic.get("optimizer_input") and slots:
        return float(diagnostic.get("total_cost_ore", 0)), slots[0]
    raise SnapshotSkip("missing same-input DP evaluation shadow")


def first_action_counterfactual(
    diagnostic: dict[str, Any],
    response: dict[str, Any],
    realized: dict[int, dict[str, float]],
    max_import_w: float,
    max_export_w: float,
    old_action: dict[str, Any] | None = None,
) -> dict[str, Any] | None:
    slots = diagnostic.get("slots", [])
    actions = response.get("plan", {}).get("actions", [])
    if not slots or not actions:
        return None
    old_slot = slots[0]
    new = actions[0]
    if old_action is None:
        try:
            _, old_action = dp_evaluation_reference(diagnostic)
        except SnapshotSkip:
            return None
    start_ms = int(old_slot.get("slot_start_ms", 0))
    actual = realized.get(start_ms)
    if actual is None:
        return None
    raw_interval_end_ms = actual.get("bucket_end_ms", math.nan)
    common = {
        "eligible": False,
        "interval_start_ms": start_ms,
        "interval_end_ms": (
            int(raw_interval_end_ms) if math.isfinite(raw_interval_end_ms) else None
        ),
        "decision_cutoff_ms": start_ms,
        "metric_scope": "grid_boundary_energy_only",
    }
    required = (
        raw_interval_end_ms,
        actual.get("pv_w", math.nan),
        actual.get("ev_w", math.nan),
        actual.get("v2x_w", math.nan),
        actual.get("house_load_w", math.nan),
        actual.get("total_ore_kwh", math.nan),
        actual.get("spot_ore_kwh", math.nan),
    )
    if not all(math.isfinite(value) for value in required):
        return {**common, "excluded_reason": "non-finite realized interval"}

    interval_end_ms = int(actual["bucket_end_ms"])
    planned_end_ms = start_ms + int(old_slot.get("len_min", 0)) * 60_000
    if planned_end_ms != interval_end_ms:
        return {
            **common,
            "excluded_reason": "realized interval does not match the first action",
        }

    reference_pv_limit_w = float(old_action.get("pv_limit_w", 0) or 0)
    candidate_pv_limit_w = float(new.get("pv_limit_w", 0) or 0)
    reference_balance_residual_w = _forecast_balance_residual_w(old_slot, old_action)
    candidate_balance_residual_w = _forecast_balance_residual_w(old_slot, new)
    balance_implies_curtailment = any(
        residual is not None and abs(residual) > 2
        for residual in (reference_balance_residual_w, candidate_balance_residual_w)
    )
    if (
        reference_pv_limit_w > 1e-5
        or candidate_pv_limit_w > 1e-5
        or balance_implies_curtailment
    ):
        return {
            **common,
            "excluded_reason": "PV curtailment is not modeled in counterfactual replay",
            "reference_pv_limit_w": reference_pv_limit_w,
            "candidate_pv_limit_w": candidate_pv_limit_w,
            "reference_forecast_balance_residual_w": reference_balance_residual_w,
            "candidate_forecast_balance_residual_w": candidate_balance_residual_w,
        }

    params = diagnostic.get("params", {})
    export_ore = actual["spot_ore_kwh"]
    export_ore += float(params.get("export_bonus_ore_kwh", 0))
    export_ore -= float(params.get("export_fee_ore_kwh", 0))
    floor = params.get("export_floor_ore_kwh")
    if floor is not None:
        export_ore = max(export_ore, float(floor))
    base_w = actual["house_load_w"] + actual["ev_w"] + actual["v2x_w"] + actual["pv_w"]
    old_grid_w = base_w + float(old_action.get("battery_w", 0))
    new_grid_w = base_w + float(new.get("battery_w", 0))
    dt_h = (actual["bucket_end_ms"] - start_ms) / 3_600_000.0
    mode = str(params.get("mode", "self_consumption"))
    min_grid_w = min(0.0, base_w)
    if mode == "self_consumption":
        mode_violation = not (
            min(base_w, 0.0) - 50 <= new_grid_w <= max(base_w, 0.0) + 50
        )
    elif mode in {"cheap_charge", "passive_arbitrage"}:
        mode_violation = new_grid_w < min_grid_w - 50
    else:
        mode_violation = False
    limit_violation = (
        (max_import_w > 0 and new_grid_w > max_import_w + 2)
        or (max_export_w > 0 and new_grid_w < -max_export_w - 2)
    )
    old_cost = _interval_cost(old_grid_w, dt_h, actual["total_ore_kwh"], export_ore)
    new_cost = _interval_cost(new_grid_w, dt_h, actual["total_ore_kwh"], export_ore)
    return {
        **common,
        "eligible": True,
        "actual_base_w": base_w,
        "forecast_base_w": float(old_slot.get("load_w", 0)) + float(old_slot.get("pv_w", 0)),
        "reference_battery_w": float(old_action.get("battery_w", 0)),
        "candidate_battery_w": float(new.get("battery_w", 0)),
        "reference_grid_w": old_grid_w,
        "candidate_grid_w": new_grid_w,
        "reference_grid_cost_ore": old_cost,
        "candidate_grid_cost_ore": new_cost,
        "grid_cost_delta_ore": new_cost - old_cost,
        "mode_violation": mode_violation,
        "limit_violation": limit_violation,
    }


def realized_first_slot(
    diagnostic: dict[str, Any],
    response: dict[str, Any],
    realized: dict[int, dict[str, float]],
    max_import_w: float,
    max_export_w: float,
    old_action: dict[str, Any] | None = None,
) -> dict[str, Any] | None:
    """Compatibility alias; reports a first-action counterfactual, not delivery."""

    return first_action_counterfactual(
        diagnostic,
        response,
        realized,
        max_import_w,
        max_export_w,
        old_action,
    )


def run_backtest(
    dataset: Path,
    output: Path,
    *,
    solver: str,
    formulation: str,
    time_limit_s: float,
    max_import_w: float,
    max_export_w: float,
    min_arbitrage_spread_ore_kwh: float,
    limit: int,
    realized_csv: Path | None,
) -> dict[str, Any]:
    metadata, snapshots = load_dataset(dataset)
    realized = load_realized_csv(realized_csv)
    if limit > 0:
        snapshots = snapshots[:limit]
    counterfactual_requested = realized_csv is not None
    selection_exclusions: Counter[str] = Counter()
    if counterfactual_requested:
        replay_records, selection_exclusions = select_causal_first_steps(snapshots, realized)
    else:
        replay_records = sorted(snapshots, key=_record_timestamp_ms)

    results: list[dict[str, Any]] = []
    failures: Counter[str] = Counter()
    skips: Counter[str] = Counter()
    counterfactual_exclusions: Counter[str] = Counter()
    solve_times: list[float] = []
    horizon_deltas: list[float] = []
    out_of_bounds_starts = 0
    first_action_rows: list[dict[str, Any]] = []

    for position, record in enumerate(replay_records, start=1):
        diagnostic = record.get("diagnostic", {})
        summary = record.get("summary", {})
        params = diagnostic.get("params", {}) if isinstance(diagnostic, dict) else {}
        initial = float(params.get("initial_soc_pct", 0)) if isinstance(params, dict) else 0
        minimum = float(params.get("soc_min_pct", 0)) if isinstance(params, dict) else 0
        maximum = float(params.get("soc_max_pct", 100)) if isinstance(params, dict) else 100
        if initial < minimum - 1e-9 or initial > maximum + 1e-9:
            out_of_bounds_starts += 1
        try:
            request = request_from_diagnostic(
                diagnostic,
                solver=solver,
                formulation=formulation,
                time_limit_s=time_limit_s,
                max_import_w=max_import_w,
                max_export_w=max_export_w,
                min_arbitrage_spread_ore_kwh=min_arbitrage_spread_ore_kwh,
            )
            old_cost, old_action = dp_evaluation_reference(diagnostic)
        except SnapshotSkip as exc:
            skips[str(exc)] += 1
            continue

        response = handle(request)
        row = {
            "ts_ms": int(summary.get("ts_ms", diagnostic.get("computed_at_ms", 0))),
            "reason": str(summary.get("reason", diagnostic.get("last_reason", "unknown"))),
            "initial_soc_pct": initial,
        }
        if not response.get("ok"):
            error = response.get("error", {})
            message = f"{error.get('code', 'unknown')}: {error.get('message', 'unknown')}"
            failures[message] += 1
            row.update({"ok": False, "error": message})
            results.append(row)
            print(f"replayed {position}/{len(replay_records)} failed: {message}", file=sys.stderr)
            continue

        new_cost = float(response["plan"]["total_cost_ore"])
        solve_ms = float(response["solver"]["solve_ms"])
        delta = new_cost - old_cost
        horizon_deltas.append(delta)
        solve_times.append(solve_ms)
        row.update(
            {
                "ok": True,
                "horizon_objective": {
                    "reference_dp_cost_ore": old_cost,
                    "candidate_optimizer_cost_ore": new_cost,
                    "delta_ore": delta,
                    "additive": False,
                },
                "solve_ms": solve_ms,
                "status": response["solver"]["status"],
                "formulation": response["solver"]["formulation"],
                "service_slack": response["solver"]["service_slack"],
            }
        )
        if counterfactual_requested:
            counterfactual = first_action_counterfactual(
                diagnostic, response, realized, max_import_w, max_export_w, old_action
            )
            if counterfactual is not None:
                row["first_action_counterfactual"] = counterfactual
                if counterfactual["eligible"]:
                    first_action_rows.append(counterfactual)
                else:
                    counterfactual_exclusions[str(counterfactual["excluded_reason"])] += 1
            else:
                counterfactual_exclusions["counterfactual unavailable"] += 1
        results.append(row)
        print(f"replayed {position}/{len(replay_records)}", file=sys.stderr)

    first_action_deltas = [
        float(row["grid_cost_delta_ore"]) for row in first_action_rows
    ]

    report = {
        "schema_version": 2,
        "generated_at_ms": int(time.time() * 1000),
        "dataset": metadata,
        "configuration": {
            "solver": solver,
            "formulation": formulation,
            "time_limit_s": time_limit_s,
            "max_import_w": max_import_w,
            "max_export_w": max_export_w,
            "min_arbitrage_spread_ore_kwh": min_arbitrage_spread_ore_kwh,
            "historical_scenarios": False,
            "decision_cutoff": "realized_interval_start",
            "state_policy": "independent_persisted_snapshot",
        },
        "summary": {
            "snapshots": len(snapshots),
            "replayed_snapshots": len(replay_records),
            "solved": len(solve_times),
            "failed": sum(failures.values()),
            "skipped": sum(skips.values()),
            "out_of_bounds_starts": out_of_bounds_starts,
            "solve_ms": {
                "p50": _percentile(solve_times, 0.50),
                "p95": _percentile(solve_times, 0.95),
                "p99": _percentile(solve_times, 0.99),
                "max": max(solve_times) if solve_times else None,
            },
            "horizon_objective_diagnostics": {
                "comparisons": len(horizon_deltas),
                "additive": False,
                "delta_p50": _percentile(horizon_deltas, 0.50),
                "delta_p95": _percentile(horizon_deltas, 0.95),
            },
            "first_action_counterfactual": {
                "requested": counterfactual_requested,
                "metric_scope": "grid_boundary_energy_only",
                "selected_intervals": len(replay_records) if counterfactual_requested else 0,
                "scored_intervals": len(first_action_rows),
                "reference_dp_grid_cost_ore": sum(
                    float(row["reference_grid_cost_ore"]) for row in first_action_rows
                ),
                "candidate_optimizer_grid_cost_ore": sum(
                    float(row["candidate_grid_cost_ore"]) for row in first_action_rows
                ),
                "grid_cost_delta_ore": sum(first_action_deltas),
                "grid_cost_delta_p50": _percentile(first_action_deltas, 0.50),
                "grid_cost_delta_p95": _percentile(first_action_deltas, 0.95),
                "mode_violations": sum(bool(row["mode_violation"]) for row in first_action_rows),
                "limit_violations": sum(bool(row["limit_violation"]) for row in first_action_rows),
                "selection_exclusions": dict(selection_exclusions.most_common()),
                "counterfactual_exclusions": dict(counterfactual_exclusions.most_common()),
            },
            "failures": dict(failures.most_common()),
            "skips": dict(skips.most_common()),
        },
        "limitations": [
            "Historical diagnostics preserve forecast snapshots, not realized outcomes.",
            "The legacy diagnostic schema does not preserve full loadpoint contracts; those snapshots are skipped.",
            "Historical PV/load scenario distributions are unavailable, so replay uses the persisted base/downside slots without CVaR.",
            "Overlapping full-horizon objectives are per-snapshot diagnostics and are never summed.",
            "First-action counterfactuals reprice planned battery actions against realized exogenous interval averages; neither command results nor measured battery delivery are persisted.",
            "Each first-action comparison starts from its diagnostic's persisted SoC. Closed-loop state propagation needs both policies to be recomputed from the same propagated state.",
            "First-action sums cover grid-boundary energy cost only. They omit battery wear and end-energy value, so they cannot rank policies that finish an interval with different stored energy.",
            "Counterfactuals with a positive PV limit or a forecast balance that implies curtailment are excluded because replay does not model curtailed PV.",
            "The legacy active-zero PV cap is detectable only when persisted grid power exposes its forecast balance residual.",
        ],
        "results": results,
    }
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(report, indent=2, allow_nan=False) + "\n", encoding="utf-8")
    return report


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Export and replay historical MPC diagnostics")
    subparsers = parser.add_subparsers(dest="command", required=True)

    export = subparsers.add_parser("export", help="export a read-only diagnostic sample")
    export.add_argument("--api-base", required=True)
    export.add_argument("--output", type=Path, required=True)
    export.add_argument("--days", type=int, default=30)
    export.add_argument("--samples", type=int, default=200)
    export.add_argument("--timeout-s", type=float, default=30)

    run = subparsers.add_parser("run", help="solve a previously exported dataset offline")
    run.add_argument("--input", type=Path, required=True)
    run.add_argument("--output", type=Path, required=True)
    run.add_argument("--solver", choices=["HIGHS", "CLARABEL"], default="HIGHS")
    run.add_argument("--formulation", choices=["auto", "milp", "relaxed"], default="auto")
    run.add_argument("--time-limit-s", type=float, default=5)
    run.add_argument("--max-import-w", type=float, default=0)
    run.add_argument("--max-export-w", type=float, default=0)
    run.add_argument("--min-arbitrage-spread-ore-kwh", type=float, default=0)
    run.add_argument("--limit", type=int, default=0)
    run.add_argument("--realized-csv", type=Path)
    return parser


def main() -> None:
    args = _parser().parse_args()
    if args.command == "export":
        if args.days <= 0 or args.samples <= 0:
            raise SystemExit("--days and --samples must be positive")
        summary = export_dataset(args.api_base, args.output, args.days, args.samples, args.timeout_s)
    else:
        summary = run_backtest(
            args.input,
            args.output,
            solver=args.solver,
            formulation=args.formulation,
            time_limit_s=args.time_limit_s,
            max_import_w=max(0, args.max_import_w),
            max_export_w=max(0, args.max_export_w),
            min_arbitrage_spread_ore_kwh=max(0, args.min_arbitrage_spread_ore_kwh),
            limit=max(0, args.limit),
            realized_csv=args.realized_csv,
        )["summary"]
    json.dump(summary, sys.stdout, indent=2, allow_nan=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
