use mlua::{Lua, Result as LuaResult, Table, Value};

/// Allowed global names in the sandbox.
const ALLOWED_GLOBALS: &[&str] = &[
    // Core language
    "assert",
    "error",
    "ipairs",
    "next",
    "pairs",
    "pcall",
    "xpcall",
    "rawequal",
    "rawget",
    "rawlen",
    "rawset",
    "select",
    "tonumber",
    "tostring",
    "type",
    "unpack",
    // Safe standard libraries
    "math",
    "string",
    "table",
    "utf8",
    // Print (redirected to host.log in drivers)
    "print",
];

/// Globals that MUST be removed for sandbox safety.
const BLOCKED_GLOBALS: &[&str] = &[
    "io",
    "os",
    "debug",
    "dofile",
    "loadfile",
    "require",
    "collectgarbage",
    "load", // could load bytecode
];

/// Apply sandbox restrictions to the Lua VM.
pub fn apply_sandbox(lua: &Lua) -> LuaResult<()> {
    let globals = lua.globals();

    for name in BLOCKED_GLOBALS {
        globals.raw_set(*name, Value::Nil)?;
    }

    Ok(())
}

/// Create an isolated per-driver environment table.
/// The environment inherits allowed globals via __index on a shared base,
/// but each driver gets its own write space.
pub fn create_driver_env(lua: &Lua, driver_name: &str) -> LuaResult<Table> {
    // Build a read-only base table with only allowed globals
    let base = lua.create_table()?;
    let globals = lua.globals();

    for name in ALLOWED_GLOBALS {
        if let Ok(val) = globals.raw_get::<Value>(*name) {
            if val != Value::Nil {
                base.raw_set(*name, val)?;
            }
        }
    }

    // Add safe `load` that only accepts text (no bytecode)
    let safe_load = lua.create_function(|lua, code: String| lua.load(&code).into_function())?;
    base.raw_set("load", safe_load)?;

    // Create the driver's private environment
    let env = lua.create_table()?;

    // Store driver name in the env
    env.raw_set("__driver_name", driver_name)?;

    // Set __index to fall through to base globals
    let meta = lua.create_table()?;
    meta.raw_set("__index", base)?;
    let _ = env.set_metatable(Some(meta));

    Ok(env)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_sandbox_blocks_io() {
        let lua = Lua::new();
        apply_sandbox(&lua).unwrap();
        let result: LuaResult<Value> = lua.globals().get("io");
        assert!(matches!(result, Ok(Value::Nil)));
    }

    #[test]
    fn test_sandbox_blocks_os() {
        let lua = Lua::new();
        apply_sandbox(&lua).unwrap();
        let result: LuaResult<Value> = lua.globals().get("os");
        assert!(matches!(result, Ok(Value::Nil)));
    }

    #[test]
    fn test_driver_env_isolation() {
        let lua = Lua::new();
        apply_sandbox(&lua).unwrap();
        let env = create_driver_env(&lua, "test_driver").unwrap();

        // Can access allowed globals through __index
        let math: Table = env.get("math").unwrap();
        let pi: f64 = math.get("pi").unwrap();
        assert!((pi - std::f64::consts::PI).abs() < 0.001);

        // Can set own variables
        env.set("my_var", 42).unwrap();
        let val: i32 = env.get("my_var").unwrap();
        assert_eq!(val, 42);
    }
}
