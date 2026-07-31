from __future__ import annotations

import argparse
import csv
import hashlib
import itertools
import json
import math
import sys
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Any, Iterable, Sequence

import numpy as np

from .protocol import ProtocolError, finite_number, positive_number, require_dict, require_list


ARTIFACT_KIND = "ftw.thermal_twin"
ARTIFACT_SCHEMA_VERSION = 1
MODEL_TYPE = "ftw-1r1c-v1"
MIN_TRANSITIONS = 48


class CalibrationError(ValueError):
    pass


def _finite(value: Any, field: str) -> float:
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise CalibrationError(f"{field} must be a number") from exc
    if not math.isfinite(result):
        raise CalibrationError(f"{field} must be finite")
    return result


def _positive(value: Any, field: str) -> float:
    result = _finite(value, field)
    if result <= 0:
        raise CalibrationError(f"{field} must be > 0")
    return result


def _non_negative(value: Any, field: str) -> float:
    result = _finite(value, field)
    if result < 0:
        raise CalibrationError(f"{field} must be >= 0")
    return result


@dataclass(frozen=True)
class COPCurve:
    reference_temperature_c: float
    cop_at_reference: float
    slope_per_c: float
    minimum_cop: float = 1.0
    maximum_cop: float = 6.0

    def __post_init__(self) -> None:
        values = {
            "reference_temperature_c": self.reference_temperature_c,
            "cop_at_reference": self.cop_at_reference,
            "slope_per_c": self.slope_per_c,
            "minimum_cop": self.minimum_cop,
            "maximum_cop": self.maximum_cop,
        }
        for field, value in values.items():
            if not math.isfinite(value):
                raise CalibrationError(f"cop_curve.{field} must be finite")
        if self.minimum_cop <= 0:
            raise CalibrationError("cop_curve.minimum_cop must be > 0")
        if self.maximum_cop < self.minimum_cop:
            raise CalibrationError(
                "cop_curve.maximum_cop must be >= minimum_cop"
            )
        if not self.minimum_cop <= self.cop_at_reference <= self.maximum_cop:
            raise CalibrationError(
                "cop_curve.cop_at_reference must lie within its bounds"
            )

    def at(self, outdoor_temperature_c: float) -> float:
        raw = self.cop_at_reference + self.slope_per_c * (
            outdoor_temperature_c - self.reference_temperature_c
        )
        return min(self.maximum_cop, max(self.minimum_cop, raw))

    def to_dict(self) -> dict[str, float]:
        return {
            "reference_temperature_c": self.reference_temperature_c,
            "cop_at_reference": self.cop_at_reference,
            "slope_per_c": self.slope_per_c,
            "minimum_cop": self.minimum_cop,
            "maximum_cop": self.maximum_cop,
        }

    @classmethod
    def from_dict(cls, raw: Any) -> COPCurve:
        value = _artifact_dict(raw, "physics.cop_curve")
        return cls(
            reference_temperature_c=_finite(
                value.get("reference_temperature_c"),
                "physics.cop_curve.reference_temperature_c",
            ),
            cop_at_reference=_positive(
                value.get("cop_at_reference"),
                "physics.cop_curve.cop_at_reference",
            ),
            slope_per_c=_finite(
                value.get("slope_per_c"),
                "physics.cop_curve.slope_per_c",
            ),
            minimum_cop=_positive(
                value.get("minimum_cop"),
                "physics.cop_curve.minimum_cop",
            ),
            maximum_cop=_positive(
                value.get("maximum_cop"),
                "physics.cop_curve.maximum_cop",
            ),
        )


