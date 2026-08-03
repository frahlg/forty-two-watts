from __future__ import annotations

import json
import math
from dataclasses import replace
from pathlib import Path

import pytest

from ftw_optimizer.thermal_twin import (
    COPCurve,
    CalibrationError,
    CalibrationEvidence,
    ThermalObservation,
    ThermalTwinArtifact,
    calibrate_thermal_twin,
    load_artifact,
    load_observations_csv,
    simulate_thermal_twin,
    site_grid_power_w,
    promotion_policy_reasons,
)
from ftw_optimizer.worker import handle, handshake


def evidence(*, promotable: bool = True) -> CalibrationEvidence:
    return CalibrationEvidence(
        source="heat_pump_submeter",
        dataset_sha256="a" * 64,
        resampling_recipe="synthetic-ground-truth-v2",
        calibrator_version="ftw-thermal-calibrator-v2",
        promotion_policy_version="ftw-thermal-promotion-v2",
        sample_count=673,
        transition_count=672,
        start_timestamp_s=0,
        end_timestamp_s=672 * 900,
        step_s=900,
        train_transition_count=504,
        validation_transition_count=168,
        standardized_condition_number=2,
        one_step_rmse_c=0,
        rollout_rmse_c=0,
        persistence_rmse_c=0.1,
        solver_converged=True,
        parameter_bounds_hit=False,
        observed_outdoor_min_c=-20,
        observed_outdoor_max_c=20,
        observed_power_min_w=0,
        observed_power_max_w=4_000,
        promotable=promotable,
        promotion_reasons=() if promotable else ("test only",),
    )


def artifact() -> ThermalTwinArtifact:
    return ThermalTwinArtifact(
        site_id="home",
        home_spec_revision="b" * 64,
        model_id="home-zone",
        heat_loss_w_per_k=180,
        thermal_capacity_wh_per_k=12_000,
        cop_curve=COPCurve(
            reference_temperature_c=7,
            cop_at_reference=3.4,
            slope_per_c=0.05,
            minimum_cop=1.5,
            maximum_cop=5.5,
        ),
        disturbance_heat_w=350,
        calibration=evidence(),
    )


def test_site_balance_uses_ftw_boundary_signs() -> None:
    assert site_grid_power_w(
        native_load_w=800,
        heat_pump_power_w=1_200,
        pv_w=-3_000,
        battery_w=500,
    ) == -500
    with pytest.raises(CalibrationError, match="pv_w <= 0"):
        site_grid_power_w(
            native_load_w=800,
            heat_pump_power_w=1_200,
            pv_w=3_000,
        )


def test_artifact_round_trip_is_content_addressed(tmp_path: Path) -> None:
    expected = artifact()
    path = tmp_path / "home.json"
    path.write_text(json.dumps(expected.to_dict()), encoding="utf-8")
    actual = load_artifact(path)
    assert actual == expected
    assert len(actual.revision) == 64

    tampered = expected.to_dict()
    tampered["physics"]["heat_loss_w_per_k"] = 181
    path.write_text(json.dumps(tampered), encoding="utf-8")
    with pytest.raises(CalibrationError, match="revision"):
        load_artifact(path)


def test_typed_revision_handles_unicode_and_html_characters() -> None:
    value = replace(
        artifact(),
        site_id="hem-å<&",
        model_id="zon-å<&",
    )
    assert value.revision == (
        "1e05dfec3d201f6c7c2442bfd763956716cd0a420a35f625ac57fe8610267eb0"
    )


def test_consumer_recomputes_promotion_policy() -> None:
    forged = replace(
        evidence(),
        resampling_recipe="series-bucket-average-v1",
    )
    assert "resampling recipe is not approved" in promotion_policy_reasons(
        forged,
        "ftw-1r1c-v1",
    )


