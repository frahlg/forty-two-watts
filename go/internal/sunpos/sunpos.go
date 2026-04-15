// Package sunpos computes solar position (zenith/azimuth) and plane-of-array
// irradiance for arbitrary panel orientations. Physics-only, no fitted
// constants — used as the prior for the data-driven PV twin.
//
// Reference: Reda & Andreas (2003), "Solar Position Algorithm for Solar
// Radiation Applications", NREL/TP-560-34302. We use the simplified
// SPENCER (1971) form which is accurate to ~0.05° and avoids planetary
// ephemeris tables — perfect for an embedded EMS that just needs the
// shape of the day right.
package sunpos

import (
	"math"
	"time"
)

// Position is the apparent location of the sun seen from one point on Earth.
//
//	Zenith  = 0° → sun directly overhead, 90° → at horizon, >90° → below.
//	Azimuth = 0° → north, 90° → east, 180° → south, 270° → west (clockwise).
type Position struct {
	ZenithDeg  float64
	AzimuthDeg float64
}

// At returns the sun position at time t for an observer at (lat, lon).
// Time is interpreted in UTC. lat in degrees north, lon in degrees east.
func At(t time.Time, lat, lon float64) Position {
	// Day of year as fractional value (used by Spencer's series).
	utc := t.UTC()
	doy := float64(utc.YearDay())
	hour := float64(utc.Hour()) + float64(utc.Minute())/60 + float64(utc.Second())/3600

	// Spencer (1971) Fourier expansion of solar declination + eqn-of-time.
	// gamma = fractional year in radians.
	gamma := 2 * math.Pi * (doy - 1 + (hour-12)/24) / 365

	// Equation of time (minutes): correction for orbital eccentricity.
	eqt := 229.18 * (0.000075 +
		0.001868*math.Cos(gamma) - 0.032077*math.Sin(gamma) -
		0.014615*math.Cos(2*gamma) - 0.040849*math.Sin(2*gamma))

	// Solar declination (radians).
	decl := 0.006918 -
		0.399912*math.Cos(gamma) + 0.070257*math.Sin(gamma) -
		0.006758*math.Cos(2*gamma) + 0.000907*math.Sin(2*gamma) -
		0.002697*math.Cos(3*gamma) + 0.00148*math.Sin(3*gamma)

	// Solar time (minutes): UTC clock + longitude offset + EoT.
	timeOffset := eqt + 4*lon
	tst := hour*60 + timeOffset

	// Hour angle: 0 at solar noon, +15° per hour east → west.
	ha := (tst/4 - 180) * math.Pi / 180

	latR := lat * math.Pi / 180

	// Zenith.
	cosZ := math.Sin(latR)*math.Sin(decl) + math.Cos(latR)*math.Cos(decl)*math.Cos(ha)
	if cosZ > 1 { cosZ = 1 }
	if cosZ < -1 { cosZ = -1 }
	zenith := math.Acos(cosZ)

	// Azimuth via atan2.
	// Standard form (NOAA): atan2(-sin(ha), tan(decl)*cos(lat) - sin(lat)*cos(ha))
	// returns azimuth measured from NORTH clockwise. At solar noon (ha=0) in
	// the northern hemisphere with sun south of the observer, the
	// denominator is negative → atan2 returns π → 180°. Morning (ha<0) →
	// num positive → az 90°-180° (sun in east). Afternoon → 180°-270°.
	sinHa := math.Sin(ha)
	num := -sinHa
	den := math.Tan(decl)*math.Cos(latR) - math.Sin(latR)*math.Cos(ha)
	az := math.Atan2(num, den)
	if az < 0 { az += 2 * math.Pi }

	return Position{
		ZenithDeg:  zenith * 180 / math.Pi,
		AzimuthDeg: az * 180 / math.Pi,
	}
}

// AOI returns the angle of incidence between the sun's rays and the normal
// of a panel with the given (tiltDeg, azimuthDeg). Used to project DNI on
// the plane of array.
//
// tilt:  0° = horizontal, 90° = vertical
// az:    0° = north-facing, 90° = east, 180° = south, 270° = west
//
// Returns AOI in degrees in [0, 180]. AOI > 90° means sun is behind panel.
func AOI(sun Position, panelTiltDeg, panelAzDeg float64) float64 {
	zR := sun.ZenithDeg * math.Pi / 180
	sR := sun.AzimuthDeg * math.Pi / 180
	tR := panelTiltDeg * math.Pi / 180
	pR := panelAzDeg * math.Pi / 180
	cosAOI := math.Cos(zR)*math.Cos(tR) +
		math.Sin(zR)*math.Sin(tR)*math.Cos(sR-pR)
	if cosAOI > 1 { cosAOI = 1 }
	if cosAOI < -1 { cosAOI = -1 }
	return math.Acos(cosAOI) * 180 / math.Pi
}

// ClearSkyW returns extraterrestrial irradiance on a horizontal surface
// in W/m² (the "what the sun could deliver if there was no atmosphere").
// Multiplied by an atmospheric transmissivity factor (0.7 default) to
// approximate ground-level clear-sky GHI. Used as the prior signal for
// the PV twin.
//
// Returns 0 when the sun is below the horizon.
func ClearSkyW(t time.Time, lat, lon float64) float64 {
	sun := At(t, lat, lon)
	if sun.ZenithDeg >= 90 {
		return 0
	}
	// Solar constant adjusted for orbital distance (Spencer 1971).
	doy := float64(t.UTC().YearDay())
	gamma := 2 * math.Pi * (doy - 1) / 365
	e0 := 1.000110 +
		0.034221*math.Cos(gamma) + 0.001280*math.Sin(gamma) +
		0.000719*math.Cos(2*gamma) + 0.000077*math.Sin(2*gamma)
	const I0 = 1361.0 // solar constant W/m²
	dni := I0 * e0
	// Atmospheric transmissivity (Bird simple model uses ~0.75 average).
	const tau = 0.7
	return dni * tau * math.Cos(sun.ZenithDeg*math.Pi/180)
}

// POA estimates plane-of-array irradiance for one tilted panel using the
// isotropic-sky model. Splits clear-sky horizontal irradiance into beam
// (DNI) and diffuse (DHI) components via a simple Erbs correlation, then
// projects each onto the panel.
//
// Returns W/m² on the panel surface; clamped to ≥ 0.
func POA(t time.Time, lat, lon, panelTiltDeg, panelAzDeg float64) float64 {
	sun := At(t, lat, lon)
	if sun.ZenithDeg >= 90 {
		return 0
	}
	ghi := ClearSkyW(t, lat, lon)
	if ghi <= 0 {
		return 0
	}
	// Erbs et al. (1982) clearness-based diffuse fraction. We don't have
	// real GHI measurements, so kt comes from our own clear-sky model →
	// always ~0.7-0.75 → diffuse ratio ~0.2. Conservative.
	kd := 0.2
	dhi := ghi * kd
	dni := (ghi - dhi) / math.Cos(sun.ZenithDeg*math.Pi/180)

	aoi := AOI(sun, panelTiltDeg, panelAzDeg)
	if aoi > 90 {
		// Sun behind panel — only diffuse counts.
		return dhi * (1 + math.Cos(panelTiltDeg*math.Pi/180)) / 2
	}
	beamPOA := dni * math.Cos(aoi*math.Pi/180)
	diffusePOA := dhi * (1 + math.Cos(panelTiltDeg*math.Pi/180)) / 2
	out := beamPOA + diffusePOA
	if out < 0 { out = 0 }
	return out
}