@dataclass(frozen=True)
class CalibrationEvidence:
    source: str
    sample_count: int
    transition_count: int
    start_timestamp_s: float
    end_timestamp_s: float
    step_s: float
    train_transition_count: int
    validation_transition_count: int
    standardized_condition_number: float
    one_step_rmse_c: float
    rollout_rmse_c: float
    persistence_rmse_c: float
    promotable: bool
    promotion_reasons: tuple[str, ...]

    def __post_init__(self) -> None:
        for field, value in (
            ("standardized_condition_number", self.standardized_condition_number),
            ("one_step_rmse_c", self.one_step_rmse_c),
            ("rollout_rmse_c", self.rollout_rmse_c),
            ("persistence_rmse_c", self.persistence_rmse_c),
        ):
            if not math.isfinite(value) or value < 0:
                raise CalibrationError(f"calibration.{field} must be finite and >= 0")
        if self.promotable and self.promotion_reasons:
            raise CalibrationError(
                "a promotable calibration cannot have promotion_reasons"
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "source": self.source,
            "sample_count": int(self.sample_count),
            "transition_count": int(self.transition_count),
            "start_timestamp_s": float(self.start_timestamp_s),
            "end_timestamp_s": float(self.end_timestamp_s),
            "step_s": float(self.step_s),
            "train_transition_count": int(self.train_transition_count),
            "validation_transition_count": int(self.validation_transition_count),
            "standardized_condition_number": float(
                self.standardized_condition_number
            ),
            "one_step_rmse_c": float(self.one_step_rmse_c),
            "rollout_rmse_c": float(self.rollout_rmse_c),
            "persistence_rmse_c": float(self.persistence_rmse_c),
            "promotable": self.promotable,
            "promotion_reasons": list(self.promotion_reasons),
        }

    @classmethod
    def from_dict(cls, raw: Any) -> CalibrationEvidence:
        value = _artifact_dict(raw, "calibration")
        reasons = value.get("promotion_reasons", [])
        if not isinstance(reasons, list) or any(
            not isinstance(reason, str) for reason in reasons
        ):
            raise CalibrationError(
                "calibration.promotion_reasons must be an array of strings"
            )
        promotable = value.get("promotable")
        if not isinstance(promotable, bool):
            raise CalibrationError("calibration.promotable must be a boolean")
        counts: dict[str, int] = {}
        for field in (
            "sample_count",
            "transition_count",
            "train_transition_count",
            "validation_transition_count",
        ):
            count = value.get(field)
            if isinstance(count, bool) or not isinstance(count, int) or count < 0:
                raise CalibrationError(f"calibration.{field} must be an integer >= 0")
            counts[field] = count
        source = value.get("source")
        if not isinstance(source, str) or not source:
            raise CalibrationError("calibration.source must be a non-empty string")
        return cls(
            source=source,
            sample_count=counts["sample_count"],
            transition_count=counts["transition_count"],
            start_timestamp_s=_finite(
                value.get("start_timestamp_s"),
                "calibration.start_timestamp_s",
            ),
            end_timestamp_s=_finite(
                value.get("end_timestamp_s"),
                "calibration.end_timestamp_s",
            ),
            step_s=_positive(value.get("step_s"), "calibration.step_s"),
            train_transition_count=counts["train_transition_count"],
            validation_transition_count=counts["validation_transition_count"],
            standardized_condition_number=_positive(
                value.get("standardized_condition_number"),
                "calibration.standardized_condition_number",
            ),
            one_step_rmse_c=_non_negative(
                value.get("one_step_rmse_c"),
                "calibration.one_step_rmse_c",
            ),
            rollout_rmse_c=_non_negative(
                value.get("rollout_rmse_c"),
                "calibration.rollout_rmse_c",
            ),
            persistence_rmse_c=_non_negative(
                value.get("persistence_rmse_c"),
                "calibration.persistence_rmse_c",
            ),
            promotable=promotable,
            promotion_reasons=tuple(reasons),
        )


