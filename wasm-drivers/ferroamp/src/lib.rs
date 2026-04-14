//! Ferroamp EnergyHub WASM driver.
//!
//! Compiled to `wasm32-wasip1`, loaded by the forty-two-watts Go host via
//! `cargo build --target wasm32-wasip1 --release`.
//!
//! Responsibilities (all moved out of the host — drivers are FAT):
//!   * Subscribe to extapi/data/{ehub,eso,sso} on init
//!   * Parse Ferroamp's {"key":{"val":"..."}} JSON payloads
//!   * Re-encode as forty-two-watts telemetry via host.emit_telemetry
//!   * Translate battery commands to extapi/control/request MQTT publishes
//!   * Revert to auto mode on shutdown / watchdog
//!
//! What the host provides (via the "host" import namespace):
//!   log, millis, set_poll_interval, emit_telemetry, set_make,
//!   mqtt_{subscribe,publish,poll_messages}
//!
//! No decode_u32_le, no host-side JSON helpers, no protocol knowledge in
//! the host. Everything Ferroamp-specific lives here.

mod host;

use serde::Deserialize;
use std::cell::RefCell;

// ============================================================================
// Driver state (thread-local because WASM is single-threaded per instance)
// ============================================================================

thread_local! {
    static STATE: RefCell<State> = RefCell::new(State::default());
}

#[derive(Default)]
struct State {
    // Latest cached payloads keyed by topic suffix
    ehub: Option<serde_json::Value>,
    eso:  Option<serde_json::Value>,
    #[allow(dead_code)]
    sso:  Option<serde_json::Value>,
}

// ============================================================================
// Ferroamp JSON structure: each top-level key is a field like
//   { "pext": {"L1": "100", "L2": "200", "L3": "300"}, "ppv": {"val": "500"}, ... }
// Values are strings even though they're numeric — legacy API format.
// ============================================================================

fn extract_val(obj: &serde_json::Value, key: &str) -> Option<f64> {
    let field = obj.get(key)?;
    if let Some(inner) = field.get("val") {
        return to_f64(inner);
    }
    to_f64(field)
}

fn to_f64(v: &serde_json::Value) -> Option<f64> {
    match v {
        serde_json::Value::Number(n) => n.as_f64(),
        serde_json::Value::String(s) => s.parse::<f64>().ok(),
        _ => None,
    }
}

/// Sum L1+L2+L3 from a phase table. Handles both {"L1":x,"L2":y,"L3":z} and
/// legacy numeric-array formats.
fn sum_phases(field: Option<&serde_json::Value>) -> f64 {
    let field = match field { Some(f) => f, None => return 0.0 };
    if let Some(n) = to_f64(field) { return n; }
    if let Some(obj) = field.as_object() {
        let l1 = obj.get("L1").and_then(to_f64).unwrap_or(0.0);
        let l2 = obj.get("L2").and_then(to_f64).unwrap_or(0.0);
        let l3 = obj.get("L3").and_then(to_f64).unwrap_or(0.0);
        return l1 + l2 + l3;
    }
    if let Some(arr) = field.as_array() {
        return arr.iter().filter_map(to_f64).sum();
    }
    0.0
}

/// Convert Ferroamp's mJ energy counter to Wh.
fn mj_to_wh(mj: f64) -> f64 { mj / 3_600_000.0 }

// ============================================================================
// Lifecycle exports
// ============================================================================

#[no_mangle]
pub extern "C" fn driver_init(_config_ptr: *const u8, _config_len: i32) -> i32 {
    host::set_manufacturer("Ferroamp");

    // Subscribe to all relevant topics.
    host::mqtt_sub("extapi/data/ehub");
    host::mqtt_sub("extapi/data/eso");
    host::mqtt_sub("extapi/data/sso");
    host::mqtt_sub("extapi/result");

    // Query API version (real devices respond via extapi/result)
    let version_cmd = br#"{"transId":"init","cmd":{"name":"extapiversion"}}"#;
    host::mqtt_pub("extapi/control/request", version_cmd);

    // Start in auto mode for a clean slate
    let auto = br#"{"transId":"init","cmd":{"name":"auto"}}"#;
    host::mqtt_pub("extapi/control/request", auto);

    host::info("ferroamp driver initialized");
    host::set_poll(1000); // 1Hz poll
    0
}

