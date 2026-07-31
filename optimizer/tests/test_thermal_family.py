from __future__ import annotations

import json
import math
import urllib.parse
from pathlib import Path

import pytest

from ftw_optimizer.home_spec import (
    HOME_SPEC_KIND,
    HOME_SPEC_SCHEMA_VERSION,
    TWO_R_TWO_C_MODEL_TYPE,
    HomeSpec,
)
from ftw_optimizer.thermal_backtest import export_thermal_dataset
from ftw_optimizer.thermal_family import (
    TwoR2CArtifact,
    artifact_from_dict,
    calibrate_model_family,
    simulate_two_r2c,
)
from ftw_optimizer.thermal_twin import (
    MODEL_TYPE,
    COPCurve,
    CalibrationError,
    CalibrationEvidence,
    ThermalObservation,
    ThermalTwinArtifact,
    simulate_thermal_twin,
)
from ftw_optimizer.worker import handle, handshake


def calibration_evidence() -> CalibrationEvidence:
    return CalibrationEvidence(
        source="synthetic-ground-truth",
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
        promotable=True,
        promotion_reasons=(),
    )


def home_spec(
    *,
    candidates: tuple[str, ...] = (
        MODEL_TYPE,
        TWO_R_TWO_C_MODEL_TYPE,
    ),
    include_power: bool = True,
) -> HomeSpec:
    sensors = {
        "indoor_temperature": {
            "driver": "heat-pump",
            "metric": "hp_indoor_temp_c",
        },
        "outdoor_temperature": {
            "driver": "heat-pump",
            "metric": "hp_outdoor_temp_c",
        },
    }
    if include_power:
        sensors["heat_pump_power"] = {
            "driver": "heat-pump",
            "metric": "hp_power_w",
        }
    return HomeSpec.from_dict(
        {
            "schema_version": HOME_SPEC_SCHEMA_VERSION,
            "kind": HOME_SPEC_KIND,
            "site_id": "home",
            "primary_zone_id": "main",
            "zones": [
                {
                    "id": "main",
                    "floor_area_m2": 145,
                    "volume_m3": 360,
                    "comfort": {
                        "minimum_temperature_c": 19,
                        "maximum_temperature_c": 23,
                    },
                }
            ],
            "heating": {
                "source": "air_water_heat_pump",
                "emitters": "radiators",
                "maximum_electric_power_w": 4_000,
                "cop_curve": {
                    "reference_temperature_c": 7,
                    "cop_at_reference": 3.4,
                    "slope_per_c": 0.05,
                    "minimum_cop": 1.5,
                    "maximum_cop": 5.5,
                },
                "buffer_tank_l": 200,
            },
            "sensors": sensors,
            "priors": {
                "heat_loss_w_per_k": [40, 400],
                "total_capacity_wh_per_k": [2_000, 60_000],
                "mass_coupling_w_per_k": [80, 4_000],
                "air_capacity_fraction": [0.02, 0.5],
                "disturbance_heat_w": [-2_000, 3_000],
            },
            "model_selection": {
                "candidates": list(candidates),
                "train_fraction": 0.75,
                "minimum_rollout_improvement_c": 0.03,
                "minimum_relative_improvement": 0.1,
            },
        }
    )


def two_r2c_artifact() -> TwoR2CArtifact:
    return TwoR2CArtifact(
        model_id="main",
        heat_loss_w_per_k=160,
        mass_coupling_w_per_k=900,
        air_capacity_wh_per_k=1_200,
        mass_capacity_wh_per_k=14_000,
        cop_curve=home_spec().heating.cop_curve,
        disturbance_heat_w=250,
        calibration=calibration_evidence(),
    )


