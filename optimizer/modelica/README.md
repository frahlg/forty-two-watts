# FTW thermal reference FMU

`FTW.HomeThermalTwin` and `FTW.TwoNodeHomeThermalTwin` are the physical
references for the optimizer's `ftw-1r1c-v1` and `ftw-2r2c-v1` models. The
second model separates fast indoor air from slow building mass. Both use the
same COP curve, residual heat input, and FTW site power balance.

Build it with OpenModelica 1.27:

```bash
cd optimizer/modelica
omc build_fmu.mos
```

The script exports an FMI 2.0 Co-Simulation FMU with CVODE. The optimizer does
not need OpenModelica or the FMU at run time. It uses the matching reduced model
for planning, while the FMU serves replay, model checks, and later shadow tests.
On first use, OpenModelica installs its pinned Modelica 4.1 library.

Validate and run both results with FMPy:

```bash
python -m pip install fmpy==0.3.30
python verify_fmu.py HomeThermalTwin.fmu TwoNodeHomeThermalTwin.fmu
```

Calibrate the reduced model from regular telemetry:

```bash
ftw-optimizer-calibrate-thermal telemetry.csv \
  --model-id home-zone \
  --cop-at-reference 3.4 \
  --output home-zone.json
```

Compare both model candidates from FTW's read-only series API:

```bash
ftw-optimizer-thermal-backtest export \
  --home-spec home-spec.json \
  --api-base http://homelab-rpi:8080 \
  --days 30 \
  --output thermal-observations.json

ftw-optimizer-thermal-backtest run \
  --home-spec home-spec.json \
  --input thermal-observations.json \
  --output thermal-backtest.json \
  --artifact-dir thermal-artifacts
```

Start from `optimizer/home-spec.example.json`. Each sensor maps a logical input
to one long-format FTW driver metric. Scale and offset fields can normalize a
source without changing its stored history.

The exporter fetches bounded time blocks and keeps the longest complete,
regular run. The comparison retains 1R1C unless 2R2C improves unseen rollout
error by the margins in the home specification.

The CSV needs `timestamp_s` or a time-zoned `timestamp`, plus
`indoor_temp_c`, `outdoor_temp_c`, and `heat_pump_power_w`. A grid meter alone
cannot identify heat-pump input. Use a heat-pump submeter or a checked component
balance. The COP curve must come from maker data or a heat-meter test because
electric power and room temperature cannot separate COP from thermal capacity.

The self-contained model uses only the Modelica Standard Library. A detailed
home model can later replace its internals with Modelica Buildings, AixLib, or
BESMod components while retaining the FMU signal and sign contract.