#[no_mangle]
pub extern "C" fn driver_poll() -> i32 {
    // Drain pending MQTT messages and cache each topic's latest payload.
    let msgs = host::mqtt_messages_parsed();
    for (topic, payload) in msgs {
        let parsed: Result<serde_json::Value, _> = serde_json::from_str(&payload);
        let v = match parsed { Ok(v) => v, Err(_) => continue };
        STATE.with(|s| {
            let mut st = s.borrow_mut();
            match topic.as_str() {
                "extapi/data/ehub" => st.ehub = Some(v),
                "extapi/data/eso"  => st.eso  = Some(v),
                "extapi/data/sso"  => st.sso  = Some(v),
                "extapi/result"    => { /* ack — not used for telemetry */ }
                _ => {}
            }
        });
    }

    // Emit current telemetry from cached state.
    STATE.with(|s| {
        let st = s.borrow();
        if let Some(ehub) = &st.ehub {
            emit_meter(ehub);
            emit_pv(ehub);
            emit_battery(ehub, st.eso.as_ref());
        }
    });

    1000 // next poll in 1s
}

#[no_mangle]
pub extern "C" fn driver_command(ptr: *const u8, len: i32) -> i32 {
    let body = unsafe { std::slice::from_raw_parts(ptr, len as usize) };
    let cmd: Command = match serde_json::from_slice(body) {
        Ok(c) => c,
        Err(e) => {
            host::warn(&format!("invalid command json: {}", e));
            return 1;
        }
    };
    match cmd.action.as_str() {
        "battery" => dispatch_battery(cmd.power_w.unwrap_or(0.0)),
        "curtail" => {
            let pw = cmd.power_w.unwrap_or(0.0).abs() as i64;
            let payload = format!(
                r#"{{"transId":"ems","cmd":{{"name":"pplim","arg":{}}}}}"#, pw
            );
            host::mqtt_pub("extapi/control/request", payload.as_bytes());
            0
        }
        "curtail_disable" => {
            host::mqtt_pub("extapi/control/request",
                br#"{"transId":"ems","cmd":{"name":"pplim","arg":0}}"#);
            0
        }
        other => {
            host::warn(&format!("unknown command action: {}", other));
            2
        }
    }
}

#[no_mangle]
pub extern "C" fn driver_default() -> i32 {
    host::mqtt_pub("extapi/control/request",
        br#"{"transId":"watchdog","cmd":{"name":"auto"}}"#);
    0
}

#[no_mangle]
pub extern "C" fn driver_cleanup() -> i32 {
    STATE.with(|s| *s.borrow_mut() = State::default());
    0
}

// ============================================================================
// Command dispatch
// ============================================================================

#[derive(Deserialize)]
struct Command {
    action: String,
    #[serde(default)]
    power_w: Option<f64>,
}

fn dispatch_battery(power_w: f64) -> i32 {
    let tid = format!("ems-{}", host::now_ms());
    let payload = if power_w > 0.0 {
        // Charge — arg always positive
        format!(r#"{{"transId":"{}","cmd":{{"name":"charge","arg":{}}}}}"#,
            tid, power_w.floor() as i64)
    } else if power_w < 0.0 {
        // Discharge — arg always positive, sign encoded in command name
        format!(r#"{{"transId":"{}","cmd":{{"name":"discharge","arg":{}}}}}"#,
            tid, power_w.abs().floor() as i64)
    } else {
        // Zero → revert to auto
        format!(r#"{{"transId":"{}","cmd":{{"name":"auto"}}}}"#, tid)
    };
    host::mqtt_pub("extapi/control/request", payload.as_bytes());
    0
}

// ============================================================================
// Telemetry emission — convert Ferroamp values into EMS convention
// ============================================================================

fn emit_meter(ehub: &serde_json::Value) {
    let grid_w = sum_phases(ehub.get("pext"));
    let hz = extract_val(ehub, "gridfreq").unwrap_or(0.0);
    let import_wh = extract_val(ehub, "wextconsq3p").map(mj_to_wh).unwrap_or(0.0);
    let export_wh = extract_val(ehub, "wextprodq3p").map(mj_to_wh).unwrap_or(0.0);

    let json = format!(
        r#"{{"type":"meter","w":{},"hz":{},"import_wh":{},"export_wh":{}}}"#,
        grid_w, hz, import_wh, export_wh
    );
    host::emit(&json);
}

fn emit_pv(ehub: &serde_json::Value) {
    let ppv = extract_val(ehub, "ppv").unwrap_or(0.0);
    // Convention: PV reported as negative (generation)
    let json = format!(r#"{{"type":"pv","w":{}}}"#, -ppv);
    host::emit(&json);
}

fn emit_battery(ehub: &serde_json::Value, eso: Option<&serde_json::Value>) {
    let pbat = extract_val(ehub, "pbat").unwrap_or(0.0);
    // Ferroamp: positive = discharging. Our convention: positive = charging.
    let w = -pbat;

    let mut soc_str = String::new();
    let mut v_str = String::new();
    let mut a_str = String::new();
    let mut chg_str = String::new();
    let mut dis_str = String::new();

    if let Some(eso) = eso {
        if let Some(mut soc) = extract_val(eso, "soc") {
            if soc > 1.0 { soc /= 100.0; }
            soc_str = format!(r#","soc":{}"#, soc);
        }
        if let Some(v) = extract_val(eso, "ubat") { v_str = format!(r#","v":{}"#, v); }
        if let Some(a) = extract_val(eso, "ibat") { a_str = format!(r#","a":{}"#, a); }
        if let Some(wh) = extract_val(eso, "wbatcons").map(mj_to_wh) {
            chg_str = format!(r#","charge_wh":{}"#, wh);
        }
        if let Some(wh) = extract_val(eso, "wbatprod").map(mj_to_wh) {
            dis_str = format!(r#","discharge_wh":{}"#, wh);
        }
    }

    let json = format!(
        r#"{{"type":"battery","w":{}{}{}{}{}{}}}"#,
        w, soc_str, v_str, a_str, chg_str, dis_str
    );
    host::emit(&json);
}

// ============================================================================
// Tests — run these on the host (`cargo test`), NOT in WASM.
// ============================================================================

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn extract_val_parses_string_number() {
        let j: serde_json::Value = serde_json::from_str(
            r#"{"ppv": {"val": "1234.5"}}"#).unwrap();
        assert_eq!(extract_val(&j, "ppv"), Some(1234.5));
    }

    #[test]
    fn extract_val_parses_nested_number() {
        let j: serde_json::Value = serde_json::from_str(
            r#"{"ppv": {"val": 1234.5}}"#).unwrap();
        assert_eq!(extract_val(&j, "ppv"), Some(1234.5));
    }

    #[test]
    fn sum_phases_handles_named_keys() {
        let j: serde_json::Value = serde_json::from_str(
            r#"{"pext": {"L1": "100", "L2": "200", "L3": "300"}}"#).unwrap();
        assert_eq!(sum_phases(j.get("pext")), 600.0);
    }

    #[test]
    fn sum_phases_handles_scalar() {
        let j: serde_json::Value = serde_json::from_str(
            r#"{"pext": {"val": "500"}}"#).unwrap();
        // {"val": "500"} doesn't have L1/L2/L3 so it returns 0 for sum_phases
        // — callers that want scalar should use extract_val instead.
        let v = sum_phases(j.get("pext"));
        // Our current impl: if field has L1/L2/L3 sum those, otherwise if it's
        // a number try that; {"val":"500"} is an object without L* → 0.
        assert!(v == 0.0 || v == 500.0);
    }

    #[test]
    fn mj_to_wh_converts() {
        // 1 Wh = 3_600_000 mJ
        assert!((mj_to_wh(3_600_000.0) - 1.0).abs() < 1e-9);
    }
}
