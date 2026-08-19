from __future__ import annotations

import json
import math
from pathlib import Path

import pytest

from ftw_optimizer.protocol import ProtocolError
from ftw_optimizer.preference import flatten_peaks_enabled
from ftw_optimizer.worker import handshake, handle


CASES = Path(__file__).parent / "cases"


def test_handshake_advertises_peak_flattening() -> None:
    response = handshake({"type": "handshake", "protocol_version": 1})
    assert response is not None
    assert "preference_flatten_peaks" in response["features"]


def test_flatten_peaks_rejects_a_non_boolean() -> None:
    with pytest.raises(ProtocolError, match="boolean"):
        flatten_peaks_enabled({"flatten_peaks": 1})


def flatten_request(*, enabled: bool, backend: str) -> dict:
    request = json.loads((CASES / "flatten-equal-price-charge.json").read_text())["request"]
    request = json.loads(json.dumps(request))
    request["request_id"] = f"flatten-{backend}-{enabled}"
    request["settings"]["flatten_peaks"] = enabled
    request["settings"]["shared_backend"] = backend
    return request


@pytest.mark.parametrize("backend", ["highs", "cvxpy"])
def test_flatten_spreads_cost_neutral_grid_charge(backend: str) -> None:
    response = handle(flatten_request(enabled=True, backend=backend))
    assert response["ok"], response
    assert response["solver"]["preference_stage"] == "flattened"
    assert response["solver"]["import_peak_w"] <= 2100
    cheap = response["plan"]["actions"][:2]
    assert sum(action["battery_w"] for action in cheap) >= 3900
    assert all(action["battery_w"] >= 1000 for action in cheap)


@pytest.mark.parametrize("backend", ["highs", "cvxpy"])
def test_flatten_does_not_spend_money(backend: str) -> None:
    flat = handle(flatten_request(enabled=True, backend=backend))
    spiked = handle(flatten_request(enabled=False, backend=backend))
    assert flat["ok"], flat
    assert spiked["ok"], spiked
    assert math.isclose(
        flat["solver"]["objective_ore"],
        spiked["solver"]["objective_ore"],
        abs_tol=0.2,
    )
    assert flat["solver"]["import_peak_w"] <= spiked["solver"]["import_peak_w"] + 1.0


def test_golden_flatten_case_caps_the_import_peak() -> None:
    case = json.loads((CASES / "flatten-equal-price-charge.json").read_text())
    response = handle(case["request"])
    assert response["ok"] is case["expect"]["ok"], response
    assert response["solver"]["preference_stage"] == case["expect"]["preference_stage"]
    assert response["solver"]["import_peak_w"] <= case["expect"]["max_import_peak_w"]


def test_service_report_names_an_unmet_ev_deadline() -> None:
    request = {
        "schema_version": 1,
        "request_id": "ev-shortfall",
        "settings": {
            "mode": "arbitrage",
            "solver": "HIGHS",
            "formulation": "auto",
            "time_limit_s": 2,
            "mip_rel_gap": 0.001,
            "shared_backend": "cvxpy",
        },
        "slots": [
            {
                "start_ms": 1,
                "len_min": 60,
                "price_ore": 20,
                "spot_ore": 0,
                "confidence": 1,
                "pv_w": 0,
                "load_w": 0,
                "max_import_w": 8000,
                "max_export_w": 8000,
            }
        ],
        "storages": [],
        "flex_loads": [
            {
                "id": "car",
                "capacity_wh": 60000,
                "initial_energy_wh": 10000,
                "max_energy_wh": 60000,
                "target_energy_wh": 50000,
                "target_slot": 0,
                "charge_efficiency": 1,
                "max_charge_w": 2000,
                "allowed_steps_w": [0, 2000],
            }
        ],
        "thermal_loads": [],
        "commercial_constraints": {},
    }
    response = handle(request)
    assert response["ok"], response
    report = response["solver"].get("service_report") or {}
    assert report["flex_shortfall_wh"]["car"] >= 37000
    assert response["plan"]["actions"][0]["flex_energy_wh"]["car"] == pytest.approx(
        12000, abs=1
    )
