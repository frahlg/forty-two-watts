from __future__ import annotations

import math
import sys
from pathlib import Path

import numpy as np
from fmpy import simulate_fmu
from fmpy.validation import validate_fmu


def matrix_exponential(matrix: np.ndarray, duration_h: float) -> np.ndarray:
    values, vectors = np.linalg.eig(matrix)
    inverse = np.linalg.inv(vectors)
    result = vectors @ np.diag(np.exp(values * duration_h)) @ inverse
    return np.real_if_close(result).astype(float)


def verify(path: Path) -> None:
    problems = validate_fmu(path)
    if problems:
        raise SystemExit("\n".join(problems))
    two_node = "TwoNode" in path.stem
    output = [
        "indoorTemperatureC",
        "heatPumpThermalPowerW",
        "cop",
        "gridPowerW",
    ]
    if two_node:
        output.append("massTemperatureC")
    result = simulate_fmu(
        path,
        stop_time=3600,
        output_interval=300,
        start_values={
            "outsideTemperatureC": 0,
            "heatPumpElectricPowerW": 2000,
            "disturbanceHeatW": 350,
            "nativeLoadW": 500,
            "pvPowerW": -1000,
            "batteryPowerW": 0,
        },
        output=output,
    )
    final = result[-1]
    expected_cop = 3.4 + 0.05 * (0 - 7)
    if not math.isclose(final["cop"], expected_cop, rel_tol=1e-6):
        raise SystemExit(f"unexpected COP {final['cop']}")
    if not math.isclose(
        final["heatPumpThermalPowerW"],
        expected_cop * 2000,
        rel_tol=1e-6,
    ):
        raise SystemExit(
            f"unexpected heat-pump heat {final['heatPumpThermalPowerW']}"
        )
    if not math.isclose(final["gridPowerW"], 1500, abs_tol=1e-6):
        raise SystemExit(f"unexpected grid power {final['gridPowerW']}")
    if two_node:
        heat_loss = 160
        coupling = 900
        air_capacity = 1200
        mass_capacity = 14_000
        continuous = np.asarray(
            [
                [
                    -(heat_loss + coupling) / air_capacity,
                    coupling / air_capacity,
                ],
                [
                    coupling / mass_capacity,
                    -coupling / mass_capacity,
                ],
            ]
        )
        forcing = np.asarray(
            [
                (
                    heat_loss * 0
                    + expected_cop * 2000
                    + 350
                )
                / air_capacity,
                0,
            ]
        )
        equilibrium = np.linalg.solve(continuous, -forcing)
        initial = np.asarray([20.5, 20.0])
        expected = equilibrium + matrix_exponential(
            continuous,
            1,
        ) @ (initial - equilibrium)
        expected_indoor_c = float(expected[0])
        if not math.isclose(
            final["massTemperatureC"],
            float(expected[1]),
            abs_tol=1e-3,
        ):
            raise SystemExit(
                f"unexpected mass temperature {final['massTemperatureC']}"
            )
    else:
        decay = math.exp(-(180 / 12_000))
        equilibrium_c = (expected_cop * 2000 + 350) / 180
        expected_indoor_c = decay * 20.5 + (1 - decay) * equilibrium_c
    if not math.isclose(
        final["indoorTemperatureC"],
        expected_indoor_c,
        abs_tol=1e-3 if two_node else 1e-4,
    ):
        raise SystemExit(
            f"unexpected indoor temperature {final['indoorTemperatureC']}"
        )
    print(
        f"{path.name} valid:"
        f" COP={final['cop']:.3f},"
        f" grid={final['gridPowerW']:.1f} W,"
        f" indoor={final['indoorTemperatureC']:.3f} C"
    )


def main() -> None:
    if len(sys.argv) < 2:
        raise SystemExit("usage: verify_fmu.py PATH [PATH ...]")
    for value in sys.argv[1:]:
        verify(Path(value))


if __name__ == "__main__":
    main()