@dataclass(frozen=True)
class ThermalTwinArtifact:
    model_id: str
    heat_loss_w_per_k: float
    thermal_capacity_wh_per_k: float
    cop_curve: COPCurve
    disturbance_heat_w: float
    calibration: CalibrationEvidence

    def __post_init__(self) -> None:
        if not isinstance(self.model_id, str) or not self.model_id:
            raise CalibrationError("model_id must be non-empty")
        if not math.isfinite(self.heat_loss_w_per_k) or self.heat_loss_w_per_k <= 0:
            raise CalibrationError("heat_loss_w_per_k must be finite and > 0")
        if (
            not math.isfinite(self.thermal_capacity_wh_per_k)
            or self.thermal_capacity_wh_per_k <= 0
        ):
            raise CalibrationError(
                "thermal_capacity_wh_per_k must be finite and > 0"
            )
        if not math.isfinite(self.disturbance_heat_w):
            raise CalibrationError("disturbance_heat_w must be finite")

    @property
    def revision(self) -> str:
        content = {
            "model_id": self.model_id,
            "physics": {
                "heat_loss_w_per_k": float(self.heat_loss_w_per_k),
                "thermal_capacity_wh_per_k": float(
                    self.thermal_capacity_wh_per_k
                ),
                "cop_curve": {
                    field: float(value)
                    for field, value in self.cop_curve.to_dict().items()
                },
            },
            "residual": {"constant_heat_w": float(self.disturbance_heat_w)},
            "calibration": self.calibration.to_dict(),
        }
        encoded = json.dumps(
            content,
            allow_nan=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode()
        return hashlib.sha256(encoded).hexdigest()

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": ARTIFACT_SCHEMA_VERSION,
            "kind": ARTIFACT_KIND,
            "model_type": MODEL_TYPE,
            "model_id": self.model_id,
            "revision": self.revision,
            "physics": {
                "heat_loss_w_per_k": self.heat_loss_w_per_k,
                "thermal_capacity_wh_per_k": self.thermal_capacity_wh_per_k,
                "cop_curve": self.cop_curve.to_dict(),
            },
            "residual": {"constant_heat_w": self.disturbance_heat_w},
            "calibration": self.calibration.to_dict(),
        }

    def optimizer_load(
        self,
        *,
        initial_temperature_c: float,
        minimum_temperature_c: float,
        maximum_temperature_c: float,
        outside_temperature_c: Sequence[float],
        max_electric_power_w: float,
        allowed_steps_w: Sequence[float] | None = None,
        allow_unpromotable: bool = False,
    ) -> dict[str, Any]:
        if not self.calibration.promotable and not allow_unpromotable:
            reasons = ", ".join(self.calibration.promotion_reasons)
            raise CalibrationError(
                "thermal artifact is not promotable"
                + (f": {reasons}" if reasons else "")
            )
        outside = [
            _finite(value, f"outside_temperature_c[{index}]")
            for index, value in enumerate(outside_temperature_c)
        ]
        if not outside:
            raise CalibrationError("outside_temperature_c must not be empty")
        initial = _finite(initial_temperature_c, "initial_temperature_c")
        minimum = _finite(minimum_temperature_c, "minimum_temperature_c")
        maximum = _finite(maximum_temperature_c, "maximum_temperature_c")
        if minimum >= maximum:
            raise CalibrationError(
                "minimum_temperature_c must be below maximum_temperature_c"
            )
        if not minimum <= initial <= maximum:
            raise CalibrationError(
                "initial_temperature_c must lie within the comfort bounds"
            )
        result: dict[str, Any] = {
            "id": self.model_id,
            "model_type": MODEL_TYPE,
            "source_revision": self.revision,
            "initial_temp_c": initial,
            "min_temp_c": minimum,
            "max_temp_c": maximum,
            "outside_temp_c": outside,
            "max_power_w": _positive(
                max_electric_power_w,
                "max_electric_power_w",
            ),
            "heat_loss_w_per_k": self.heat_loss_w_per_k,
            "thermal_capacity_wh_per_k": self.thermal_capacity_wh_per_k,
            "cop": [self.cop_curve.at(value) for value in outside],
            "disturbance_heat_w": [self.disturbance_heat_w] * len(outside),
        }
        if allowed_steps_w is not None:
            steps = sorted(
                {
                    _finite(value, f"allowed_steps_w[{index}]")
                    for index, value in enumerate(allowed_steps_w)
                }
            )
            if not steps or steps[0] < 0 or 0.0 not in steps:
                raise CalibrationError(
                    "allowed_steps_w must contain 0 and no negative value"
                )
            result["allowed_steps_w"] = steps
        return result

    @classmethod
    def from_dict(cls, raw: Any) -> ThermalTwinArtifact:
        value = _artifact_dict(raw, "artifact")
        if value.get("schema_version") != ARTIFACT_SCHEMA_VERSION:
            raise CalibrationError(
                "artifact.schema_version must be "
                f"{ARTIFACT_SCHEMA_VERSION}"
            )
        if value.get("kind") != ARTIFACT_KIND:
            raise CalibrationError(f"artifact.kind must be {ARTIFACT_KIND!r}")
        if value.get("model_type") != MODEL_TYPE:
            raise CalibrationError(f"artifact.model_type must be {MODEL_TYPE!r}")
        model_id = value.get("model_id")
        if not isinstance(model_id, str) or not model_id:
            raise CalibrationError("artifact.model_id must be non-empty")
        physics = _artifact_dict(value.get("physics"), "physics")
        residual = _artifact_dict(value.get("residual"), "residual")
        artifact = cls(
            model_id=model_id,
            heat_loss_w_per_k=_positive(
                physics.get("heat_loss_w_per_k"),
                "physics.heat_loss_w_per_k",
            ),
            thermal_capacity_wh_per_k=_positive(
                physics.get("thermal_capacity_wh_per_k"),
                "physics.thermal_capacity_wh_per_k",
            ),
            cop_curve=COPCurve.from_dict(physics.get("cop_curve")),
            disturbance_heat_w=_finite(
                residual.get("constant_heat_w"),
                "residual.constant_heat_w",
            ),
            calibration=CalibrationEvidence.from_dict(value.get("calibration")),
        )
        revision = value.get("revision")
        if not isinstance(revision, str) or revision != artifact.revision:
            raise CalibrationError("artifact.revision does not match its contents")
        return artifact


