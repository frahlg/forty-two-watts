from __future__ import annotations

import argparse
import hashlib
import json
import math
import sys
import time
import urllib.parse
import urllib.request
from collections.abc import Callable, Sequence
from pathlib import Path
from typing import Any

from .home_spec import HomeSpec, SeriesRef, load_home_spec
from .thermal_family import (
    FamilyCalibrationResult,
    calibrate_model_family,
)
from .thermal_twin import (
    MIN_TRANSITIONS,
    CalibrationError,
    ThermalObservation,
)


THERMAL_DATASET_KIND = "ftw.thermal_observations"
THERMAL_DATASET_SCHEMA_VERSION = 2
SERIES_API_RESAMPLING_RECIPE = "series-bucket-average-v1"
SERIES_API_PROMOTION_BLOCKER = (
    "the current series API does not preserve boundary temperature and "
    "time-weighted power semantics"
)
FetchJSON = Callable[[str, float], dict[str, Any]]


def _dataset_digest(dataset: dict[str, Any]) -> str:
    content = {
        "schema_version": dataset.get("schema_version"),
        "kind": dataset.get("kind"),
        "home_spec_revision": dataset.get("home_spec_revision"),
        "step_s": dataset.get("step_s"),
        "resampling_recipe": dataset.get("resampling_recipe"),
        "observations": dataset.get("observations"),
    }
    encoded = json.dumps(
        content,
        allow_nan=False,
        separators=(",", ":"),
        sort_keys=True,
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def _safe_source(api_base: str) -> str:
    parsed = urllib.parse.urlsplit(api_base.rstrip("/"))
    host = parsed.hostname or ""
    if ":" in host:
        host = f"[{host}]"
    port = f":{parsed.port}" if parsed.port is not None else ""
    return urllib.parse.urlunsplit(
        (parsed.scheme, f"{host}{port}", parsed.path, "", "")
    )


def _get_json(url: str, timeout_s: float) -> dict[str, Any]:
    request = urllib.request.Request(
        url,
        headers={
            "Accept": "application/json",
            "User-Agent": "ftw-optimizer-thermal-backtest/1",
        },
        method="GET",
    )
    with urllib.request.urlopen(request, timeout=timeout_s) as response:
        payload = json.load(response)
    if not isinstance(payload, dict):
        raise CalibrationError(
            f"{url} returned a non-object JSON payload"
        )
    return payload


def _series_points(
    payload: dict[str, Any],
    metric: str,
) -> list[dict[str, Any]]:
    found = False
    if payload.get("metric") == metric:
        found = True
        points = payload.get("points")
    else:
        series = payload.get("series")
        points = None
        if isinstance(series, list):
            for item in series:
                if (
                    isinstance(item, dict)
                    and item.get("metric") == metric
                ):
                    found = True
                    points = item.get("points")
                    break
    if found and points is None:
        return []
    if not isinstance(points, list):
        raise CalibrationError(
            f"FTW series response has no points for metric {metric!r}"
        )
    return [point for point in points if isinstance(point, dict)]


def _chunk_ranges(
    since_ms: int,
    until_ms: int,
    chunk_ms: int,
) -> list[tuple[int, int]]:
    if since_ms <= 0 or until_ms <= since_ms:
        raise CalibrationError(
            "thermal export needs positive, increasing time bounds"
        )
    if chunk_ms <= 0:
        raise CalibrationError("thermal export chunk size must be positive")
    chunks: list[tuple[int, int]] = []
    start = since_ms
    while start < until_ms:
        end = min(until_ms, start + chunk_ms)
        chunks.append((start, end))
        start = end
    return chunks


def _fetch_sensor_buckets(
    *,
    api_base: str,
    sensor: SeriesRef,
    since_ms: int,
    until_ms: int,
    step_ms: int,
    chunk_days: int,
    timeout_s: float,
    fetch_json: FetchJSON,
) -> tuple[dict[int, float], int]:
    bucket_count = math.ceil((until_ms - since_ms) / step_ms)
    accumulated: dict[int, tuple[float, int]] = {}
    raw_point_count = 0
    for chunk_start, chunk_end in _chunk_ranges(
        since_ms,
        until_ms,
        chunk_days * 24 * 60 * 60 * 1_000,
    ):
        point_budget = max(
            1,
            math.ceil((chunk_end - chunk_start) / step_ms),
        )
        query = urllib.parse.urlencode(
            {
                "driver": sensor.driver,
                "metric": sensor.metric,
                "since": chunk_start,
                "until": chunk_end,
                "points": point_budget,
            }
        )
        payload = fetch_json(
            f"{api_base.rstrip('/')}/api/series?{query}",
            timeout_s,
        )
        for point in _series_points(payload, sensor.metric):
            try:
                timestamp_ms = int(
                    point.get("ts", point.get("ts_ms"))
                )
                value = sensor.apply(float(point["v"]))
                samples = max(1, int(point.get("n", 1)))
            except (TypeError, ValueError, KeyError):
                continue
            if (
                timestamp_ms < since_ms
                or timestamp_ms > until_ms
                or not math.isfinite(value)
            ):
                continue
            index = min(
                bucket_count - 1,
                max(0, (timestamp_ms - since_ms) // step_ms),
            )
            previous_total, previous_count = accumulated.get(
                index,
                (0.0, 0),
            )
            accumulated[index] = (
                previous_total + value * samples,
                previous_count + samples,
            )
            raw_point_count += 1
    return (
        {
            index: total / count
            for index, (total, count) in accumulated.items()
            if count > 0
        },
        raw_point_count,
    )


def _longest_contiguous_run(
    indexes: Sequence[int],
) -> list[int]:
    if not indexes:
        return []
    ordered = sorted(set(indexes))
    longest: list[int] = []
    current = [ordered[0]]
    for index in ordered[1:]:
        if index == current[-1] + 1:
            current.append(index)
            continue
        if len(current) > len(longest):
            longest = current
        current = [index]
    if len(current) > len(longest):
        longest = current
    return longest


def export_thermal_dataset(
    *,
    home_spec: HomeSpec,
    api_base: str,
    since_ms: int,
    until_ms: int,
    step_s: int = 900,
    chunk_days: int = 7,
    timeout_s: float = 30.0,
    fetch_json: FetchJSON = _get_json,
) -> dict[str, Any]:
    if step_s <= 0:
        raise CalibrationError("step_s must be positive")
    if chunk_days <= 0:
        raise CalibrationError("chunk_days must be positive")
    if since_ms <= 0 or until_ms <= since_ms:
        raise CalibrationError(
            "thermal export needs positive, increasing time bounds"
        )
    safe_source = _safe_source(api_base)
    required = {
        "indoor_temperature": "indoor_temp_c",
        "outdoor_temperature": "outdoor_temp_c",
        "heat_pump_power": "heat_pump_power_w",
    }
    missing = sorted(set(required) - set(home_spec.sensors))
    if missing:
        result = {
            "schema_version": THERMAL_DATASET_SCHEMA_VERSION,
            "kind": THERMAL_DATASET_KIND,
            "home_spec_revision": home_spec.revision,
            "source": safe_source,
            "since_ms": since_ms,
            "until_ms": until_ms,
            "step_s": step_s,
            "resampling_recipe": SERIES_API_RESAMPLING_RECIPE,
            "ready": False,
            "coverage": {
                "total_buckets": math.ceil(
                    (until_ms - since_ms) / (step_s * 1_000)
                ),
                "complete_buckets": 0,
                "longest_contiguous_samples": 0,
                "required_samples": MIN_TRANSITIONS + 1,
                "sensor_bucket_counts": {},
                "sensor_point_counts": {},
            },
            "blocking_reasons": [
                "home spec has no sensor mapping for "
                + ", ".join(missing)
            ],
            "observations": [],
        }
        result["dataset_sha256"] = _dataset_digest(result)
        result["promotion_blocking_reasons"] = [
            SERIES_API_PROMOTION_BLOCKER
        ]
        return result
    step_ms = step_s * 1_000
    buckets: dict[str, dict[int, float]] = {}
    raw_counts: dict[str, int] = {}
    for sensor_name in required:
        values, raw_count = _fetch_sensor_buckets(
            api_base=api_base,
            sensor=home_spec.sensors[sensor_name],
            since_ms=since_ms,
            until_ms=until_ms,
            step_ms=step_ms,
            chunk_days=chunk_days,
            timeout_s=timeout_s,
            fetch_json=fetch_json,
        )
        buckets[sensor_name] = values
        raw_counts[sensor_name] = raw_count
    complete = set(buckets["indoor_temperature"])
    complete &= set(buckets["outdoor_temperature"])
    complete &= set(buckets["heat_pump_power"])
    longest = _longest_contiguous_run(sorted(complete))
    observations = [
        {
            "timestamp_s": (since_ms + index * step_ms) / 1_000.0,
            "indoor_temp_c": buckets["indoor_temperature"][index],
            "outdoor_temp_c": buckets["outdoor_temperature"][index],
            "heat_pump_power_w": buckets["heat_pump_power"][index],
        }
        for index in longest
    ]
    blocking_reasons: list[str] = []
    for sensor_name, values in buckets.items():
        if not values:
            blocking_reasons.append(
                f"{sensor_name} has no samples in the requested window"
            )
    if len(longest) < MIN_TRANSITIONS + 1:
        blocking_reasons.append(
            f"longest complete run has {len(longest)} samples; "
            f"at least {MIN_TRANSITIONS + 1} are required"
        )
    duration_h = (
        0.0
        if len(longest) < 2
        else (len(longest) - 1) * step_s / 3_600.0
    )
    if duration_h < 72:
        blocking_reasons.append(
            f"longest complete run covers {duration_h:.1f} hours; "
            "72 hours are required for promotion"
        )
    result = {
        "schema_version": THERMAL_DATASET_SCHEMA_VERSION,
        "kind": THERMAL_DATASET_KIND,
        "home_spec_revision": home_spec.revision,
        "source": safe_source,
        "since_ms": since_ms,
        "until_ms": until_ms,
        "step_s": step_s,
        "resampling_recipe": SERIES_API_RESAMPLING_RECIPE,
        "ready": not blocking_reasons,
        "coverage": {
            "total_buckets": math.ceil(
                (until_ms - since_ms) / step_ms
            ),
            "complete_buckets": len(complete),
            "longest_contiguous_samples": len(longest),
            "required_samples": MIN_TRANSITIONS + 1,
            "sensor_bucket_counts": {
                name: len(values) for name, values in buckets.items()
            },
            "sensor_point_counts": raw_counts,
        },
        "blocking_reasons": blocking_reasons,
        "observations": observations,
    }
    result["dataset_sha256"] = _dataset_digest(result)
    result["promotion_blocking_reasons"] = [SERIES_API_PROMOTION_BLOCKER]
    return result


def observations_from_dataset(
    dataset: dict[str, Any],
    *,
    home_spec: HomeSpec,
) -> list[ThermalObservation]:
    if dataset.get("schema_version") != THERMAL_DATASET_SCHEMA_VERSION:
        raise CalibrationError(
            "unsupported thermal dataset schema_version"
        )
    if dataset.get("kind") != THERMAL_DATASET_KIND:
        raise CalibrationError(
            f"thermal dataset kind must be {THERMAL_DATASET_KIND!r}"
        )
    if dataset.get("home_spec_revision") != home_spec.revision:
        raise CalibrationError(
            "thermal dataset was exported for a different home spec"
        )
    recipe = dataset.get("resampling_recipe")
    if not isinstance(recipe, str) or not recipe:
        raise CalibrationError("thermal dataset resampling_recipe is missing")
    digest = dataset.get("dataset_sha256")
    if digest is not None and digest != _dataset_digest(dataset):
        raise CalibrationError("thermal dataset digest does not match its contents")
    try:
        declared_step_s = float(dataset.get("step_s"))
    except (TypeError, ValueError) as exc:
        raise CalibrationError("thermal dataset step_s must be a number") from exc
    if not math.isfinite(declared_step_s) or declared_step_s <= 0:
        raise CalibrationError("thermal dataset step_s must be positive")
    rows = dataset.get("observations")
    if not isinstance(rows, list):
        raise CalibrationError(
            "thermal dataset observations must be an array"
        )
    observations: list[ThermalObservation] = []
    for index, row in enumerate(rows):
        if not isinstance(row, dict):
            raise CalibrationError(
                f"thermal dataset observation {index} must be an object"
            )
        try:
            observations.append(
                ThermalObservation(
                    timestamp_s=float(row["timestamp_s"]),
                    indoor_temperature_c=float(
                        row["indoor_temp_c"]
                    ),
                    outdoor_temperature_c=float(
                        row["outdoor_temp_c"]
                    ),
                    heat_pump_power_w=float(
                        row["heat_pump_power_w"]
                    ),
                )
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise CalibrationError(
                f"invalid thermal dataset observation {index}"
            ) from exc
    if len(observations) < MIN_TRANSITIONS + 1:
        reasons = dataset.get("blocking_reasons", [])
        suffix = (
            ": " + "; ".join(str(reason) for reason in reasons)
            if isinstance(reasons, list) and reasons
            else ""
        )
        raise CalibrationError(
            f"thermal dataset has {len(observations)} samples; "
            f"at least {MIN_TRANSITIONS + 1} are required"
            + suffix
        )
    for index in range(len(observations) - 1):
        actual_step_s = (
            observations[index + 1].timestamp_s
            - observations[index].timestamp_s
        )
        if abs(actual_step_s - declared_step_s) > max(
            1.0,
            declared_step_s * 0.02,
        ):
            raise CalibrationError(
                "thermal dataset timestamps do not match step_s"
            )
    return observations


def run_thermal_backtest(
    *,
    dataset: dict[str, Any],
    home_spec: HomeSpec,
    source: str = "heat_pump_submeter",
) -> FamilyCalibrationResult:
    observations = observations_from_dataset(dataset, home_spec=home_spec)
    digest = _dataset_digest(dataset)
    return calibrate_model_family(
        observations,
        home_spec=home_spec,
        dataset_sha256=digest,
        resampling_recipe=str(dataset["resampling_recipe"]),
        source=source,
    )


def _load_json(path: Path, label: str) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise CalibrationError(f"cannot read {label}: {exc}") from exc
    if not isinstance(value, dict):
        raise CalibrationError(f"{label} must contain an object")
    return value


def _write_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(
        json.dumps(
            value,
            allow_nan=False,
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )
    temporary.replace(path)


def _write_backtest_result(
    result: FamilyCalibrationResult,
    output: Path,
    artifact_dir: Path | None,
) -> None:
    payload = {
        "report": result.report.to_dict(),
        "artifacts": {
            model_type: artifact.to_dict()
            for model_type, artifact in result.artifacts.items()
        },
    }
    _write_json(output, payload)
    if artifact_dir is None:
        return
    artifact_dir.mkdir(parents=True, exist_ok=True)
    for model_type, artifact in result.artifacts.items():
        safe_name = model_type.replace("/", "_")
        _write_json(
            artifact_dir / f"{safe_name}-{artifact.revision[:12]}.json",
            artifact.to_dict(),
        )


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=(
            "Export FTW thermal telemetry and compare home model candidates"
        )
    )
    subparsers = parser.add_subparsers(dest="command", required=True)
    export = subparsers.add_parser(
        "export",
        help="export regular thermal observations through the read-only API",
    )
    export.add_argument("--home-spec", type=Path, required=True)
    export.add_argument("--api-base", required=True)
    export.add_argument("--output", type=Path, required=True)
    export.add_argument("--days", type=int, default=30)
    export.add_argument("--until-ms", type=int)
    export.add_argument("--step-min", type=int, default=15)
    export.add_argument("--chunk-days", type=int, default=7)
    export.add_argument("--timeout-s", type=float, default=30)

    run = subparsers.add_parser(
        "run",
        help="calibrate and compare models on an exported dataset",
    )
    run.add_argument("--home-spec", type=Path, required=True)
    run.add_argument("--input", type=Path, required=True)
    run.add_argument("--output", type=Path, required=True)
    run.add_argument("--artifact-dir", type=Path)
    run.add_argument(
        "--power-source",
        choices=(
            "heat_pump_submeter",
            "validated_component_balance",
        ),
        default="heat_pump_submeter",
    )
    return parser


def main(argv: Sequence[str] | None = None) -> None:
    args = _parser().parse_args(argv)
    try:
        home_spec = load_home_spec(args.home_spec)
        if args.command == "export":
            if (
                args.days <= 0
                or args.step_min <= 0
                or args.chunk_days <= 0
                or args.timeout_s <= 0
            ):
                raise CalibrationError(
                    "days, step-min, chunk-days and timeout-s "
                    "must be positive"
                )
            until_ms = (
                int(time.time() * 1_000)
                if args.until_ms is None
                else args.until_ms
            )
            step_ms = args.step_min * 60 * 1_000
            until_ms -= until_ms % step_ms
            since_ms = until_ms - args.days * 24 * 60 * 60 * 1_000
            dataset = export_thermal_dataset(
                home_spec=home_spec,
                api_base=args.api_base,
                since_ms=since_ms,
                until_ms=until_ms,
                step_s=args.step_min * 60,
                chunk_days=args.chunk_days,
                timeout_s=args.timeout_s,
            )
            _write_json(args.output, dataset)
            json.dump(
                {
                    "ready": dataset["ready"],
                    "coverage": dataset["coverage"],
                    "blocking_reasons": dataset["blocking_reasons"],
                },
                sys.stdout,
                indent=2,
            )
            sys.stdout.write("\n")
            return
        result = run_thermal_backtest(
            dataset=_load_json(args.input, "thermal dataset"),
            home_spec=home_spec,
            source=args.power_source,
        )
        _write_backtest_result(
            result,
            args.output,
            args.artifact_dir,
        )
        json.dump(result.report.to_dict(), sys.stdout, indent=2)
        sys.stdout.write("\n")
    except CalibrationError as exc:
        print(f"thermal backtest: {exc}", file=sys.stderr)
        raise SystemExit(2) from exc


if __name__ == "__main__":
    main()
