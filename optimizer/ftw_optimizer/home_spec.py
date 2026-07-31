from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .thermal_twin import COPCurve, CalibrationError, MODEL_TYPE


HOME_SPEC_KIND = "ftw.home_thermal_spec"
HOME_SPEC_SCHEMA_VERSION = 1
TWO_R_TWO_C_MODEL_TYPE = "ftw-2r2c-v1"
SUPPORTED_MODEL_TYPES = (MODEL_TYPE, TWO_R_TWO_C_MODEL_TYPE)


def _object(value: Any, field: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise CalibrationError(f"{field} must be an object")
    return value


def _number(value: Any, field: str) -> float:
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise CalibrationError(f"{field} must be a number") from exc
    if not math.isfinite(result):
        raise CalibrationError(f"{field} must be finite")
    return result


def _positive(value: Any, field: str) -> float:
    result = _number(value, field)
    if result <= 0:
        raise CalibrationError(f"{field} must be > 0")
    return result


def _non_empty(value: Any, field: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise CalibrationError(f"{field} must be a non-empty string")
    return value.strip()


@dataclass(frozen=True)
class ParameterRange:
    minimum: float
    maximum: float

    def __post_init__(self) -> None:
        if (
            not math.isfinite(self.minimum)
            or not math.isfinite(self.maximum)
            or self.minimum >= self.maximum
        ):
            raise CalibrationError(
                "parameter range bounds must be finite and increasing"
            )

    def to_list(self) -> list[float]:
        return [float(self.minimum), float(self.maximum)]

    @classmethod
    def from_value(
        cls,
        raw: Any,
        field: str,
        default: tuple[float, float],
    ) -> ParameterRange:
        if raw is None:
            return cls(*default)
        if not isinstance(raw, list) or len(raw) != 2:
            raise CalibrationError(f"{field} must be [minimum, maximum]")
        return cls(
            _number(raw[0], f"{field}[0]"),
            _number(raw[1], f"{field}[1]"),
        )


@dataclass(frozen=True)
class ThermalPriors:
    heat_loss_w_per_k: ParameterRange = ParameterRange(5.0, 5_000.0)
    total_capacity_wh_per_k: ParameterRange = ParameterRange(
        200.0,
        1_000_000.0,
    )
    mass_coupling_w_per_k: ParameterRange = ParameterRange(5.0, 10_000.0)
    air_capacity_fraction: ParameterRange = ParameterRange(0.01, 0.6)
    disturbance_heat_w: ParameterRange = ParameterRange(-20_000.0, 20_000.0)

    def __post_init__(self) -> None:
        if self.air_capacity_fraction.minimum <= 0:
            raise CalibrationError(
                "priors.air_capacity_fraction minimum must be > 0"
            )
        if self.air_capacity_fraction.maximum >= 1:
            raise CalibrationError(
                "priors.air_capacity_fraction maximum must be < 1"
            )
        if self.total_capacity_wh_per_k.minimum <= 0:
            raise CalibrationError(
                "priors.total_capacity_wh_per_k minimum must be > 0"
            )
        if self.heat_loss_w_per_k.minimum <= 0:
            raise CalibrationError(
                "priors.heat_loss_w_per_k minimum must be > 0"
            )
        if self.mass_coupling_w_per_k.minimum <= 0:
            raise CalibrationError(
                "priors.mass_coupling_w_per_k minimum must be > 0"
            )

    def to_dict(self) -> dict[str, list[float]]:
        return {
            "heat_loss_w_per_k": self.heat_loss_w_per_k.to_list(),
            "total_capacity_wh_per_k": (
                self.total_capacity_wh_per_k.to_list()
            ),
            "mass_coupling_w_per_k": (
                self.mass_coupling_w_per_k.to_list()
            ),
            "air_capacity_fraction": self.air_capacity_fraction.to_list(),
            "disturbance_heat_w": self.disturbance_heat_w.to_list(),
        }

    @classmethod
    def from_dict(cls, raw: Any) -> ThermalPriors:
        value = {} if raw is None else _object(raw, "priors")
        return cls(
            heat_loss_w_per_k=ParameterRange.from_value(
                value.get("heat_loss_w_per_k"),
                "priors.heat_loss_w_per_k",
                (5.0, 5_000.0),
            ),
            total_capacity_wh_per_k=ParameterRange.from_value(
                value.get("total_capacity_wh_per_k"),
                "priors.total_capacity_wh_per_k",
                (200.0, 1_000_000.0),
            ),
            mass_coupling_w_per_k=ParameterRange.from_value(
                value.get("mass_coupling_w_per_k"),
                "priors.mass_coupling_w_per_k",
                (5.0, 10_000.0),
            ),
            air_capacity_fraction=ParameterRange.from_value(
                value.get("air_capacity_fraction"),
                "priors.air_capacity_fraction",
                (0.01, 0.6),
            ),
            disturbance_heat_w=ParameterRange.from_value(
                value.get("disturbance_heat_w"),
                "priors.disturbance_heat_w",
                (-20_000.0, 20_000.0),
            ),
        )


@dataclass(frozen=True)
class SeriesRef:
    driver: str
    metric: str
    scale: float = 1.0
    offset: float = 0.0

    def __post_init__(self) -> None:
        _non_empty(self.driver, "series.driver")
        _non_empty(self.metric, "series.metric")
        if not math.isfinite(self.scale) or self.scale == 0:
            raise CalibrationError("series.scale must be finite and non-zero")
        if not math.isfinite(self.offset):
            raise CalibrationError("series.offset must be finite")

    def apply(self, value: float) -> float:
        return float(value) * self.scale + self.offset

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "driver": self.driver,
            "metric": self.metric,
        }
        if self.scale != 1:
            result["scale"] = self.scale
        if self.offset != 0:
            result["offset"] = self.offset
        return result

    @classmethod
    def from_dict(cls, raw: Any, field: str) -> SeriesRef:
        value = _object(raw, field)
        return cls(
            driver=_non_empty(value.get("driver"), f"{field}.driver"),
            metric=_non_empty(value.get("metric"), f"{field}.metric"),
            scale=_number(value.get("scale", 1), f"{field}.scale"),
            offset=_number(value.get("offset", 0), f"{field}.offset"),
        )


@dataclass(frozen=True)
class ZoneSpec:
    zone_id: str
    floor_area_m2: float | None
    volume_m3: float | None
    minimum_temperature_c: float
    maximum_temperature_c: float

    def __post_init__(self) -> None:
        _non_empty(self.zone_id, "zones[].id")
        if self.floor_area_m2 is not None and (
            self.floor_area_m2 <= 0
            or not math.isfinite(self.floor_area_m2)
        ):
            raise CalibrationError(
                "zones[].floor_area_m2 must be > 0 when set"
            )
        if self.volume_m3 is not None and (
            self.volume_m3 <= 0 or not math.isfinite(self.volume_m3)
        ):
            raise CalibrationError("zones[].volume_m3 must be > 0 when set")
        if (
            not math.isfinite(self.minimum_temperature_c)
            or not math.isfinite(self.maximum_temperature_c)
            or self.minimum_temperature_c >= self.maximum_temperature_c
        ):
            raise CalibrationError(
                "zones[] temperature bounds must be finite and increasing"
            )

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "id": self.zone_id,
            "comfort": {
                "minimum_temperature_c": self.minimum_temperature_c,
                "maximum_temperature_c": self.maximum_temperature_c,
            },
        }
        if self.floor_area_m2 is not None:
            result["floor_area_m2"] = self.floor_area_m2
        if self.volume_m3 is not None:
            result["volume_m3"] = self.volume_m3
        return result

    @classmethod
    def from_dict(cls, raw: Any, index: int) -> ZoneSpec:
        field = f"zones[{index}]"
        value = _object(raw, field)
        comfort = _object(value.get("comfort", {}), f"{field}.comfort")
        volume = value.get("volume_m3")
        return cls(
            zone_id=_non_empty(value.get("id"), f"{field}.id"),
            floor_area_m2=(
                None
                if value.get("floor_area_m2") is None
                else _positive(
                    value.get("floor_area_m2"),
                    f"{field}.floor_area_m2",
                )
            ),
            volume_m3=(
                None
                if volume is None
                else _positive(volume, f"{field}.volume_m3")
            ),
            minimum_temperature_c=_number(
                comfort.get("minimum_temperature_c", 19),
                f"{field}.comfort.minimum_temperature_c",
            ),
            maximum_temperature_c=_number(
                comfort.get("maximum_temperature_c", 23),
                f"{field}.comfort.maximum_temperature_c",
            ),
        )


