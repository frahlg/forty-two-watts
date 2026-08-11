from __future__ import annotations

import copy
import time
from dataclasses import replace
from typing import Any

import numpy as np

from .direct_highs import (
    SharedBaselineReplayError,
    solve_direct_highs,
)
from .model import _STORAGE_INITIAL_ABOVE_MAXIMUM_KEY, _solver_options
from .multistage import _prepare
from .protocol import ProtocolError, finite_number, require_dict, require_list
from .scenario_tree import ScenarioTree


class DirectSharedIneligible(RuntimeError):
    pass


def solve_shared_highs(payload: dict[str, Any], started: float) -> dict[str, Any]:
    """Solve shared storage through the sparse HiGHS builder."""
    prepared_started = time.perf_counter()
    direct_payload, risk_alpha = _direct_payload(payload)
    prepared = replace(
        _prepare(direct_payload), economic_cvar_alpha=risk_alpha
    )

    solver = str(prepared.settings.get("solver", "HIGHS")).upper()
    if solver not in {"HIGHS", "CLARABEL"}:
        raise ProtocolError("settings.solver must be HIGHS or CLARABEL")
    if solver != "HIGHS":
        raise DirectSharedIneligible("direct shared backend requires solver HIGHS")
    if prepared.formulation == "milp":
        raise DirectSharedIneligible(
            "direct shared backend requires a continuous formulation"
        )
    if any(
        bool(spec.get(_STORAGE_INITIAL_ABOVE_MAXIMUM_KEY, False))
        for spec in prepared.storages
    ):
        raise DirectSharedIneligible(
            "direct shared backend requires storage starts at or below its operating maximum"
        )
    if (
        prepared.discrete
        or prepared.unsafe_cycle
        or bool(np.any(prepared.effective_import < 0))
        or prepared.unsafe_meter_split
    ):
        raise DirectSharedIneligible(
            "direct shared backend requires a cycle-safe continuous tariff"
        )

    shared_pv_generation = np.minimum.reduce(
        [
            np.maximum(0.0, -scenario.pv)
            for scenario in prepared.scenario_set.scenarios
        ]
    )
    scenario_count = len(prepared.scenario_set.scenarios)
    shared_tree = ScenarioTree(
        node_at=np.zeros((scenario_count, prepared.n), dtype=np.int64),
        branch_slots=(),
        node_count=1,
    )
    blocks = tuple((slot, slot + 1) for slot in range(prepared.n))

    # Match the shared champion's fallback site bound. Explicit slot limits
    # still take precedence in both implementations.
    max_site_power = max(
        1000.0,
        float(np.max(prepared.base_load + shared_pv_generation))
        + sum(
            float(spec.get("max_charge_w", 0))
            + float(spec.get("max_discharge_w", 0))
            for spec in prepared.storages
        ),
    )
    raw_import_limit = np.asarray(
        [
            max(
                0.0,
                finite_number(
                    slot.get("max_import_w", 0),
                    f"slots[{index}].max_import_w",
                ),
            )
            for index, slot in enumerate(prepared.slots)
        ]
    )
    raw_export_limit = np.asarray(
        [
            max(
                0.0,
                finite_number(
                    slot.get("max_export_w", 0),
                    f"slots[{index}].max_export_w",
                ),
            )
            for index, slot in enumerate(prepared.slots)
        ]
    )
    prepared = replace(
        prepared,
        tree=shared_tree,
        blocks=blocks,
        first_stage_slots=prepared.n,
        service_cvar_weight=0.0,
        max_site_power=max_site_power,
        import_bound=np.where(
            raw_import_limit > 0, raw_import_limit, max_site_power
        ),
        export_bound=np.where(
            raw_export_limit > 0, raw_export_limit, max_site_power
        ),
    )
    prepare_ms = (time.perf_counter() - prepared_started) * 1000.0
    deadline = started + float(
        _solver_options(prepared.settings, "HIGHS")["time_limit"]
    )
    try:
        return solve_direct_highs(
            prepared,
            started,
            prepare_ms,
            "shared",
            shared=True,
            deadline=deadline,
        )
    except SharedBaselineReplayError as exc:
        return solve_direct_highs(
            prepared,
            started,
            prepare_ms,
            "shared",
            shared=True,
            exact_shared_baseline=True,
            deadline=deadline,
            prior_build_ms=exc.build_ms,
            prior_solver_ms=exc.solver_ms,
        )