def test_optimizer_rejects_artifact_that_failed_promotion() -> None:
    rejected = ThermalTwinArtifact(
        site_id="home",
        home_spec_revision="b" * 64,
        model_id="home-zone",
        heat_loss_w_per_k=180,
        thermal_capacity_wh_per_k=12_000,
        cop_curve=artifact().cop_curve,
        disturbance_heat_w=350,
        calibration=evidence(promotable=False),
    )
    load_args = {
        "initial_temperature_c": 20,
        "minimum_temperature_c": 19,
        "maximum_temperature_c": 22,
        "outside_temperature_c": [0, 1],
        "max_electric_power_w": 4_000,
    }

    with pytest.raises(CalibrationError, match="not promotable"):
        rejected.optimizer_load(**load_args)
    assert rejected.optimizer_load(
        **load_args,
        allow_unpromotable=True,
    )["source_revision"] == rejected.revision


def test_optimizer_load_accepts_initial_temperature_outside_comfort() -> None:
    load = artifact().optimizer_load(
        initial_temperature_c=18,
        minimum_temperature_c=19,
        maximum_temperature_c=22,
        outside_temperature_c=[0],
        max_electric_power_w=4_000,
    )
    assert load["initial_temp_c"] == 18


def test_optimizer_load_rejects_step_above_maximum_power() -> None:
    with pytest.raises(CalibrationError, match="must not exceed"):
        artifact().optimizer_load(
            initial_temperature_c=20,
            minimum_temperature_c=19,
            maximum_temperature_c=22,
            outside_temperature_c=[0],
            max_electric_power_w=4_000,
            allowed_steps_w=[0, 5_000],
        )