@dataclass(frozen=True)
class HeatingSpec:
    source: str
    emitters: str
    maximum_electric_power_w: float
    cop_curve: COPCurve
    buffer_tank_l: float | None = None
    hot_water_tank_l: float | None = None

    def __post_init__(self) -> None:
        _non_empty(self.source, "heating.source")
        _non_empty(self.emitters, "heating.emitters")
        if (
            not math.isfinite(self.maximum_electric_power_w)
            or self.maximum_electric_power_w <= 0
        ):
            raise CalibrationError(
                "heating.maximum_electric_power_w must be > 0"
            )
        for field, value in (
            ("buffer_tank_l", self.buffer_tank_l),
            ("hot_water_tank_l", self.hot_water_tank_l),
        ):
            if value is not None and (not math.isfinite(value) or value <= 0):
                raise CalibrationError(
                    f"heating.{field} must be > 0 when set"
                )

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "source": self.source,
            "emitters": self.emitters,
            "maximum_electric_power_w": self.maximum_electric_power_w,
            "cop_curve": self.cop_curve.to_dict(),
        }
        if self.buffer_tank_l is not None:
            result["buffer_tank_l"] = self.buffer_tank_l
        if self.hot_water_tank_l is not None:
            result["hot_water_tank_l"] = self.hot_water_tank_l
        return result

    @classmethod
    def from_dict(cls, raw: Any) -> HeatingSpec:
        value = _object(raw, "heating")
        return cls(
            source=_non_empty(value.get("source"), "heating.source"),
            emitters=_non_empty(value.get("emitters"), "heating.emitters"),
            maximum_electric_power_w=_positive(
                value.get("maximum_electric_power_w"),
                "heating.maximum_electric_power_w",
            ),
            cop_curve=COPCurve.from_dict(value.get("cop_curve")),
            buffer_tank_l=(
                None
                if value.get("buffer_tank_l") is None
                else _positive(
                    value.get("buffer_tank_l"),
                    "heating.buffer_tank_l",
                )
            ),
            hot_water_tank_l=(
                None
                if value.get("hot_water_tank_l") is None
                else _positive(
                    value.get("hot_water_tank_l"),
                    "heating.hot_water_tank_l",
                )
            ),
        )


