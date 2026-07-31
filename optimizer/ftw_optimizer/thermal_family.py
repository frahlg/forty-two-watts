from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from typing import Any, Iterable, Sequence

import numpy as np
from scipy.linalg import expm
from scipy.optimize import least_squares

from .home_spec import (
    TWO_R_TWO_C_MODEL_TYPE,
    HomeSpec,
    ThermalPriors,
)
from .protocol import (
    ProtocolError,
    finite_number,
    positive_number,
    require_dict,
)
from .thermal_twin import (
    ARTIFACT_KIND,
    ARTIFACT_SCHEMA_VERSION,
    MIN_TRANSITIONS,
    MODEL_TYPE,
    COPCurve,
    CalibrationError,
    CalibrationEvidence,
    ThermalObservation,
    ThermalTwinArtifact,
    calibrate_thermal_twin,
)


@dataclass(frozen=True)
class TwoR2CArtifact:
    model_id: str
    heat_loss_w_per_k: float
    mass_coupling_w_per_k: float
    air_capacity_wh_per_k: float
    mass_capacity_wh_per_k: float
    cop_curve: COPCurve
    disturbance_heat_w: float
    calibration: CalibrationEvidence

    def __post_init__(self) -> None:
        if not isinstance(self.model_id, str) or not self.model_id:
            raise CalibrationError("model_id must be non-empty")
        for field, value in (
            ("heat_loss_w_per_k", self.heat_loss_w_per_k),
            ("mass_coupling_w_per_k", self.mass_coupling_w_per_k),
            ("air_capacity_wh_per_k", self.air_capacity_wh_per_k),
            ("mass_capacity_wh_per_k", self.mass_capacity_wh_per_k),
        ):
            if not math.isfinite(value) or value <= 0:
                raise CalibrationError(f"{field} must be finite and > 0")
        if not math.isfinite(self.disturbance_heat_w):
            raise CalibrationError("disturbance_heat_w must be finite")

    @property
    def revision(self) -> str:
        encoded = json.dumps(
            self._content(),
            allow_nan=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode()
        return hashlib.sha256(encoded).hexdigest()

    def _content(self) -> dict[str, Any]:
        return {
            "model_id": self.model_id,
            "physics": {
                "heat_loss_w_per_k": float(self.heat_loss_w_per_k),
                "mass_coupling_w_per_k": float(
                    self.mass_coupling_w_per_k
                ),
                "air_capacity_wh_per_k": float(
                    self.air_capacity_wh_per_k
                ),
                "mass_capacity_wh_per_k": float(
                    self.mass_capacity_wh_per_k
                ),
                "cop_curve": {
                    field: float(value)
                    for field, value in self.cop_curve.to_dict().items()
                },
            },
            "residual": {
                "constant_heat_w": float(self.disturbance_heat_w)
            },
            "calibration": self.calibration.to_dict(),
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": ARTIFACT_SCHEMA_VERSION,
            "kind": ARTIFACT_KIND,
            "model_type": TWO_R_TWO_C_MODEL_TYPE,
            "revision": self.revision,
            **self._content(),
        }

    @classmethod
    def from_dict(cls, raw: Any) -> TwoR2CArtifact:
        if not isinstance(raw, dict):
            raise CalibrationError("artifact must be an object")
        if raw.get("schema_version") != ARTIFACT_SCHEMA_VERSION:
            raise CalibrationError(
                "artifact.schema_version must be "
                f"{ARTIFACT_SCHEMA_VERSION}"
            )
        if raw.get("kind") != ARTIFACT_KIND:
            raise CalibrationError(
                f"artifact.kind must be {ARTIFACT_KIND!r}"
            )
        if raw.get("model_type") != TWO_R_TWO_C_MODEL_TYPE:
            raise CalibrationError(
                "artifact.model_type must be "
                f"{TWO_R_TWO_C_MODEL_TYPE!r}"
            )
        physics = raw.get("physics")
        residual = raw.get("residual")
        if not isinstance(physics, dict) or not isinstance(residual, dict):
            raise CalibrationError(
                "artifact physics and residual must be objects"
            )
        artifact = cls(
            model_id=_artifact_string(raw.get("model_id"), "model_id"),
            heat_loss_w_per_k=_artifact_positive(
                physics.get("heat_loss_w_per_k"),
                "physics.heat_loss_w_per_k",
            ),
            mass_coupling_w_per_k=_artifact_positive(
                physics.get("mass_coupling_w_per_k"),
                "physics.mass_coupling_w_per_k",
            ),
            air_capacity_wh_per_k=_artifact_positive(
                physics.get("air_capacity_wh_per_k"),
                "physics.air_capacity_wh_per_k",
            ),
            mass_capacity_wh_per_k=_artifact_positive(
                physics.get("mass_capacity_wh_per_k"),
                "physics.mass_capacity_wh_per_k",
            ),
            cop_curve=COPCurve.from_dict(physics.get("cop_curve")),
            disturbance_heat_w=_artifact_finite(
                residual.get("constant_heat_w"),
                "residual.constant_heat_w",
            ),
            calibration=CalibrationEvidence.from_dict(
                raw.get("calibration")
            ),
        )
        if raw.get("revision") != artifact.revision:
            raise CalibrationError(
                "artifact.revision does not match its contents"
            )
        return artifact

    def optimizer_load(
        self,
        *,
        initial_temperature_c: float,
        minimum_temperature_c: float,
        maximum_temperature_c: float,
        outside_temperature_c: Sequence[float],
        max_electric_power_w: float,
        initial_mass_temperature_c: float | None = None,
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
            _artifact_finite(
                value,
                f"outside_temperature_c[{index}]",
            )
            for index, value in enumerate(outside_temperature_c)
        ]
        if not outside:
            raise CalibrationError(
                "outside_temperature_c must not be empty"
            )
        initial = _artifact_finite(
            initial_temperature_c,
            "initial_temperature_c",
        )
        initial_mass = (
            initial
            if initial_mass_temperature_c is None
            else _artifact_finite(
                initial_mass_temperature_c,
                "initial_mass_temperature_c",
            )
        )
        minimum = _artifact_finite(
            minimum_temperature_c,
            "minimum_temperature_c",
        )
        maximum = _artifact_finite(
            maximum_temperature_c,
            "maximum_temperature_c",
        )
        if minimum >= maximum:
            raise CalibrationError(
                "minimum_temperature_c must be below "
                "maximum_temperature_c"
            )
        if not minimum <= initial <= maximum:
            raise CalibrationError(
                "initial_temperature_c must lie within the comfort bounds"
            )
        result: dict[str, Any] = {
            "id": self.model_id,
            "model_type": TWO_R_TWO_C_MODEL_TYPE,
            "source_revision": self.revision,
            "initial_temp_c": initial,
            "initial_mass_temp_c": initial_mass,
            "min_temp_c": minimum,
            "max_temp_c": maximum,
            "outside_temp_c": outside,
            "max_power_w": _artifact_positive(
                max_electric_power_w,
                "max_electric_power_w",
            ),
            "heat_loss_w_per_k": self.heat_loss_w_per_k,
            "mass_coupling_w_per_k": self.mass_coupling_w_per_k,
            "air_capacity_wh_per_k": self.air_capacity_wh_per_k,
            "mass_capacity_wh_per_k": self.mass_capacity_wh_per_k,
            "cop": [self.cop_curve.at(value) for value in outside],
            "disturbance_heat_w": [self.disturbance_heat_w] * len(outside),
        }
        if allowed_steps_w is not None:
            steps = sorted(
                {
                    _artifact_finite(
                        value,
                        f"allowed_steps_w[{index}]",
                    )
                    for index, value in enumerate(allowed_steps_w)
                }
            )
            if not steps or steps[0] < 0 or 0.0 not in steps:
                raise CalibrationError(
                    "allowed_steps_w must contain 0 and no negative value"
                )
            result["allowed_steps_w"] = steps
        return result


ThermalArtifact = ThermalTwinArtifact | TwoR2CArtifact


@dataclass(frozen=True)
class TwoR2CTransitionCoefficients:
    state: np.ndarray
    outside: np.ndarray
    power: np.ndarray
    offset: np.ndarray


@dataclass(frozen=True)
class CandidateSummary:
    model_type: str
    complexity: int
    status: str
    promotable: bool
    one_step_rmse_c: float | None
    rollout_rmse_c: float | None
    persistence_rmse_c: float | None
    reasons: tuple[str, ...]
    revision: str | None

    def to_dict(self) -> dict[str, Any]:
        return {
            "model_type": self.model_type,
            "complexity": self.complexity,
            "status": self.status,
            "promotable": self.promotable,
            "one_step_rmse_c": self.one_step_rmse_c,
            "rollout_rmse_c": self.rollout_rmse_c,
            "persistence_rmse_c": self.persistence_rmse_c,
            "reasons": list(self.reasons),
            "revision": self.revision,
        }


@dataclass(frozen=True)
class ModelSelectionReport:
    home_spec_revision: str
    sample_count: int
    start_timestamp_s: float
    end_timestamp_s: float
    champion_model_type: str | None
    champion_revision: str | None
    decision: str
    candidates: tuple[CandidateSummary, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "kind": "ftw.thermal_model_selection",
            "home_spec_revision": self.home_spec_revision,
            "sample_count": self.sample_count,
            "start_timestamp_s": self.start_timestamp_s,
            "end_timestamp_s": self.end_timestamp_s,
            "champion_model_type": self.champion_model_type,
            "champion_revision": self.champion_revision,
            "decision": self.decision,
            "candidates": [
                candidate.to_dict() for candidate in self.candidates
            ],
        }


@dataclass(frozen=True)
class FamilyCalibrationResult:
    report: ModelSelectionReport
    artifacts: dict[str, ThermalArtifact]

    @property
    def champion(self) -> ThermalArtifact | None:
        model_type = self.report.champion_model_type
        return None if model_type is None else self.artifacts[model_type]


def _artifact_finite(value: Any, field: str) -> float:
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise CalibrationError(f"{field} must be a number") from exc
    if not math.isfinite(result):
        raise CalibrationError(f"{field} must be finite")
    return result


def _artifact_positive(value: Any, field: str) -> float:
    result = _artifact_finite(value, field)
    if result <= 0:
        raise CalibrationError(f"{field} must be > 0")
    return result


def _artifact_string(value: Any, field: str) -> str:
    if not isinstance(value, str) or not value:
        raise CalibrationError(f"{field} must be non-empty")
    return value


def artifact_from_dict(raw: Any) -> ThermalArtifact:
    if not isinstance(raw, dict):
        raise CalibrationError("artifact must be an object")
    model_type = raw.get("model_type")
    if model_type == MODEL_TYPE:
        return ThermalTwinArtifact.from_dict(raw)
    if model_type == TWO_R_TWO_C_MODEL_TYPE:
        return TwoR2CArtifact.from_dict(raw)
    raise CalibrationError(f"unsupported thermal model_type {model_type!r}")


def _two_r2c_discrete(
    *,
    heat_loss_w_per_k: float,
    mass_coupling_w_per_k: float,
    air_capacity_wh_per_k: float,
    mass_capacity_wh_per_k: float,
    step_h: float,
) -> tuple[np.ndarray, np.ndarray]:
    continuous = np.asarray(
        [
            [
                -(heat_loss_w_per_k + mass_coupling_w_per_k)
                / air_capacity_wh_per_k,
                mass_coupling_w_per_k / air_capacity_wh_per_k,
            ],
            [
                mass_coupling_w_per_k / mass_capacity_wh_per_k,
                -mass_coupling_w_per_k / mass_capacity_wh_per_k,
            ],
        ],
        dtype=float,
    )
    inputs = np.asarray(
        [
            [
                heat_loss_w_per_k / air_capacity_wh_per_k,
                1.0 / air_capacity_wh_per_k,
            ],
            [0.0, 0.0],
        ],
        dtype=float,
    )
    augmented = np.zeros((4, 4))
    augmented[:2, :2] = continuous
    augmented[:2, 2:] = inputs
    discrete = expm(augmented * step_h)
    return discrete[:2, :2], discrete[:2, 2:]


def two_r2c_step(
    artifact: TwoR2CArtifact,
    state_temperature_c: Sequence[float],
    outdoor_temperature_c: float,
    heat_pump_power_w: float,
    step_h: float,
) -> tuple[float, float]:
    if len(state_temperature_c) != 2:
        raise CalibrationError(
            "2R2C state must contain air and mass temperature"
        )
    state = np.asarray(
        [
            _artifact_finite(
                state_temperature_c[0],
                "air_temperature_c",
            ),
            _artifact_finite(
                state_temperature_c[1],
                "mass_temperature_c",
            ),
        ]
    )
    outdoor = _artifact_finite(
        outdoor_temperature_c,
        "outdoor_temperature_c",
    )
    power = _artifact_finite(
        heat_pump_power_w,
        "heat_pump_power_w",
    )
    duration = _artifact_positive(step_h, "step_h")
    if power < 0:
        raise CalibrationError(
            "heat_pump_power_w must be >= 0 under the FTW site convention"
        )
    state_matrix, input_matrix = _two_r2c_discrete(
        heat_loss_w_per_k=artifact.heat_loss_w_per_k,
        mass_coupling_w_per_k=artifact.mass_coupling_w_per_k,
        air_capacity_wh_per_k=artifact.air_capacity_wh_per_k,
        mass_capacity_wh_per_k=artifact.mass_capacity_wh_per_k,
        step_h=duration,
    )
    heat_w = (
        artifact.cop_curve.at(outdoor) * power
        + artifact.disturbance_heat_w
    )
    result = (
        state_matrix @ state
        + input_matrix[:, 0] * outdoor
        + input_matrix[:, 1] * heat_w
    )
    return float(result[0]), float(result[1])


def simulate_two_r2c(
    artifact: TwoR2CArtifact,
    *,
    initial_air_temperature_c: float,
    initial_mass_temperature_c: float,
    outside_temperature_c: Sequence[float],
    heat_pump_power_w: Sequence[float],
    step_h: float,
) -> list[tuple[float, float]]:
    if len(outside_temperature_c) != len(heat_pump_power_w):
        raise CalibrationError(
            "outside_temperature_c and heat_pump_power_w "
            "must have equal lengths"
        )
    states = [
        (
            _artifact_finite(
                initial_air_temperature_c,
                "initial_air_temperature_c",
            ),
            _artifact_finite(
                initial_mass_temperature_c,
                "initial_mass_temperature_c",
            ),
        )
    ]
    for outdoor, power in zip(
        outside_temperature_c,
        heat_pump_power_w,
        strict=True,
    ):
        states.append(
            two_r2c_step(
                artifact,
                states[-1],
                outdoor,
                power,
                step_h,
            )
        )
    return states


def _optimizer_vector(
    value: Any,
    length: int,
    field: str,
    *,
    positive: bool = False,
) -> np.ndarray:
    validate = positive_number if positive else finite_number
    if isinstance(value, list):
        if len(value) != length:
            raise ProtocolError(f"{field} must have {length} entries")
        return np.asarray(
            [
                validate(item, f"{field}[{index}]")
                for index, item in enumerate(value)
            ]
        )
    return np.full(length, validate(value, field))


def _optimizer_cop(
    spec: dict[str, Any],
    outside_temperature_c: np.ndarray,
) -> np.ndarray:
    length = len(outside_temperature_c)
    if spec.get("cop") is not None:
        return _optimizer_vector(
            spec.get("cop"),
            length,
            "thermal_loads[].cop",
            positive=True,
        )
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
    return np.clip(
        reference_cop
        + slope * (outside_temperature_c - reference),
        minimum,
        maximum,
    )


def two_r2c_transition_coefficients(
    spec: dict[str, Any],
    dt_h: np.ndarray,
    outside_temperature_c: np.ndarray,
) -> TwoR2CTransitionCoefficients:
    if spec.get("model_type") != TWO_R_TWO_C_MODEL_TYPE:
        raise ProtocolError(
            "thermal model_type must be "
            f"{TWO_R_TWO_C_MODEL_TYPE!r}"
        )
    for legacy in (
        "gain_c_per_kwh",
        "loss_per_hour",
        "thermal_capacity_wh_per_k",
    ):
        if legacy in spec:
            raise ProtocolError(
                f"2R2C thermal model cannot also set {legacy}"
            )
    heat_loss = positive_number(
        spec.get("heat_loss_w_per_k"),
        "thermal_loads[].heat_loss_w_per_k",
    )
    coupling = positive_number(
        spec.get("mass_coupling_w_per_k"),
        "thermal_loads[].mass_coupling_w_per_k",
    )
    air_capacity = positive_number(
        spec.get("air_capacity_wh_per_k"),
        "thermal_loads[].air_capacity_wh_per_k",
    )
    mass_capacity = positive_number(
        spec.get("mass_capacity_wh_per_k"),
        "thermal_loads[].mass_capacity_wh_per_k",
    )
    cop = _optimizer_cop(spec, outside_temperature_c)
    disturbance = _optimizer_vector(
        spec.get("disturbance_heat_w", 0),
        len(dt_h),
        "thermal_loads[].disturbance_heat_w",
    )
    state = np.zeros((len(dt_h), 2, 2))
    outside = np.zeros((len(dt_h), 2))
    power = np.zeros((len(dt_h), 2))
    offset = np.zeros((len(dt_h), 2))
    cache: dict[float, tuple[np.ndarray, np.ndarray]] = {}
    for index, duration in enumerate(dt_h):
        duration_key = float(duration)
        matrices = cache.get(duration_key)
        if matrices is None:
            matrices = _two_r2c_discrete(
                heat_loss_w_per_k=heat_loss,
                mass_coupling_w_per_k=coupling,
                air_capacity_wh_per_k=air_capacity,
                mass_capacity_wh_per_k=mass_capacity,
                step_h=duration_key,
            )
            cache[duration_key] = matrices
        state[index] = matrices[0]
        outside[index] = matrices[1][:, 0]
        power[index] = matrices[1][:, 1] * cop[index]
        offset[index] = matrices[1][:, 1] * disturbance[index]
    return TwoR2CTransitionCoefficients(
        state=state,
        outside=outside,
        power=power,
        offset=offset,
    )


def _regular_observations(
    observations: Iterable[ThermalObservation],
    train_fraction: float,
) -> tuple[
    list[ThermalObservation],
    np.ndarray,
    np.ndarray,
    np.ndarray,
    np.ndarray,
    float,
    int,
]:
    values = list(observations)
    if len(values) - 1 < MIN_TRANSITIONS:
        raise CalibrationError(
            f"thermal calibration needs at least "
            f"{MIN_TRANSITIONS + 1} samples"
        )
    if not 0.5 <= train_fraction <= 0.9:
        raise CalibrationError(
            "train_fraction must be between 0.5 and 0.9"
        )
    timestamps = np.asarray([value.timestamp_s for value in values])
    steps_s = np.diff(timestamps)
    if np.any(steps_s <= 0):
        raise CalibrationError("timestamps must be strictly increasing")
    step_s = float(np.median(steps_s))
    if np.max(np.abs(steps_s - step_s)) > max(1.0, step_s * 0.02):
        raise CalibrationError(
            "samples must use one regular interval; "
            "split gaps into separate runs"
        )
    indoor = np.asarray(
        [value.indoor_temperature_c for value in values]
    )
    outdoor = np.asarray(
        [value.outdoor_temperature_c for value in values[:-1]]
    )
    electric = np.asarray(
        [value.heat_pump_power_w for value in values[:-1]]
    )
    train_count = int((len(values) - 1) * train_fraction)
    train_count = min(len(values) - 9, max(32, train_count))
    if train_count <= 0 or len(values) - 1 - train_count < 8:
        raise CalibrationError(
            "calibration needs at least eight validation transitions"
        )
    if float(np.std(outdoor[:train_count] - indoor[:train_count])) < 0.05:
        raise CalibrationError(
            "indoor-outdoor temperature difference has too little excitation"
        )
    thermal = np.asarray(
        [
            values[index].heat_pump_power_w
            for index in range(train_count)
        ]
    )
    if float(np.std(thermal)) < 20:
        raise CalibrationError(
            "heat-pump input has too little excitation"
        )
    return (
        values,
        timestamps,
        indoor,
        outdoor,
        electric,
        step_s,
        train_count,
    )


def _parameters_from_vector(
    vector: np.ndarray,
) -> tuple[float, float, float, float, float, float]:
    heat_loss = math.exp(float(vector[0]))
    coupling = math.exp(float(vector[1]))
    total_capacity = math.exp(float(vector[2]))
    air_fraction = float(vector[3])
    air_capacity = total_capacity * air_fraction
    mass_capacity = total_capacity * (1.0 - air_fraction)
    disturbance = float(vector[4]) * 1_000.0
    initial_mass_delta = float(vector[5])
    return (
        heat_loss,
        coupling,
        air_capacity,
        mass_capacity,
        disturbance,
        initial_mass_delta,
    )


def _simulate_parameters(
    vector: np.ndarray,
    *,
    initial_air_temperature_c: float,
    outside_temperature_c: np.ndarray,
    heat_pump_power_w: np.ndarray,
    cop_curve: COPCurve,
    step_h: float,
) -> np.ndarray:
    (
        heat_loss,
        coupling,
        air_capacity,
        mass_capacity,
        disturbance,
        initial_mass_delta,
    ) = _parameters_from_vector(vector)
    state_matrix, input_matrix = _two_r2c_discrete(
        heat_loss_w_per_k=heat_loss,
        mass_coupling_w_per_k=coupling,
        air_capacity_wh_per_k=air_capacity,
        mass_capacity_wh_per_k=mass_capacity,
        step_h=step_h,
    )
    states = np.zeros((len(outside_temperature_c) + 1, 2))
    states[0] = (
        initial_air_temperature_c,
        initial_air_temperature_c + initial_mass_delta,
    )
    for index, (outdoor, power) in enumerate(
        zip(
            outside_temperature_c,
            heat_pump_power_w,
            strict=True,
        )
    ):
        heat_w = cop_curve.at(float(outdoor)) * float(power) + disturbance
        states[index + 1] = (
            state_matrix @ states[index]
            + input_matrix[:, 0] * outdoor
            + input_matrix[:, 1] * heat_w
        )
    return states


def _rmse(actual: np.ndarray, predicted: np.ndarray) -> float:
    if len(actual) == 0 or len(actual) != len(predicted):
        raise CalibrationError(
            "RMSE inputs must have equal non-zero lengths"
        )
    return float(np.sqrt(np.mean(np.square(actual - predicted))))


def calibrate_two_r2c(
    observations: Iterable[ThermalObservation],
    *,
    model_id: str,
    cop_curve: COPCurve,
    priors: ThermalPriors,
    train_fraction: float = 0.75,
    source: str = "heat_pump_submeter",
    initializer: ThermalTwinArtifact | None = None,
) -> TwoR2CArtifact:
    if source not in {
        "heat_pump_submeter",
        "validated_component_balance",
    }:
        raise CalibrationError(
            "source must be heat_pump_submeter or "
            "validated_component_balance"
        )
    (
        values,
        timestamps,
        indoor,
        outdoor,
        electric,
        step_s,
        train_count,
    ) = _regular_observations(observations, train_fraction)
    if initializer is None:
        try:
            initializer = calibrate_thermal_twin(
                values,
                model_id=model_id,
                cop_curve=cop_curve,
                train_fraction=train_fraction,
                source=source,
            )
        except CalibrationError:
            initializer = None
    heat_loss_range = priors.heat_loss_w_per_k
    capacity_range = priors.total_capacity_wh_per_k
    coupling_range = priors.mass_coupling_w_per_k
    fraction_range = priors.air_capacity_fraction
    disturbance_range = priors.disturbance_heat_w
    lower = np.asarray(
        [
            math.log(heat_loss_range.minimum),
            math.log(coupling_range.minimum),
            math.log(capacity_range.minimum),
            fraction_range.minimum,
            disturbance_range.minimum / 1_000.0,
            -10.0,
        ]
    )
    upper = np.asarray(
        [
            math.log(heat_loss_range.maximum),
            math.log(coupling_range.maximum),
            math.log(capacity_range.maximum),
            fraction_range.maximum,
            disturbance_range.maximum / 1_000.0,
            10.0,
        ]
    )
    if initializer is None:
        initial_heat_loss = math.sqrt(
            heat_loss_range.minimum * heat_loss_range.maximum
        )
        initial_capacity = math.sqrt(
            capacity_range.minimum * capacity_range.maximum
        )
        initial_disturbance = min(
            disturbance_range.maximum,
            max(disturbance_range.minimum, 0.0),
        )
    else:
        initial_heat_loss = float(
            np.clip(
                initializer.heat_loss_w_per_k,
                heat_loss_range.minimum * 1.01,
                heat_loss_range.maximum * 0.99,
            )
        )
        initial_capacity = float(
            np.clip(
                initializer.thermal_capacity_wh_per_k,
                capacity_range.minimum * 1.01,
                capacity_range.maximum * 0.99,
            )
        )
        initial_disturbance = float(
            np.clip(
                initializer.disturbance_heat_w,
                disturbance_range.minimum,
                disturbance_range.maximum,
            )
        )
    step_h = step_s / 3_600.0
    base_log_loss = math.log(initial_heat_loss)
    base_log_capacity = math.log(initial_capacity)
    base_disturbance_kw = initial_disturbance / 1_000.0

    def residual(vector: np.ndarray) -> np.ndarray:
        states = _simulate_parameters(
            vector,
            initial_air_temperature_c=float(indoor[0]),
            outside_temperature_c=outdoor[:train_count],
            heat_pump_power_w=electric[:train_count],
            cop_curve=cop_curve,
            step_h=step_h,
        )
        data_residual = states[1:, 0] - indoor[1 : train_count + 1]
        regularization = np.asarray(
            [
                0.05 * (vector[0] - base_log_loss),
                0.05 * (vector[2] - base_log_capacity),
                0.02 * (vector[4] - base_disturbance_kw),
            ]
        )
        return np.concatenate((data_residual, regularization))

    fraction_seeds = (0.05, 0.15, 0.35, 0.5)
    coupling_multipliers = (1.0, 5.0, 20.0, 8.0)
    best_result: Any | None = None
    best_data_error = math.inf
    for fraction_seed, coupling_multiplier in zip(
        fraction_seeds,
        coupling_multipliers,
        strict=True,
    ):
        fraction = float(
            np.clip(
                fraction_seed,
                fraction_range.minimum * 1.01,
                fraction_range.maximum * 0.99,
            )
        )
        coupling = float(
            np.clip(
                initial_heat_loss * coupling_multiplier,
                coupling_range.minimum * 1.01,
                coupling_range.maximum * 0.99,
            )
        )
        seed = np.asarray(
            [
                base_log_loss,
                math.log(coupling),
                base_log_capacity,
                fraction,
                base_disturbance_kw,
                0.0,
            ]
        )
        result = least_squares(
            residual,
            seed,
            bounds=(lower, upper),
            loss="soft_l1",
            f_scale=0.1,
            x_scale="jac",
            max_nfev=300,
        )
        data_error = float(
            np.sum(np.square(residual(result.x)[:train_count]))
        )
        if data_error < best_data_error:
            best_result = result
            best_data_error = data_error
    if best_result is None:
        raise CalibrationError("2R2C calibration found no feasible model")
    (
        heat_loss,
        coupling,
        air_capacity,
        mass_capacity,
        disturbance,
        initial_mass_delta,
    ) = _parameters_from_vector(best_result.x)
    train_states = _simulate_parameters(
        best_result.x,
        initial_air_temperature_c=float(indoor[0]),
        outside_temperature_c=outdoor[:train_count],
        heat_pump_power_w=electric[:train_count],
        cop_curve=cop_curve,
        step_h=step_h,
    )
    provisional = TwoR2CArtifact(
        model_id=model_id,
        heat_loss_w_per_k=heat_loss,
        mass_coupling_w_per_k=coupling,
        air_capacity_wh_per_k=air_capacity,
        mass_capacity_wh_per_k=mass_capacity,
        cop_curve=cop_curve,
        disturbance_heat_w=disturbance,
        calibration=CalibrationEvidence(
            source=source,
            sample_count=len(values),
            transition_count=len(values) - 1,
            start_timestamp_s=float(timestamps[0]),
            end_timestamp_s=float(timestamps[-1]),
            step_s=step_s,
            train_transition_count=train_count,
            validation_transition_count=len(values) - 1 - train_count,
            standardized_condition_number=1.0,
            one_step_rmse_c=0.0,
            rollout_rmse_c=0.0,
            persistence_rmse_c=0.0,
            promotable=False,
            promotion_reasons=(),
        ),
    )
    validation_indices = range(train_count, len(values) - 1)
    observer_state = np.asarray(
        [indoor[train_count], train_states[-1, 1]]
    )
    one_step_values: list[float] = []
    for index in validation_indices:
        predicted = np.asarray(
            two_r2c_step(
                provisional,
                observer_state,
                outdoor[index],
                electric[index],
                step_h,
            )
        )
        one_step_values.append(float(predicted[0]))
        observer_state = np.asarray(
            [indoor[index + 1], predicted[1]]
        )
    rollout_state = (
        float(indoor[train_count]),
        float(train_states[-1, 1]),
    )
    rollout_values: list[float] = []
    for index in validation_indices:
        rollout_state = two_r2c_step(
            provisional,
            rollout_state,
            outdoor[index],
            electric[index],
            step_h,
        )
        rollout_values.append(rollout_state[0])
    validation_actual = indoor[train_count + 1 :]
    one_step_rmse = _rmse(
        validation_actual,
        np.asarray(one_step_values),
    )
    rollout_rmse = _rmse(
        validation_actual,
        np.asarray(rollout_values),
    )
    persistence_rmse = _rmse(
        validation_actual,
        indoor[train_count:-1],
    )
    singular_values = np.linalg.svd(
        np.asarray(best_result.jac),
        compute_uv=False,
    )
    condition_number = (
        math.inf
        if singular_values[-1] <= 1e-12
        else float(singular_values[0] / singular_values[-1])
    )
    promotion_reasons: list[str] = []
    duration_h = (timestamps[-1] - timestamps[0]) / 3_600.0
    if duration_h < 72:
        promotion_reasons.append("less than 72 hours of evidence")
    if not best_result.success:
        promotion_reasons.append("nonlinear calibration did not converge")
    if not math.isfinite(condition_number) or condition_number > 1_000_000:
        promotion_reasons.append(
            "scaled Jacobian condition number exceeds 1000000"
        )
    if one_step_rmse > 0.5:
        promotion_reasons.append(
            "one-step validation RMSE exceeds 0.5 C"
        )
    if rollout_rmse > 1.0:
        promotion_reasons.append(
            "rollout validation RMSE exceeds 1.0 C"
        )
    if one_step_rmse >= persistence_rmse:
        promotion_reasons.append(
            "one-step validation does not beat temperature persistence"
        )
    if np.any(np.asarray(best_result.active_mask) != 0):
        promotion_reasons.append(
            "a fitted parameter reached its search bound"
        )
    evidence = CalibrationEvidence(
        source=source,
        sample_count=len(values),
        transition_count=len(values) - 1,
        start_timestamp_s=float(timestamps[0]),
        end_timestamp_s=float(timestamps[-1]),
        step_s=step_s,
        train_transition_count=train_count,
        validation_transition_count=len(values) - 1 - train_count,
        standardized_condition_number=(
            condition_number if math.isfinite(condition_number) else 1e30
        ),
        one_step_rmse_c=one_step_rmse,
        rollout_rmse_c=rollout_rmse,
        persistence_rmse_c=persistence_rmse,
        promotable=not promotion_reasons,
        promotion_reasons=tuple(promotion_reasons),
    )
    return TwoR2CArtifact(
        model_id=model_id,
        heat_loss_w_per_k=heat_loss,
        mass_coupling_w_per_k=coupling,
        air_capacity_wh_per_k=air_capacity,
        mass_capacity_wh_per_k=mass_capacity,
        cop_curve=cop_curve,
        disturbance_heat_w=disturbance,
        calibration=evidence,
    )


def calibrate_model_family(
    observations: Iterable[ThermalObservation],
    *,
    home_spec: HomeSpec,
    source: str = "heat_pump_submeter",
) -> FamilyCalibrationResult:
    values = list(observations)
    if not values:
        raise CalibrationError("thermal observations must not be empty")
    artifacts: dict[str, ThermalArtifact] = {}
    summaries: list[CandidateSummary] = []
    initializer: ThermalTwinArtifact | None = None
    if MODEL_TYPE in home_spec.model_selection.candidates or (
        TWO_R_TWO_C_MODEL_TYPE in home_spec.model_selection.candidates
    ):
        try:
            initializer = calibrate_thermal_twin(
                values,
                model_id=home_spec.primary_zone_id,
                cop_curve=home_spec.heating.cop_curve,
                train_fraction=home_spec.model_selection.train_fraction,
                source=source,
            )
            if MODEL_TYPE in home_spec.model_selection.candidates:
                _validate_one_r1c_priors(
                    initializer,
                    home_spec.priors,
                )
                artifacts[MODEL_TYPE] = initializer
                summaries.append(
                    _candidate_summary(
                        MODEL_TYPE,
                        1,
                        initializer,
                    )
                )
        except CalibrationError as exc:
            if MODEL_TYPE in home_spec.model_selection.candidates:
                summaries.append(
                    _failed_candidate(MODEL_TYPE, 1, str(exc))
                )
    if TWO_R_TWO_C_MODEL_TYPE in home_spec.model_selection.candidates:
        try:
            artifact = calibrate_two_r2c(
                values,
                model_id=home_spec.primary_zone_id,
                cop_curve=home_spec.heating.cop_curve,
                priors=home_spec.priors,
                train_fraction=home_spec.model_selection.train_fraction,
                source=source,
                initializer=initializer,
            )
            artifacts[TWO_R_TWO_C_MODEL_TYPE] = artifact
            summaries.append(
                _candidate_summary(
                    TWO_R_TWO_C_MODEL_TYPE,
                    2,
                    artifact,
                )
            )
        except CalibrationError as exc:
            summaries.append(
                _failed_candidate(
                    TWO_R_TWO_C_MODEL_TYPE,
                    2,
                    str(exc),
                )
            )
    summaries.sort(key=lambda summary: summary.complexity)
    eligible = [
        summary
        for summary in summaries
        if summary.promotable
        and summary.rollout_rmse_c is not None
        and summary.model_type in artifacts
    ]
    champion: CandidateSummary | None = None
    decision: str
    if not eligible:
        decision = "no candidate passed the promotion checks"
    else:
        champion = eligible[0]
        decision = f"selected simplest promotable model {champion.model_type}"
        for candidate in eligible[1:]:
            assert champion.rollout_rmse_c is not None
            assert candidate.rollout_rmse_c is not None
            absolute = (
                champion.rollout_rmse_c - candidate.rollout_rmse_c
            )
            relative = absolute / max(champion.rollout_rmse_c, 1e-9)
            if (
                absolute
                >= home_spec.model_selection.minimum_rollout_improvement_c
                and relative
                >= home_spec.model_selection.minimum_relative_improvement
            ):
                previous = champion
                champion = candidate
                decision = (
                    f"selected {candidate.model_type}; validation rollout "
                    f"improved by {absolute:.3f} C "
                    f"({relative * 100:.1f}%) over "
                    f"{previous.model_type}"
                )
    report = ModelSelectionReport(
        home_spec_revision=home_spec.revision,
        sample_count=len(values),
        start_timestamp_s=float(values[0].timestamp_s),
        end_timestamp_s=float(values[-1].timestamp_s),
        champion_model_type=(
            None if champion is None else champion.model_type
        ),
        champion_revision=(
            None if champion is None else champion.revision
        ),
        decision=decision,
        candidates=tuple(summaries),
    )
    return FamilyCalibrationResult(report=report, artifacts=artifacts)


def _validate_one_r1c_priors(
    artifact: ThermalTwinArtifact,
    priors: ThermalPriors,
) -> None:
    checks = (
        (
            "heat_loss_w_per_k",
            artifact.heat_loss_w_per_k,
            priors.heat_loss_w_per_k,
        ),
        (
            "thermal_capacity_wh_per_k",
            artifact.thermal_capacity_wh_per_k,
            priors.total_capacity_wh_per_k,
        ),
        (
            "disturbance_heat_w",
            artifact.disturbance_heat_w,
            priors.disturbance_heat_w,
        ),
    )
    for field, value, bounds in checks:
        if not bounds.minimum <= value <= bounds.maximum:
            raise CalibrationError(
                f"fitted {field} is outside the home-spec prior "
                f"{bounds.minimum}..{bounds.maximum}"
            )


def _candidate_summary(
    model_type: str,
    complexity: int,
    artifact: ThermalArtifact,
) -> CandidateSummary:
    evidence = artifact.calibration
    return CandidateSummary(
        model_type=model_type,
        complexity=complexity,
        status="eligible" if evidence.promotable else "rejected",
        promotable=evidence.promotable,
        one_step_rmse_c=evidence.one_step_rmse_c,
        rollout_rmse_c=evidence.rollout_rmse_c,
        persistence_rmse_c=evidence.persistence_rmse_c,
        reasons=evidence.promotion_reasons,
        revision=artifact.revision,
    )


def _failed_candidate(
    model_type: str,
    complexity: int,
    reason: str,
) -> CandidateSummary:
    return CandidateSummary(
        model_type=model_type,
        complexity=complexity,
        status="failed",
        promotable=False,
        one_step_rmse_c=None,
        rollout_rmse_c=None,
        persistence_rmse_c=None,
        reasons=(reason,),
        revision=None,
    )
