package main

import (
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

func newControlStateFromConfig(cfg *config.Config) *control.State {
	ctrl := control.NewState(cfg.Site.GridTargetW, cfg.Site.GridToleranceW, cfg.SiteMeterDriver())
	if cfg.Site.Gain != 0 {
		ctrl.PI.Kp = cfg.Site.Gain
	}
	ctrl.SlewRateW = cfg.Site.SlewRateW
	// applyDefaults() ensures SlewEnabled is non-nil at this point.
	if cfg.Site.SlewEnabled != nil {
		ctrl.SlewEnabled = *cfg.Site.SlewEnabled
	}
	ctrl.MinDispatchIntervalS = cfg.Site.MinDispatchIntervalS
	ctrl.InverterGroups = inverterGroupsFrom(cfg.Drivers)
	ctrl.SupportsPVCurtail = supportsPVCurtailFrom(cfg.Drivers)
	ctrl.DriverLimits = driverLimitsFrom(cfg.Drivers, cfg.Batteries)
	// Per-phase fuse params for the per-phase clamp inside applyFuseGuard
	// + forceFuseDischarge. Reads l1_a/l2_a/l3_a from the meter driver
	// when SiteFuseAmps > 0; otherwise the per-phase clamp is disabled.
	ctrl.SiteFuseAmps = cfg.Fuse.MaxAmps
	ctrl.SiteFuseVoltage = cfg.Fuse.Voltage
	ctrl.SiteFusePhases = cfg.Fuse.Phases
	// EffectiveSafetyMarginA distinguishes nil ("unset, use default")
	// from explicit 0 ("operator chose to disable"). The earlier
	// `<= 0 -> default` shortcut clobbered the disable case.
	ctrl.SiteFuseSafetyA = cfg.Fuse.EffectiveSafetyMarginA()
	// PV surplus absorber underlay (opt-in). cap == 0 keeps it off.
	ctrl.PVSurplusAbsorbSoCCapPct = cfg.Site.PVSurplusAbsorbSoCCapPct
	ctrl.PVSurplusAbsorbThresholdW = cfg.Site.PVSurplusAbsorbThresholdW
	// DC-link protective curtail — opt-in, default off. SoC threshold
	// and margin fall back to dispatch defaults (0.80 / 1000 W) when
	// unset, applied inside ComputePVCurtail.
	ctrl.DCLinkProtectionEnabled = cfg.Site.DCLinkProtectionEnabled
	ctrl.DCLinkProtectionSoCThreshold = cfg.Site.DCLinkProtectionSoCThreshold
	ctrl.DCLinkProtectionMarginW = cfg.Site.DCLinkProtectionMarginW
	// Site export ceiling — opt-in, default off. The fuse guard scales
	// battery discharge back so predicted export stays under max_export_w,
	// protecting inverters that trip below the breaker rating.
	ctrl.MaxExportW = cfg.Site.MaxExportW
	// Site-level fallback command cap. 0 = keep the 5 kW MaxCommandW
	// constant; >5 kW requires profile: commercial (config validation).
	ctrl.DefaultCommandW = cfg.Site.MaxCommandW
	// C&I: NMD as a contractual import ceiling (kVA → W via the assumed
	// power factor) and the load-shedding backup reserve floor.
	if cfg.Site.NMDkVA > 0 {
		ctrl.NMDImportCeilingW = cfg.Site.NMDkVA * 1000 * cfg.Site.EffectivePowerFactor()
	}
	if br := cfg.Site.BackupReserve; br != nil {
		ctrl.BackupReserveWh = br.MinUsableEnergyWh
	}
	return ctrl
}