@dataclass(frozen=True)
class ModelSelectionSpec:
    candidates: tuple[str, ...] = SUPPORTED_MODEL_TYPES
    train_fraction: float = 0.75
    minimum_rollout_improvement_c: float = 0.05
    minimum_relative_improvement: float = 0.10

    def __post_init__(self) -> None:
        if not self.candidates:
            raise CalibrationError(
                "model_selection.candidates must not be empty"
            )
        if len(set(self.candidates)) != len(self.candidates):
            raise CalibrationError(
                "model_selection.candidates must be unique"
            )
        unsupported = sorted(
            set(self.candidates) - set(SUPPORTED_MODEL_TYPES)
        )
        if unsupported:
            raise CalibrationError(
                "unsupported thermal model candidates: "
                + ", ".join(unsupported)
            )
        if not 0.5 <= self.train_fraction <= 0.9:
            raise CalibrationError(
                "model_selection.train_fraction must be between 0.5 and 0.9"
            )
        if self.minimum_rollout_improvement_c < 0:
            raise CalibrationError(
                "minimum_rollout_improvement_c must be >= 0"
            )
        if not 0 <= self.minimum_relative_improvement < 1:
            raise CalibrationError(
                "minimum_relative_improvement must be in [0, 1)"
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "candidates": list(self.candidates),
            "train_fraction": self.train_fraction,
            "minimum_rollout_improvement_c": (
                self.minimum_rollout_improvement_c
            ),
            "minimum_relative_improvement": (
                self.minimum_relative_improvement
            ),
        }

    @classmethod
    def from_dict(cls, raw: Any) -> ModelSelectionSpec:
        value = {} if raw is None else _object(raw, "model_selection")
        candidates = value.get(
            "candidates",
            list(SUPPORTED_MODEL_TYPES),
        )
        if not isinstance(candidates, list) or any(
            not isinstance(candidate, str) for candidate in candidates
        ):
            raise CalibrationError(
                "model_selection.candidates must be an array of strings"
            )
        return cls(
            candidates=tuple(candidates),
            train_fraction=_number(
                value.get("train_fraction", 0.75),
                "model_selection.train_fraction",
            ),
            minimum_rollout_improvement_c=_number(
                value.get("minimum_rollout_improvement_c", 0.05),
                "model_selection.minimum_rollout_improvement_c",
            ),
            minimum_relative_improvement=_number(
                value.get("minimum_relative_improvement", 0.10),
                "model_selection.minimum_relative_improvement",
            ),
        )