def _direct_payload(payload: dict[str, Any]) -> tuple[dict[str, Any], float]:
    if require_dict(
        payload.get("commercial_constraints", {}), "commercial_constraints"
    ):
        raise DirectSharedIneligible(
            "direct shared backend does not support commercial constraints"
        )
    if require_list(payload.get("flex_loads", []), "flex_loads"):
        raise DirectSharedIneligible(
            "direct shared backend does not support flex loads"
        )
    if require_list(payload.get("thermal_loads", []), "thermal_loads"):
        raise DirectSharedIneligible(
            "direct shared backend does not support thermal loads"
        )
    storages = require_list(payload.get("storages", []), "storages")
    if not storages:
        raise DirectSharedIneligible(
            "direct shared backend requires at least one storage"
        )
    slots = require_list(payload.get("slots", []), "slots")
    if not slots:
        raise ProtocolError("slots must not be empty")

    direct_payload = copy.deepcopy(payload)
    direct_storages = require_list(direct_payload["storages"], "storages")
    for index, raw in enumerate(direct_storages):
        spec = require_dict(raw, f"storages[{index}]")
        finite_number(
            spec.get("max_charge_w", 0), f"storages[{index}].max_charge_w"
        )
        finite_number(
            spec.get("max_discharge_w", 0),
            f"storages[{index}].max_discharge_w",
        )
        finite_number(
            spec.get("cycle_cost_ore_kwh", 0),
            f"storages[{index}].cycle_cost_ore_kwh",
        )
        finite_number(
            spec.get("throughput_cost_ore_kwh", 0),
            f"storages[{index}].throughput_cost_ore_kwh",
        )
        finite_number(
            spec.get("terminal_price_ore_kwh", 0),
            f"storages[{index}].terminal_price_ore_kwh",
        )
        if spec.get("target_energy_wh") is not None:
            finite_number(
                spec["target_energy_wh"],
                f"storages[{index}].target_energy_wh",
            )
            spec["target_slot"] = min(
                len(slots) - 1,
                max(0, int(spec.get("target_slot", len(slots) - 1))),
            )

    settings = dict(
        require_dict(direct_payload.get("settings", {}), "settings")
    )
    direct_payload["settings"] = settings
    raw_scenarios = require_list(direct_payload.get("scenarios", []), "scenarios")
    scenario_count = max(1, len(raw_scenarios))
    seen_scenario_ids: set[str] = set()
    for index, raw in enumerate(raw_scenarios):
        scenario = require_dict(raw, f"scenarios[{index}]")
        scenario_id = str(scenario.get("id", f"scenario-{index}"))
        if scenario_id in seen_scenario_ids:
            suffix = 1
            unique_id = f"{scenario_id}-{index}-{suffix}"
            while unique_id in seen_scenario_ids:
                suffix += 1
                unique_id = f"{scenario_id}-{index}-{suffix}"
            scenario["id"] = unique_id
            scenario_id = unique_id
        seen_scenario_ids.add(scenario_id)
    risk_weight = max(
        0.0,
        finite_number(settings.get("cvar_weight", 0), "settings.cvar_weight"),
    )
    risk_alpha = finite_number(
        settings.get("cvar_alpha", 0.9), "settings.cvar_alpha"
    )
    if risk_weight > 0 and scenario_count > 1 and not 0 < risk_alpha < 1:
        raise ProtocolError("settings.cvar_alpha must be between 0 and 1")

    # _prepare also serves multistage models. Pin its policy-only settings,
    # then replace the generated tree with the exact shared policy above.
    settings.update(
        {
            "scenario_limit": scenario_count,
            "non_anticipative_slots": len(slots),
            "branch_interval_slots": 1,
            "branch_horizon_slots": len(slots),
            "max_branching": 2,
            "near_horizon_slots": len(slots),
            "mid_horizon_slots": len(slots),
            "mid_block_slots": 1,
            "far_block_slots": 1,
            "service_cvar_weight": 0,
            "service_cvar_alpha": 0.95,
            "economic_cvar_weight": risk_weight,
            "economic_cvar_alpha": 0.9,
        }
    )
    return direct_payload, risk_alpha