@dataclass(frozen=True)
class ThermalObservation:
    timestamp_s: float
    indoor_temperature_c: float
    outdoor_temperature_c: float
    heat_pump_power_w: float

    def __post_init__(self) -> None:
        for field, value in (
            ("timestamp_s", self.timestamp_s),
            ("indoor_temperature_c", self.indoor_temperature_c),
            ("outdoor_temperature_c", self.outdoor_temperature_c),
            ("heat_pump_power_w", self.heat_pump_power_w),
        ):
            if not math.isfinite(value):
                raise CalibrationError(f"{field} must be finite")
        if self.heat_pump_power_w < 0:
            raise CalibrationError(
                "heat_pump_power_w must be >= 0 under the FTW site convention"
            )


@dataclass(frozen=True)
class ThermalTransitionCoefficients:
    state: np.ndarray
    outside: np.ndarray
    power: np.ndarray
    offset: np.ndarray


def _artifact_dict(value: Any, field: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise CalibrationError(f"{field} must be an object")
    return value


def _optimizer_scalar_or_vector(
    value: Any,
    n: int,
    field: str,
    *,
    positive: bool = False,
) -> np.ndarray:
    validate = positive_number if positive else finite_number
    if isinstance(value, list):
        items = require_list(value, field)
        if len(items) != n:
            raise ProtocolError(f"{field} must have {n} entries")
        return np.asarray(
            [validate(item, f"{field}[{index}]") for index, item in enumerate(items)]
        )
    number = validate(value, field)
    return np.full(n, number)


def thermal_transition_coefficients(
    spec: dict[str, Any],
    dt_h: np.ndarray,
    outside_temperature_c: np.ndarray,
) -> ThermalTransitionCoefficients:
    n = len(dt_h)
    model_type = spec.get("model_type")
    physical_fields = {
        "heat_loss_w_per_k",
        "thermal_capacity_wh_per_k",
        "cop",
        "cop_curve",
        "disturbance_heat_w",
    }
    legacy_fields = {"gain_c_per_kwh", "loss_per_hour"}
    uses_physics = model_type is not None or any(
        field in spec for field in physical_fields
    )
    if uses_physics:
        if model_type != MODEL_TYPE:
            raise ProtocolError(
                f"thermal model_type must be {MODEL_TYPE!r}"
            )
        mixed = sorted(field for field in legacy_fields if field in spec)
        if mixed:
            raise ProtocolError(
                "physical thermal model cannot also set " + ", ".join(mixed)
            )
        heat_loss = positive_number(
            spec.get("heat_loss_w_per_k"),
            "thermal_loads[].heat_loss_w_per_k",
        )
        capacity = positive_number(
            spec.get("thermal_capacity_wh_per_k"),
            "thermal_loads[].thermal_capacity_wh_per_k",
        )
        cop_value = spec.get("cop")
        if cop_value is None:
            curve = require_dict(
                spec.get("cop_curve"),
                "thermal_loads[].cop_curve",
            )
            reference = finite_number(
                curve.get("reference_temperature_c"),
                "thermal_loads[].cop_curve.reference_temperature_c",
            )
            reference_cop = positive_number(
                curve.get("cop_at_reference"),
                "thermal_loads[].cop_curve.cop_at_reference",
            )
            slope = finite_number(
                curve.get("slope_per_c"),
                "thermal_loads[].cop_curve.slope_per_c",
            )
            minimum = positive_number(
                curve.get("minimum_cop"),
                "thermal_loads[].cop_curve.minimum_cop",
            )
            maximum = positive_number(
                curve.get("maximum_cop"),
                "thermal_loads[].cop_curve.maximum_cop",
            )
            if maximum < minimum:
                raise ProtocolError(
                    "thermal_loads[].cop_curve maximum must be >= minimum"
                )
            cop = np.clip(
                reference_cop + slope * (outside_temperature_c - reference),
                minimum,
                maximum,
            )
        else:
            cop = _optimizer_scalar_or_vector(
                cop_value,
                n,
                "thermal_loads[].cop",
                positive=True,
            )
        disturbance = _optimizer_scalar_or_vector(
            spec.get("disturbance_heat_w", 0.0),
            n,
            "thermal_loads[].disturbance_heat_w",
        )
        decay = np.exp(-(heat_loss / capacity) * dt_h)
        outside = 1.0 - decay
        return ThermalTransitionCoefficients(
            state=decay,
            outside=outside,
            power=outside * cop / heat_loss,
            offset=outside * disturbance / heat_loss,
        )

    gain = positive_number(
        spec.get("gain_c_per_kwh"),
        "thermal_loads[].gain_c_per_kwh",
    )
    loss = max(
        0.0,
        finite_number(
            spec.get("loss_per_hour", 0),
            "thermal_loads[].loss_per_hour",
        ),
    )
    return ThermalTransitionCoefficients(
        state=1.0 - loss * dt_h,
        outside=loss * dt_h,
        power=gain * dt_h / 1000.0,
        offset=np.zeros(n),
    )


def thermal_step(
    artifact: ThermalTwinArtifact,
    indoor_temperature_c: float,
    outdoor_temperature_c: float,
    heat_pump_power_w: float,
    step_h: float,
) -> float:
    indoor = _finite(indoor_temperature_c, "indoor_temperature_c")
    outdoor = _finite(outdoor_temperature_c, "outdoor_temperature_c")
    power = _finite(heat_pump_power_w, "heat_pump_power_w")
    duration = _positive(step_h, "step_h")
    if power < 0:
        raise CalibrationError(
            "heat_pump_power_w must be >= 0 under the FTW site convention"
        )
    loss_rate = (
        artifact.heat_loss_w_per_k / artifact.thermal_capacity_wh_per_k
    )
    decay = math.exp(-loss_rate * duration)
    heat_w = artifact.cop_curve.at(outdoor) * power + artifact.disturbance_heat_w
    equilibrium_c = outdoor + heat_w / artifact.heat_loss_w_per_k
    return decay * indoor + (1.0 - decay) * equilibrium_c


def simulate_thermal_twin(
    artifact: ThermalTwinArtifact,
    *,
    initial_temperature_c: float,
    outside_temperature_c: Sequence[float],
    heat_pump_power_w: Sequence[float],
    step_h: float,
) -> list[float]:
    if len(outside_temperature_c) != len(heat_pump_power_w):
        raise CalibrationError(
            "outside_temperature_c and heat_pump_power_w must have equal lengths"
        )
    temperatures = [_finite(initial_temperature_c, "initial_temperature_c")]
    for index, (outdoor, power) in enumerate(
        zip(outside_temperature_c, heat_pump_power_w, strict=True)
    ):
        temperatures.append(
            thermal_step(
                artifact,
                temperatures[-1],
                _finite(outdoor, f"outside_temperature_c[{index}]"),
                _finite(power, f"heat_pump_power_w[{index}]"),
                step_h,
            )
        )
    return temperatures


def site_grid_power_w(
    *,
    native_load_w: float,
    heat_pump_power_w: float,
    pv_w: float = 0.0,
    battery_w: float = 0.0,
) -> float:
    native = _finite(native_load_w, "native_load_w")
    heat_pump = _finite(heat_pump_power_w, "heat_pump_power_w")
    pv = _finite(pv_w, "pv_w")
    battery = _finite(battery_w, "battery_w")
    if native < 0 or heat_pump < 0 or pv > 0:
        raise CalibrationError(
            "site convention requires loads >= 0 and pv_w <= 0"
        )
    return native + heat_pump + pv + battery


def _box_least_squares(
    matrix: np.ndarray,
    target: np.ndarray,
    bounds: Sequence[tuple[float, float]],
) -> tuple[np.ndarray, float]:
    best_parameters: np.ndarray | None = None
    best_error = math.inf
    tolerance = 1e-10
    for states in itertools.product(("free", "low", "high"), repeat=len(bounds)):
        parameters = np.zeros(len(bounds))
        free: list[int] = []
        fixed: list[int] = []
        for index, (state, (lower, upper)) in enumerate(zip(states, bounds, strict=True)):
            if state == "free":
                free.append(index)
            else:
                fixed.append(index)
                parameters[index] = lower if state == "low" else upper
        adjusted = target.copy()
        if fixed:
            adjusted -= matrix[:, fixed] @ parameters[fixed]
        if free:
            values, _, _, _ = np.linalg.lstsq(
                matrix[:, free],
                adjusted,
                rcond=None,
            )
            parameters[free] = values
        if any(
            parameters[index] < lower - tolerance
            or parameters[index] > upper + tolerance
            for index, (lower, upper) in enumerate(bounds)
        ):
            continue
        residual = matrix @ parameters - target
        error = float(residual @ residual)
        if error < best_error:
            best_parameters = parameters
            best_error = error
    if best_parameters is None:
        raise CalibrationError("bounded thermal fit found no feasible parameters")
    return best_parameters, best_error


def _rmse(actual: np.ndarray, predicted: np.ndarray) -> float:
    if len(actual) == 0 or len(actual) != len(predicted):
        raise CalibrationError("RMSE inputs must have equal non-zero lengths")
    return float(np.sqrt(np.mean(np.square(actual - predicted))))


def calibrate_thermal_twin(
    observations: Iterable[ThermalObservation],
    *,
    model_id: str,
    cop_curve: COPCurve,
    train_fraction: float = 0.75,
    source: str = "heat_pump_submeter",
) -> ThermalTwinArtifact:
    values = list(observations)
    if len(values) - 1 < MIN_TRANSITIONS:
        raise CalibrationError(
            f"thermal calibration needs at least {MIN_TRANSITIONS + 1} samples"
        )
    if not 0.5 <= train_fraction <= 0.9:
        raise CalibrationError("train_fraction must be between 0.5 and 0.9")
    if source not in {"heat_pump_submeter", "validated_component_balance"}:
        raise CalibrationError(
            "source must be heat_pump_submeter or validated_component_balance"
        )
    timestamps = np.asarray([value.timestamp_s for value in values])
    steps_s = np.diff(timestamps)
    if np.any(steps_s <= 0):
        raise CalibrationError("timestamps must be strictly increasing")
    step_s = float(np.median(steps_s))
    tolerance_s = max(1.0, step_s * 0.02)
    if np.max(np.abs(steps_s - step_s)) > tolerance_s:
        raise CalibrationError(
            "samples must use one regular interval; split gaps into separate runs"
        )
    step_h = step_s / 3600.0
    indoor = np.asarray([value.indoor_temperature_c for value in values])
    outdoor = np.asarray([value.outdoor_temperature_c for value in values[:-1]])
    electric = np.asarray([value.heat_pump_power_w for value in values[:-1]])
    thermal = np.asarray(
        [
            cop_curve.at(value.outdoor_temperature_c)
            * value.heat_pump_power_w
            for value in values[:-1]
        ]
    )
    target = indoor[1:] - indoor[:-1]
    matrix = np.column_stack(
        (
            outdoor - indoor[:-1],
            thermal,
            np.ones(len(target)),
        )
    )
    transition_count = len(target)
    train_count = int(transition_count * train_fraction)
    train_count = min(transition_count - 8, max(32, train_count))
    if train_count <= 0 or transition_count - train_count < 8:
        raise CalibrationError("calibration needs at least eight validation transitions")
    train_matrix = matrix[:train_count]
    train_target = target[:train_count]
    feature_scale = np.std(train_matrix[:, :2], axis=0)
    if feature_scale[0] < 0.05:
        raise CalibrationError(
            "indoor-outdoor temperature difference has too little excitation"
        )
    if feature_scale[1] < 50.0:
        raise CalibrationError(
            "heat-pump thermal input has too little excitation"
        )
    standardized = np.column_stack(
        (
            (train_matrix[:, 0] - np.mean(train_matrix[:, 0])) / feature_scale[0],
            (train_matrix[:, 1] - np.mean(train_matrix[:, 1])) / feature_scale[1],
            np.ones(train_count),
        )
    )
    if np.linalg.matrix_rank(standardized) < 3:
        raise CalibrationError(
            "thermal parameters are not identifiable from these samples"
        )
    condition_number = float(np.linalg.cond(standardized))
    if not math.isfinite(condition_number) or condition_number > 1_000:
        raise CalibrationError(
            "thermal inputs are too collinear to identify a stable model"
        )
    parameter_bounds = (
        (1e-6, 0.8),
        (1e-9, 0.02),
        (-2.0, 2.0),
    )
    parameters, _ = _box_least_squares(
        train_matrix,
        train_target,
        parameter_bounds,
    )
    loss_fraction, heat_gain_k_per_w, residual_k = parameters
    if not 0 < loss_fraction < 1 or heat_gain_k_per_w <= 0:
        raise CalibrationError("thermal fit did not yield a stable physical model")
    decay = 1.0 - loss_fraction
    loss_per_h = -math.log(decay) / step_h
    heat_loss_w_per_k = loss_fraction / heat_gain_k_per_w
    thermal_capacity_wh_per_k = heat_loss_w_per_k / loss_per_h
    disturbance_heat_w = residual_k / heat_gain_k_per_w
    if not 5.0 <= heat_loss_w_per_k <= 5_000.0:
        raise CalibrationError(
            "fitted heat loss is outside the physical guard 5..5000 W/K"
        )
    if not 200.0 <= thermal_capacity_wh_per_k <= 1_000_000.0:
        raise CalibrationError(
            "fitted thermal capacity is outside the physical guard "
            "200..1000000 Wh/K"
        )
    if abs(disturbance_heat_w) > 20_000:
        raise CalibrationError(
            "fitted residual heat exceeds the physical guard of 20 kW"
        )

    provisional_evidence = CalibrationEvidence(
        source=source,
        sample_count=len(values),
        transition_count=transition_count,
        start_timestamp_s=float(timestamps[0]),
        end_timestamp_s=float(timestamps[-1]),
        step_s=step_s,
        train_transition_count=train_count,
        validation_transition_count=transition_count - train_count,
        standardized_condition_number=condition_number,
        one_step_rmse_c=0.0,
        rollout_rmse_c=0.0,
        persistence_rmse_c=0.0,
        promotable=False,
        promotion_reasons=(),
    )
    provisional = ThermalTwinArtifact(
        model_id=model_id,
        heat_loss_w_per_k=heat_loss_w_per_k,
        thermal_capacity_wh_per_k=thermal_capacity_wh_per_k,
        cop_curve=cop_curve,
        disturbance_heat_w=disturbance_heat_w,
        calibration=provisional_evidence,
    )
    validation_indices = range(train_count, transition_count)
    one_step = np.asarray(
        [
            thermal_step(
                provisional,
                indoor[index],
                outdoor[index],
                electric[index],
                step_h,
            )
            for index in validation_indices
        ]
    )
    validation_actual = indoor[train_count + 1 :]
    rollout_values: list[float] = []
    rollout_temperature = float(indoor[train_count])
    for index in validation_indices:
        rollout_temperature = thermal_step(
            provisional,
            rollout_temperature,
            outdoor[index],
            electric[index],
            step_h,
        )
        rollout_values.append(rollout_temperature)
    one_step_rmse = _rmse(validation_actual, one_step)
    rollout_rmse = _rmse(validation_actual, np.asarray(rollout_values))
    persistence_rmse = _rmse(
        validation_actual,
        indoor[train_count:-1],
    )
    promotion_reasons: list[str] = []
    duration_h = (timestamps[-1] - timestamps[0]) / 3600.0
    if duration_h < 72:
        promotion_reasons.append("less than 72 hours of evidence")
    if condition_number > 100:
        promotion_reasons.append("standardized condition number exceeds 100")
    if one_step_rmse > 0.5:
        promotion_reasons.append("one-step validation RMSE exceeds 0.5 C")
    if rollout_rmse > 1.0:
        promotion_reasons.append("rollout validation RMSE exceeds 1.0 C")
    if one_step_rmse >= persistence_rmse:
        promotion_reasons.append(
            "one-step validation does not beat temperature persistence"
        )
    for index, (lower, upper) in enumerate(parameter_bounds):
        if math.isclose(parameters[index], lower, rel_tol=0, abs_tol=1e-8) or math.isclose(
            parameters[index],
            upper,
            rel_tol=0,
            abs_tol=1e-8,
        ):
            promotion_reasons.append("a fitted parameter reached its search bound")
            break
    evidence = CalibrationEvidence(
        source=source,
        sample_count=len(values),
        transition_count=transition_count,
        start_timestamp_s=float(timestamps[0]),
        end_timestamp_s=float(timestamps[-1]),
        step_s=step_s,
        train_transition_count=train_count,
        validation_transition_count=transition_count - train_count,
        standardized_condition_number=condition_number,
        one_step_rmse_c=one_step_rmse,
        rollout_rmse_c=rollout_rmse,
        persistence_rmse_c=persistence_rmse,
        promotable=not promotion_reasons,
        promotion_reasons=tuple(promotion_reasons),
    )
    return ThermalTwinArtifact(
        model_id=model_id,
        heat_loss_w_per_k=heat_loss_w_per_k,
        thermal_capacity_wh_per_k=thermal_capacity_wh_per_k,
        cop_curve=cop_curve,
        disturbance_heat_w=disturbance_heat_w,
        calibration=evidence,
    )


def _parse_timestamp(value: str, row_number: int) -> float:
    try:
        return float(value)
    except ValueError:
        try:
            parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError as exc:
            raise CalibrationError(
                f"row {row_number}: timestamp must be Unix seconds or ISO 8601"
            ) from exc
        if parsed.tzinfo is None:
            raise CalibrationError(
                f"row {row_number}: ISO timestamp must include a time zone"
            )
        return parsed.timestamp()


def load_observations_csv(path: str | Path) -> list[ThermalObservation]:
    source_path = Path(path)
    with source_path.open(newline="", encoding="utf-8") as source:
        reader = csv.DictReader(source)
        fields = set(reader.fieldnames or [])
        timestamp_field = "timestamp_s" if "timestamp_s" in fields else "timestamp"
        required = {
            timestamp_field,
            "indoor_temp_c",
            "outdoor_temp_c",
            "heat_pump_power_w",
        }
        missing = sorted(required - fields)
        if missing:
            if "heat_pump_power_w" in missing and "grid_power_w" in fields:
                raise CalibrationError(
                    "grid_power_w alone cannot identify heat-pump input; "
                    "provide heat_pump_power_w from a submeter or a validated "
                    "component balance"
                )
            raise CalibrationError(
                "CSV is missing required columns: " + ", ".join(missing)
            )
        observations: list[ThermalObservation] = []
        for row_number, row in enumerate(reader, start=2):
            observations.append(
                ThermalObservation(
                    timestamp_s=_parse_timestamp(
                        row[timestamp_field],
                        row_number,
                    ),
                    indoor_temperature_c=_finite(
                        row["indoor_temp_c"],
                        f"row {row_number} indoor_temp_c",
                    ),
                    outdoor_temperature_c=_finite(
                        row["outdoor_temp_c"],
                        f"row {row_number} outdoor_temp_c",
                    ),
                    heat_pump_power_w=_finite(
                        row["heat_pump_power_w"],
                        f"row {row_number} heat_pump_power_w",
                    ),
                )
            )
    return observations


def load_artifact(path: str | Path) -> ThermalTwinArtifact:
    try:
        raw = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise CalibrationError(f"cannot read thermal artifact: {exc}") from exc
    return ThermalTwinArtifact.from_dict(raw)


def _write_artifact(
    artifact: ThermalTwinArtifact,
    output: str,
) -> None:
    encoded = json.dumps(
        artifact.to_dict(),
        allow_nan=False,
        indent=2,
        sort_keys=True,
    )
    if output == "-":
        print(encoded)
        return
    Path(output).write_text(encoded + "\n", encoding="utf-8")


def calibration_main(argv: Sequence[str] | None = None) -> None:
    parser = argparse.ArgumentParser(
        description="Calibrate an FTW 1R1C thermal twin from regular telemetry"
    )
    parser.add_argument("csv", help="telemetry CSV")
    parser.add_argument("--model-id", required=True)
    parser.add_argument("--output", default="-", help="artifact JSON path or -")
    parser.add_argument("--cop-reference-temp-c", type=float, default=7.0)
    parser.add_argument("--cop-at-reference", type=float, required=True)
    parser.add_argument("--cop-slope-per-c", type=float, default=0.06)
    parser.add_argument("--minimum-cop", type=float, default=1.0)
    parser.add_argument("--maximum-cop", type=float, default=6.0)
    parser.add_argument("--train-fraction", type=float, default=0.75)
    parser.add_argument(
        "--power-source",
        choices=("heat_pump_submeter", "validated_component_balance"),
        default="heat_pump_submeter",
    )
    args = parser.parse_args(argv)
    try:
        artifact = calibrate_thermal_twin(
            load_observations_csv(args.csv),
            model_id=args.model_id,
            cop_curve=COPCurve(
                reference_temperature_c=args.cop_reference_temp_c,
                cop_at_reference=args.cop_at_reference,
                slope_per_c=args.cop_slope_per_c,
                minimum_cop=args.minimum_cop,
                maximum_cop=args.maximum_cop,
            ),
            train_fraction=args.train_fraction,
            source=args.power_source,
        )
        _write_artifact(artifact, args.output)
    except CalibrationError as exc:
        print(f"thermal calibration: {exc}", file=sys.stderr)
        raise SystemExit(2) from exc


if __name__ == "__main__":
    calibration_main()