def synthetic_inputs(
    transition_count: int = 7 * 24 * 4,
) -> tuple[list[float], list[float]]:
    outside = [
        -2
        + 8 * math.sin(2 * math.pi * index / 96)
        + 2 * math.sin(2 * math.pi * index / 37)
        for index in range(transition_count)
    ]
    levels = (0.0, 700.0, 1_600.0, 2_800.0, 3_600.0, 900.0)
    power = [
        levels[(index * 13 + index // 9) % len(levels)]
        for index in range(transition_count)
    ]
    return outside, power


def observations_from_two_r2c() -> list[ThermalObservation]:
    artifact = two_r2c_artifact()
    outside, power = synthetic_inputs()
    states = simulate_two_r2c(
        artifact,
        initial_air_temperature_c=20.5,
        initial_mass_temperature_c=20.0,
        outside_temperature_c=outside,
        heat_pump_power_w=power,
        step_h=0.25,
    )
    return [
        ThermalObservation(
            timestamp_s=index * 900,
            indoor_temperature_c=state[0],
            outdoor_temperature_c=(
                outside[index] if index < len(outside) else outside[-1]
            ),
            heat_pump_power_w=(
                power[index] if index < len(power) else power[-1]
            ),
        )
        for index, state in enumerate(states)
    ]


def test_home_spec_is_content_addressed_and_strict() -> None:
    expected = home_spec()
    actual = HomeSpec.from_dict(expected.to_dict())
    assert actual == expected
    assert len(actual.revision) == 64
    assert actual.primary_zone.floor_area_m2 == 145

    tampered = expected.to_dict()
    tampered["zones"][0]["floor_area_m2"] = 146
    with pytest.raises(CalibrationError, match="revision"):
        HomeSpec.from_dict(tampered)

    example = Path(__file__).parents[1] / "home-spec.example.json"
    loaded_example = HomeSpec.from_dict(
        json.loads(example.read_text(encoding="utf-8"))
    )
    assert loaded_example.model_selection.candidates == (
        MODEL_TYPE,
        TWO_R_TWO_C_MODEL_TYPE,
    )


def test_two_r2c_artifact_round_trip_is_content_addressed() -> None:
    expected = two_r2c_artifact()
    actual = artifact_from_dict(expected.to_dict())
    assert actual == expected

    tampered = expected.to_dict()
    tampered["physics"]["mass_capacity_wh_per_k"] += 1
    with pytest.raises(CalibrationError, match="revision"):
        artifact_from_dict(tampered)


def test_model_family_selects_two_r2c_for_two_time_scales() -> None:
    result = calibrate_model_family(
        observations_from_two_r2c(),
        home_spec=home_spec(),
        source="heat_pump_submeter",
    )
    summaries = {
        candidate.model_type: candidate
        for candidate in result.report.candidates
    }

    assert result.report.champion_model_type == TWO_R_TWO_C_MODEL_TYPE
    assert summaries[TWO_R_TWO_C_MODEL_TYPE].promotable
    assert summaries[TWO_R_TWO_C_MODEL_TYPE].rollout_rmse_c is not None
    assert (
        summaries[MODEL_TYPE].status == "failed"
        or (
            summaries[MODEL_TYPE].rollout_rmse_c is not None
            and summaries[TWO_R_TWO_C_MODEL_TYPE].rollout_rmse_c
            < summaries[MODEL_TYPE].rollout_rmse_c - 0.03
        )
    )


def test_model_family_keeps_one_r1c_when_complexity_adds_no_value() -> None:
    spec = home_spec()
    expected = ThermalTwinArtifact(
        model_id="main",
        heat_loss_w_per_k=160,
        thermal_capacity_wh_per_k=15_200,
        cop_curve=spec.heating.cop_curve,
        disturbance_heat_w=250,
        calibration=calibration_evidence(),
    )
    outside, power = synthetic_inputs()
    indoor = simulate_thermal_twin(
        expected,
        initial_temperature_c=20.5,
        outside_temperature_c=outside,
        heat_pump_power_w=power,
        step_h=0.25,
    )
    observations = [
        ThermalObservation(
            timestamp_s=index * 900,
            indoor_temperature_c=temperature,
            outdoor_temperature_c=(
                outside[index] if index < len(outside) else outside[-1]
            ),
            heat_pump_power_w=(
                power[index] if index < len(power) else power[-1]
            ),
        )
        for index, temperature in enumerate(indoor)
    ]

    result = calibrate_model_family(
        observations,
        home_spec=spec,
    )

    assert result.report.champion_model_type == MODEL_TYPE


def optimizer_request(artifact: TwoR2CArtifact) -> dict:
    slots = [
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
        for index, price in enumerate((20, 30, 250, 300, 100, 80))
    ]
    return {
        "schema_version": 1,
        "request_id": "thermal-family-test",
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
            artifact.optimizer_load(
                initial_temperature_c=20,
                initial_mass_temperature_c=20,
                minimum_temperature_c=19,
                maximum_temperature_c=22,
                outside_temperature_c=[0] * len(slots),
                max_electric_power_w=4_000,
            )
        ],
    }


def test_two_r2c_runs_in_shared_optimizer() -> None:
    response = handle(optimizer_request(two_r2c_artifact()))

    assert response["ok"], response
    temperatures = [
        action["thermal_state"]["main"]
        for action in response["plan"]["actions"]
    ]
    assert min(temperatures) >= 19 - 1e-5
    assert "thermal_twin_2r2c_v1" in handshake(
        {"type": "handshake"}
    )["features"]


def test_exporter_chunks_and_aligns_ftw_series() -> None:
    spec = home_spec()
    since_ms = 1_800_000
    step_ms = 3_600_000
    until_ms = since_ms + 96 * step_ms
    values = {
        "hp_indoor_temp_c": 20.5,
        "hp_outdoor_temp_c": 2.0,
        "hp_power_w": 1_500.0,
    }
    calls: list[dict[str, list[str]]] = []

    def fetch(url: str, _timeout_s: float) -> dict:
        query = urllib.parse.parse_qs(
            urllib.parse.urlparse(url).query
        )
        calls.append(query)
        chunk_start = int(query["since"][0])
        chunk_end = int(query["until"][0])
        metric = query["metric"][0]
        points = []
        timestamp = chunk_start + step_ms - 1
        while timestamp < chunk_end:
            points.append(
                {
                    "ts": timestamp,
                    "v": values[metric],
                    "min": values[metric],
                    "max": values[metric],
                    "n": 60,
                }
            )
            timestamp += step_ms
        return {"metric": metric, "points": points}

    dataset = export_thermal_dataset(
        home_spec=spec,
        api_base="http://ftw.test",
        since_ms=since_ms,
        until_ms=until_ms,
        step_s=3_600,
        chunk_days=2,
        fetch_json=fetch,
    )

    assert dataset["ready"]
    assert dataset["coverage"]["longest_contiguous_samples"] == 96
    assert len(dataset["observations"]) == 96
    assert len(calls) == 6


def test_exporter_reports_missing_heat_pump_power_mapping() -> None:
    dataset = export_thermal_dataset(
        home_spec=home_spec(include_power=False),
        api_base="http://ftw.test",
        since_ms=1_000,
        until_ms=1_000 + 4 * 24 * 60 * 60 * 1_000,
    )

    assert not dataset["ready"]
    assert "heat_pump_power" in dataset["blocking_reasons"][0]


def test_exporter_accepts_null_points_and_redacts_url_credentials() -> None:
    def fetch(url: str, _timeout_s: float) -> dict:
        metric = urllib.parse.parse_qs(
            urllib.parse.urlparse(url).query
        )["metric"][0]
        return {"metric": metric, "points": None}

    dataset = export_thermal_dataset(
        home_spec=home_spec(),
        api_base="http://operator:secret@ftw.test:8080",
        since_ms=1_000,
        until_ms=1_000 + 4 * 24 * 60 * 60 * 1_000,
        fetch_json=fetch,
    )

    assert not dataset["ready"]
    assert dataset["source"] == "http://ftw.test:8080"
    assert dataset["coverage"]["sensor_bucket_counts"] == {
        "heat_pump_power": 0,
        "indoor_temperature": 0,
        "outdoor_temperature": 0,
    }
    assert any(
        "heat_pump_power has no samples" in reason
        for reason in dataset["blocking_reasons"]
    )


def test_two_node_modelica_reference_matches_optimizer_contract() -> None:
    modelica = Path(__file__).parents[1] / "modelica"
    model = (
        modelica / "FTW" / "TwoNodeHomeThermalTwin.mo"
    ).read_text(encoding="utf-8")
    build = (modelica / "build_fmu.mos").read_text(encoding="utf-8")

    assert "model TwoNodeHomeThermalTwin" in model
    assert "massCoupling" in model
    assert "airCapacity" in model
    assert "massCapacity" in model
    assert (
        "gridPowerW = nativeLoadW + heatPumpElectricPowerW + pvPowerW "
        "+ batteryPowerW"
    ) in model
    assert "FTW.TwoNodeHomeThermalTwin" in build