@dataclass(frozen=True)
class HomeSpec:
    site_id: str
    primary_zone_id: str
    zones: tuple[ZoneSpec, ...]
    heating: HeatingSpec
    sensors: dict[str, SeriesRef]
    priors: ThermalPriors = ThermalPriors()
    model_selection: ModelSelectionSpec = ModelSelectionSpec()

    def __post_init__(self) -> None:
        _non_empty(self.site_id, "site_id")
        if not self.zones:
            raise CalibrationError("zones must not be empty")
        zone_ids = [zone.zone_id for zone in self.zones]
        if len(set(zone_ids)) != len(zone_ids):
            raise CalibrationError("zones[].id must be unique")
        if self.primary_zone_id not in zone_ids:
            raise CalibrationError(
                "primary_zone_id must name one of zones[].id"
            )
        for name in ("indoor_temperature", "outdoor_temperature"):
            if name not in self.sensors:
                raise CalibrationError(f"sensors.{name} is required")
        unknown = sorted(
            set(self.sensors)
            - {
                "indoor_temperature",
                "outdoor_temperature",
                "heat_pump_power",
                "supply_temperature",
                "return_temperature",
                "hot_water_temperature",
                "solar_irradiance",
            }
        )
        if unknown:
            raise CalibrationError(
                "unsupported thermal sensors: " + ", ".join(unknown)
            )

    @property
    def primary_zone(self) -> ZoneSpec:
        return next(
            zone for zone in self.zones if zone.zone_id == self.primary_zone_id
        )

    @property
    def revision(self) -> str:
        encoded = json.dumps(
            self.to_dict(include_revision=False),
            allow_nan=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode()
        return hashlib.sha256(encoded).hexdigest()

    def to_dict(self, *, include_revision: bool = True) -> dict[str, Any]:
        result: dict[str, Any] = {
            "schema_version": HOME_SPEC_SCHEMA_VERSION,
            "kind": HOME_SPEC_KIND,
            "site_id": self.site_id,
            "primary_zone_id": self.primary_zone_id,
            "zones": [zone.to_dict() for zone in self.zones],
            "heating": self.heating.to_dict(),
            "sensors": {
                name: sensor.to_dict()
                for name, sensor in sorted(self.sensors.items())
            },
            "priors": self.priors.to_dict(),
            "model_selection": self.model_selection.to_dict(),
        }
        if include_revision:
            result["revision"] = self.revision
        return result

    @classmethod
    def from_dict(cls, raw: Any) -> HomeSpec:
        value = _object(raw, "home spec")
        if value.get("schema_version") != HOME_SPEC_SCHEMA_VERSION:
            raise CalibrationError(
                f"home spec schema_version must be {HOME_SPEC_SCHEMA_VERSION}"
            )
        if value.get("kind") != HOME_SPEC_KIND:
            raise CalibrationError(
                f"home spec kind must be {HOME_SPEC_KIND!r}"
            )
        raw_zones = value.get("zones")
        if not isinstance(raw_zones, list):
            raise CalibrationError("zones must be an array")
        sensors = _object(value.get("sensors"), "sensors")
        spec = cls(
            site_id=_non_empty(value.get("site_id"), "site_id"),
            primary_zone_id=_non_empty(
                value.get("primary_zone_id"),
                "primary_zone_id",
            ),
            zones=tuple(
                ZoneSpec.from_dict(zone, index)
                for index, zone in enumerate(raw_zones)
            ),
            heating=HeatingSpec.from_dict(value.get("heating")),
            sensors={
                name: SeriesRef.from_dict(sensor, f"sensors.{name}")
                for name, sensor in sensors.items()
            },
            priors=ThermalPriors.from_dict(value.get("priors")),
            model_selection=ModelSelectionSpec.from_dict(
                value.get("model_selection")
            ),
        )
        revision = value.get("revision")
        if revision is not None and revision != spec.revision:
            raise CalibrationError(
                "home spec revision does not match its contents"
            )
        return spec


def load_home_spec(path: str | Path) -> HomeSpec:
    try:
        value = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise CalibrationError(f"cannot read home spec: {exc}") from exc
    return HomeSpec.from_dict(value)
