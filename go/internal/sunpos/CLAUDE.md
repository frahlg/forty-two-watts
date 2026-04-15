# sunpos — solar position and plane-of-array irradiance

## What it does

Pure-physics solar geometry. Computes sun zenith/azimuth at a given lat/lon,
clear-sky horizontal irradiance, and plane-of-array irradiance for an
arbitrary tilt/azimuth panel. No fitted constants. Foundation for the
upcoming auto-PV flow (per-array orientation fitted from a few clear-sky
days) — currently unused in production; `pvmodel` still consumes the simpler
`clearSkyW` from the forecast module.

## Math

Spencer (1971) series (`sunpos.go:36-47`). Accuracy ~0.05°, enough for
getting the shape of the day right on an embedded EMS:

```
gamma = 2*pi*(doy - 1 + (hour-12)/24) / 365
EoT   = 229.18*(0.000075
              + 0.001868*cos(g)  - 0.032077*sin(g)
              - 0.014615*cos(2g) - 0.040849*sin(2g))      [minutes]
decl  = 0.006918
      - 0.399912*cos(g)  + 0.070257*sin(g)
      - 0.006758*cos(2g) + 0.000907*sin(2g)
      - 0.002697*cos(3g) + 0.00148*sin(3g)                [radians]
```

Hour angle (`sunpos.go:50-54`): `ha = (tst/4 - 180) * pi/180` where
`tst = hour*60 + EoT + 4*lon`.

Zenith (`sunpos.go:58-62`):

```
cos(z) = sin(lat)*sin(decl) + cos(lat)*cos(decl)*cos(ha)
```

Azimuth via `atan2`, north-zero clockwise (`sunpos.go:70-74`):

```
az = atan2(-sin(ha), tan(decl)*cos(lat) - sin(lat)*cos(ha))
```

Negative results wrapped to [0, 2π).

Angle of incidence for a panel at (tilt, pAz) (`sunpos.go:95-99`):

```
cos(aoi) = cos(z)*cos(tilt) + sin(z)*sin(tilt)*cos(sAz - pAz)
```

Clear-sky horizontal irradiance (`sunpos.go:117-124`) — extraterrestrial
flux scaled by orbital distance (Spencer) and an atmospheric transmissivity
`tau = 0.7`:

```
e0  = 1.000110 + 0.034221*cos(g) + 0.001280*sin(g)
               + 0.000719*cos(2g) + 0.000077*sin(2g)
DNI = I0 * e0                       (I0 = 1361 W/m^2)
GHI = DNI * tau * cos(z)            (0 when sun below horizon)
```

POA with isotropic-sky diffuse + Erbs split (`sunpos.go:133-159`):

```
kd  = 0.2                     (fixed — we only have modeled GHI)
DHI = GHI * kd
DNI = (GHI - DHI) / cos(z)
POA = DNI*cos(aoi) + DHI*(1 + cos(tilt))/2     (aoi <= 90)
POA = DHI*(1 + cos(tilt))/2                    (aoi  > 90, sun behind)
```

## Inputs / outputs

- `At(t, lat, lon) Position` — time in UTC, lat °N, lon °E. Returns
  zenith + azimuth in degrees.
- `AOI(sun, panelTiltDeg, panelAzDeg) float64` — degrees, in [0, 180].
- `ClearSkyW(t, lat, lon) float64` — W/m² horizontal, non-negative.
- `POA(t, lat, lon, panelTiltDeg, panelAzDeg) float64` — W/m² on panel.

## Training cadence + persistence

None. Pure deterministic function of `(time, lat, lon, tilt, azimuth)`.
No state to persist, no samples to train on.

## Public API surface

- `type Position struct { ZenithDeg, AzimuthDeg float64 }`
- `At(t time.Time, lat, lon float64) Position`
- `AOI(sun Position, panelTiltDeg, panelAzDeg float64) float64`
- `ClearSkyW(t time.Time, lat, lon float64) float64`
- `POA(t time.Time, lat, lon, panelTiltDeg, panelAzDeg float64) float64`

## How it talks to neighbors

- `main.go` wires `ClearSkyW` behind `pvmodel.ClearSkyFunc` (or leaves the
  naive forecast value; both signatures match).
- Intended consumer of `POA`: the upcoming auto-PV fit — on clear-sky days
  the predicted POA per candidate (tilt, azimuth) is regressed against
  measured output to recover orientation per string.
- `sunpos_test.go` has 5 tests (zenith at equinox noon, azimuth morning vs
  afternoon, ClearSkyW sign, POA south-facing at noon, POA sun-behind
  bound).

## What NOT to do

- Do not fit `tau` or `kd` to local data here. If site-specific
  transmissivity matters, learn it in `pvmodel` where RLS already absorbs
  the constant into `beta[2]`.
- Do not pass local time — `At` expects UTC internally (`t.UTC()` is
  called). Passing local-zone times will shift the day by up to 12 h.
- Do not use `POA` as the PV forecast yet — the production pipeline goes
  through `forecast` + `pvmodel`. `POA` is scaffolding for auto-PV.
- Do not reorder the azimuth convention. `atan2(-sin(ha), …)` puts 0 at
  north clockwise; any change silently breaks the PV orientation math
  downstream.
