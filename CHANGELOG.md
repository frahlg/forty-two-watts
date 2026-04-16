## [1.1.0](https://github.com/erikarenhill/forty-two-watts/compare/v1.0.1...v1.1.0) (2026-04-16)

### Bug Fixes

* 5 Go-side P1 bugs from Codex review ([#46](https://github.com/erikarenhill/forty-two-watts/issues/46)) ([0cd2885](https://github.com/erikarenhill/forty-two-watts/commit/0cd2885bdb79d6a4c3116bb4930ec785cea8f944))
* 5 Go-side P1 bugs from Codex review ([#47](https://github.com/erikarenhill/forty-two-watts/issues/47)) ([4f2eaf6](https://github.com/erikarenhill/forty-two-watts/commit/4f2eaf69f626caddf2bae456ac047301f9a36840))
* **solaredge_pv:** read SunSpec scale factors every poll, not cached ([#38](https://github.com/erikarenhill/forty-two-watts/issues/38)) ([26f8793](https://github.com/erikarenhill/forty-two-watts/commit/26f8793f22888dc11d29fd157b10b4340da34c8d))

### Drivers

* fix 9 P1 bugs flagged by Codex review ([#44](https://github.com/erikarenhill/forty-two-watts/issues/44)) ([b20e485](https://github.com/erikarenhill/forty-two-watts/commit/b20e485f5fa0a5a20d3a4e83d49410528f81ea1e))

### UI

* show mode band in plan chart + grid target on status card ([877e0bd](https://github.com/erikarenhill/forty-two-watts/commit/877e0bde83964ddb26ce4894ab0adc446fd7801b))

### Control loop

* slew-rate anchors on actual battery power, not stale command ([#41](https://github.com/erikarenhill/forty-two-watts/issues/41)) ([4f73f19](https://github.com/erikarenhill/forty-two-watts/commit/4f73f19abfb6e322a4934d9e9bb46b645afd1352))

### MPC planner

* log optimize params + ems_mode per action for plan chart ([9e8c14b](https://github.com/erikarenhill/forty-two-watts/commit/9e8c14bd388b869091c2315bd4a42def648bf987))
* value SoC at import−export spread in self-consumption modes ([#40](https://github.com/erikarenhill/forty-two-watts/issues/40)) ([a90d525](https://github.com/erikarenhill/forty-two-watts/commit/a90d5259209ca9fd8094927b060f62633dd3b5d0))

## [1.0.1](https://github.com/erikarenhill/forty-two-watts/compare/v1.0.0...v1.0.1) (2026-04-15)

### Bug Fixes

* **ci:** disable @semantic-release/github PR annotation features ([51eb9e2](https://github.com/erikarenhill/forty-two-watts/commit/51eb9e240c2cac3468169c774a32116600f6b349)), closes [#32](https://github.com/erikarenhill/forty-two-watts/issues/32) [#33](https://github.com/erikarenhill/forty-two-watts/issues/33) [#34](https://github.com/erikarenhill/forty-two-watts/issues/34) [#35](https://github.com/erikarenhill/forty-two-watts/issues/35) [#36](https://github.com/erikarenhill/forty-two-watts/issues/36) [#39](https://github.com/erikarenhill/forty-two-watts/issues/39)

## 1.0.0 (2026-04-15)

### Bug Fixes

* **ci:** switch semantic-release to conventionalcommits preset ([15290c7](https://github.com/erikarenhill/forty-two-watts/commit/15290c7748def3637d649281a6563b55a8503598))
* Lua driver Command() reading wrong field — Sungrow ignored targets ([9237156](https://github.com/erikarenhill/forty-two-watts/commit/923715691d55c9dc5c3058b72271d00a72d9c93a))

### Drivers

* add Eastron SDM630 Lua driver ([#18](https://github.com/erikarenhill/forty-two-watts/issues/18)) ([d5ad806](https://github.com/erikarenhill/forty-two-watts/commit/d5ad8066377371eb63f320969d153ece50d1266a))
* add Ferroamp Modbus driver (alt transport to ferroamp.lua) ([#31](https://github.com/erikarenhill/forty-two-watts/issues/31)) ([03b802c](https://github.com/erikarenhill/forty-two-watts/commit/03b802cefcd1f4d2e07ad05f493ca5643585ed0c))
* port Deye SUN-SG hybrid inverter to 42W v2.1 Lua host ([#29](https://github.com/erikarenhill/forty-two-watts/issues/29)) ([df8fbc0](https://github.com/erikarenhill/forty-two-watts/commit/df8fbc006375dfc2a3abeb2bc8ec0f01f3e1d0e1))
* port Fronius GEN24 (SunSpec) to Lua ([#19](https://github.com/erikarenhill/forty-two-watts/issues/19)) ([c1fc875](https://github.com/erikarenhill/forty-two-watts/commit/c1fc87559b404aa0429ed8ca0a71539e634cb59d))
* port Fronius Smart Meter (SunSpec Modbus, read-only) ([#24](https://github.com/erikarenhill/forty-two-watts/issues/24)) ([575895c](https://github.com/erikarenhill/forty-two-watts/commit/575895c7469283bd139deb481e601068045f7519))
* port GoodWe hybrid inverter (ET-Plus / EH) to Lua v2.1 ([#28](https://github.com/erikarenhill/forty-two-watts/issues/28)) ([e43d2d9](https://github.com/erikarenhill/forty-two-watts/commit/e43d2d92ef1a7fd26c65b839944bc8d98fa4915a))
* port Growatt hybrid inverter driver (read-only) ([#20](https://github.com/erikarenhill/forty-two-watts/issues/20)) ([92524ac](https://github.com/erikarenhill/forty-two-watts/commit/92524acdd890507873a6d5f54b3b6d4335b8e610))
* port Huawei SUN2000 hybrid inverter ([#15](https://github.com/erikarenhill/forty-two-watts/issues/15)) ([09a8855](https://github.com/erikarenhill/forty-two-watts/commit/09a88558d0ae17c7e6bdd26387c663badb55e37b))
* port Kostal Plenticore / Piko IQ (Lua, read-only) ([#21](https://github.com/erikarenhill/forty-two-watts/issues/21)) ([bdeca96](https://github.com/erikarenhill/forty-two-watts/commit/bdeca96e6c3e05cfe968e20ceb298221f2be5c84))
* port Pixii PowerShaper battery driver to v2.1 Lua host ([#22](https://github.com/erikarenhill/forty-two-watts/issues/22)) ([70a96d1](https://github.com/erikarenhill/forty-two-watts/commit/70a96d1120b2aab2cb12ef49688fe3cb204789e3))
* port SMA hybrid inverter Lua driver ([#23](https://github.com/erikarenhill/forty-two-watts/issues/23)) ([dd34555](https://github.com/erikarenhill/forty-two-watts/commit/dd3455577c7a3adebad252f81d40b81d3b982350))
* port Sofar HYD-ES/HYD-EP from hugin to Lua v2.1 ([#26](https://github.com/erikarenhill/forty-two-watts/issues/26)) ([14f6131](https://github.com/erikarenhill/forty-two-watts/commit/14f6131952b033381a5501f76265714a2b985f1c))
* port SolarEdge SunSpec inverter + meter to Lua (read-only) ([#30](https://github.com/erikarenhill/forty-two-watts/issues/30)) ([1007e63](https://github.com/erikarenhill/forty-two-watts/commit/1007e63f9d1908f3210d9b80037e4a6e05e3fa78))
* port Solis hybrid inverter ([#27](https://github.com/erikarenhill/forty-two-watts/issues/27)) ([98b2a50](https://github.com/erikarenhill/forty-two-watts/commit/98b2a50ccf59c45130de951dd22db4fc17a67a1a))
* port Victron Energy GX Modbus driver ([#25](https://github.com/erikarenhill/forty-two-watts/issues/25)) ([ad71db2](https://github.com/erikarenhill/forty-two-watts/commit/ad71db269438e7aa6e11c632ba1db10897db81be))

### UI

* inline target on hover + driver card + collapsible model cards ([de88f43](https://github.com/erikarenhill/forty-two-watts/commit/de88f4326e5aa5587b623cde76371c0f410eff27))
* legend wrap + nice-tick y-axis + cleaner chart labels ([#33](https://github.com/erikarenhill/forty-two-watts/issues/33)) ([aeb1d1c](https://github.com/erikarenhill/forty-two-watts/commit/aeb1d1cb2ab6d69984cdcd424cb6c3da7d775407))
* live version from API + no-cache headers on static assets ([7b8779b](https://github.com/erikarenhill/forty-two-watts/commit/7b8779b08c4572ccb7797bb9a518e48954651f12))

### Control loop

* fold live DerEV readings into the EV clamp ([#36](https://github.com/erikarenhill/forty-two-watts/issues/36)) ([5d57d68](https://github.com/erikarenhill/forty-two-watts/commit/5d57d68c50e6a417b45695bd3ccf551e8566277a))

### MPC planner

* fall back to forecast when learned PV twin collapses ([#39](https://github.com/erikarenhill/forty-two-watts/issues/39)) ([f3062ac](https://github.com/erikarenhill/forty-two-watts/commit/f3062acdd54206de8287b0a9af3862a13cb13105))

### Telemetry

* add DerEV type for EV charger readings ([#34](https://github.com/erikarenhill/forty-two-watts/issues/34)) ([65c9e2c](https://github.com/erikarenhill/forty-two-watts/commit/65c9e2c23b5f3eb7cb55fd952be7e724b2270e17))

### TSDB

* long-format SQLite (14d) + Parquet rolloff for older ([c53c964](https://github.com/erikarenhill/forty-two-watts/commit/c53c964e825c896fc0cf760a21ee7b0e29421d2f))

### Safety

* watchdog marks stale drivers offline + reverts to autonomous ([519196c](https://github.com/erikarenhill/forty-two-watts/commit/519196c01255db3947774bb8a267961b755d261e))