def test_calibration_recovers_synthetic_physics_and_holds_out_time() -> None:
    expected = artifact()
    step_h = 0.25
    transition_count = 7 * 24 * 4
    outside = [
        -3
        + 7 * math.sin(2 * math.pi * index / 96)
        + 1.5 * math.sin(2 * math.pi * index / 31)
        for index in range(transition_count)
    ]
    power_steps = (0.0, 900.0, 2_100.0, 3_300.0, 1_400.0)
    power = [
        power_steps[(index * 17 + index // 11) % len(power_steps)]
        for index in range(transition_count)
    ]
    indoor = simulate_thermal_twin(
        expected,
        initial_temperature_c=20.5,
        outside_temperature_c=outside,
        heat_pump_power_w=power,
        step_h=step_h,
    )
    observations = [
        ThermalObservation(
            timestamp_s=index * step_h * 3600,
            indoor_temperature_c=indoor[index],
            outdoor_temperature_c=outside[index]
            if index < transition_count
            else outside[-1],
            heat_pump_power_w=power[index]
            if index < transition_count
            else power[-1],
        )
        for index in range(transition_count + 1)
    ]

    fitted = calibrate_thermal_twin(
        observations,
        site_id="home",
        home_spec_revision="b" * 64,
        dataset_sha256="a" * 64,
        resampling_recipe="synthetic-ground-truth-v2",
        model_id="home-zone",
        cop_curve=expected.cop_curve,
        train_fraction=0.7,
    )

    assert fitted.heat_loss_w_per_k == pytest.approx(
        expected.heat_loss_w_per_k,
        rel=1e-6,
    )
    assert fitted.thermal_capacity_wh_per_k == pytest.approx(
        expected.thermal_capacity_wh_per_k,
        rel=1e-6,
    )
    assert fitted.disturbance_heat_w == pytest.approx(
        expected.disturbance_heat_w,
        rel=1e-6,
    )
    assert fitted.calibration.promotable
    assert fitted.calibration.rollout_rmse_c < 1e-8


def test_calibration_rejects_aggregate_grid_as_heat_pump_input(
    tmp_path: Path,
) -> None:
    path = tmp_path / "grid-only.csv"
    path.write_text(
        "timestamp_s,indoor_temp_c,outdoor_temp_c,grid_power_w\n"
        "0,20,0,1200\n",
        encoding="utf-8",
    )
    with pytest.raises(CalibrationError, match="grid_power_w alone"):
        load_observations_csv(path)


def test_calibration_rejects_unexcited_data() -> None:
    observations = [
        ThermalObservation(
            timestamp_s=index * 900,
            indoor_temperature_c=20,
            outdoor_temperature_c=0,
            heat_pump_power_w=1_000,
        )
        for index in range(60)
    ]
    with pytest.raises(CalibrationError, match="too little excitation"):
        calibrate_thermal_twin(
            observations,
            site_id="home",
            home_spec_revision="b" * 64,
            dataset_sha256="a" * 64,
            resampling_recipe="synthetic-ground-truth-v2",
            model_id="flat-data",
            cop_curve=artifact().cop_curve,
        )


def optimizer_request() -> dict:
    slots = []
    for index, price in enumerate((20, 30, 250, 300, 100, 80)):
        slots.append(
            {
                "start_ms": index * 3_600_000 + 1,
                "len_min": 60,
                "price_ore": price,
                "spot_ore": price,
                "confidence": 1,
                "pv_w": 0,
                "load_w": 500,
                "max_import_w": 10_000,
                "max_export_w": 10_000,
            }
        )
    return {
        "schema_version": 1,
        "request_id": "thermal-twin-test",
        "settings": {
            "mode": "arbitrage",
            "solver": "HIGHS",
            "formulation": "relaxed",
            "time_limit_s": 2,
            "mip_rel_gap": 0.001,
            "export_bonus_ore_kwh": 0,
            "export_fee_ore_kwh": 0,
        },
        "slots": slots,
        "storages": [],
        "flex_loads": [],
        "thermal_loads": [
            artifact().optimizer_load(
                initial_temperature_c=20,
                minimum_temperature_c=19,
                maximum_temperature_c=22,
                outside_temperature_c=[0] * len(slots),
                max_electric_power_w=4_000,
            )
        ],
    }


def test_physical_twin_runs_in_shared_optimizer() -> None:
    response = handle(optimizer_request())
    assert response["ok"], response
    actions = response["plan"]["actions"]
    temperatures = [
        action["thermal_state"]["home-zone"] for action in actions
    ]
    assert min(temperatures) >= 19 - 1e-5
    assert all(
        action["thermal_power_w"]["home-zone"] >= -1e-7
        for action in actions
    )
    assert "thermal_twin_1r1c_v1" in handshake(
        {"type": "handshake"}
    )["features"]


def test_physical_and_legacy_thermal_parameters_cannot_mix() -> None:
    request = optimizer_request()
    request["thermal_loads"][0]["gain_c_per_kwh"] = 1
    response = handle(request)
    assert not response["ok"]
    assert response["error"]["code"] == "invalid_request"
    assert "cannot also set gain_c_per_kwh" in response["error"]["message"]


def test_relaxed_formulation_rejects_discrete_heat_pump_steps() -> None:
    request = optimizer_request()
    request["thermal_loads"] = [
        artifact().optimizer_load(
            initial_temperature_c=20,
            minimum_temperature_c=19,
            maximum_temperature_c=22,
            outside_temperature_c=[0] * len(request["slots"]),
            max_electric_power_w=4_000,
            allowed_steps_w=[0, 4_000],
        )
    ]
    response = handle(request)
    assert not response["ok"]
    assert "requires auto or milp" in response["error"]["message"]


def test_modelica_reference_has_same_site_boundary_and_fmi_mode() -> None:
    modelica = Path(__file__).parents[1] / "modelica"
    model = (modelica / "FTW" / "HomeThermalTwin.mo").read_text(
        encoding="utf-8"
    )
    build = (modelica / "build_fmu.mos").read_text(encoding="utf-8")
    assert (
        "gridPowerW = nativeLoadW + heatPumpElectricPowerW + pvPowerW "
        "+ batteryPowerW"
    ) in model
    assert "Modelica.Thermal.HeatTransfer.Components.HeatCapacitor" in model
    assert 'version = "2.0"' in build
    assert 'fmuType = "cs"' in build
