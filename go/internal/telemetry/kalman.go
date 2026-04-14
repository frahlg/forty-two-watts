// Package telemetry is the central DER data store plus per-signal Kalman
// smoothing and per-driver health tracking.
package telemetry

// KalmanFilter1D is a scalar Kalman filter used to smooth noisy power
// readings. Adapts automatically: noisy signal → trusts prediction more,
// stable signal → trusts measurement more.
type KalmanFilter1D struct {
	// Estimate is the current smoothed value
	Estimate float64
	// Uncertainty grows with process noise and shrinks on measurement
	Uncertainty float64
	// ProcessNoise models the expected change between samples
	ProcessNoise float64
	// MeasurementNoise models the noise floor of the sensor
	MeasurementNoise float64

	initialized bool
}

// NewKalman returns a fresh filter. process ≈ how much the true value changes
// between samples (e.g. 100W/cycle for power); measurement ≈ sensor noise
// (e.g. 50W for a Modbus meter).
func NewKalman(process, measurement float64) *KalmanFilter1D {
	return &KalmanFilter1D{
		ProcessNoise:     process,
		MeasurementNoise: measurement,
		Uncertainty:      1000, // start uncertain
	}
}

// Update feeds a new measurement and returns the filtered estimate.
func (k *KalmanFilter1D) Update(measurement float64) float64 {
	if !k.initialized {
		k.Estimate = measurement
		k.Uncertainty = k.MeasurementNoise
		k.initialized = true
		return measurement
	}
	// Predict step
	predUnc := k.Uncertainty + k.ProcessNoise
	// Kalman gain
	gain := predUnc / (predUnc + k.MeasurementNoise)
	// Update
	k.Estimate += gain * (measurement - k.Estimate)
	k.Uncertainty = (1 - gain) * predUnc
	return k.Estimate
}

// Initialized reports whether the filter has seen any measurements.
func (k *KalmanFilter1D) Initialized() bool { return k.initialized }
