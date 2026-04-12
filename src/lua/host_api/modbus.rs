use super::HostContext;
use crate::modbus::ModbusClient;
use mlua::{Lua, Result as LuaResult, Table, Value};
use std::sync::{Arc, Mutex};

/// Register Modbus host functions.
///
/// Unlike zap-core, there are no explicit handles. The connection is auto-created
/// from the driver's config and shared via HostContext.
///
/// - host.modbus_read(register, count, type?) -> table | nil
/// - host.modbus_write(register, value) -> bool
/// - host.modbus_write_multiple(register, values_table) -> bool
pub fn register(lua: &Lua, host: &Table, ctx: &HostContext) -> LuaResult<()> {
    if let Some(ref client) = ctx.modbus_client {
        register_read(lua, host, client.clone())?;
        register_write(lua, host, client.clone())?;
        register_write_multiple(lua, host, client.clone())?;
    } else {
        // No Modbus connection -- register stubs that return nil/false
        let read_stub = lua.create_function(|_, _args: mlua::MultiValue| {
            tracing::debug!("modbus_read() called but no Modbus connection configured");
            Ok(Value::Nil)
        })?;
        host.set("modbus_read", read_stub)?;

        let write_stub = lua.create_function(|_, _args: mlua::MultiValue| {
            tracing::debug!("modbus_write() called but no Modbus connection configured");
            Ok(false)
        })?;
        host.set("modbus_write", write_stub)?;

        let write_multi_stub = lua.create_function(|_, _args: mlua::MultiValue| {
            tracing::debug!("modbus_write_multiple() called but no Modbus connection configured");
            Ok(false)
        })?;
        host.set("modbus_write_multiple", write_multi_stub)?;
    }

    Ok(())
}

fn register_read(
    lua: &Lua,
    host: &Table,
    client: Arc<Mutex<ModbusClient>>,
) -> LuaResult<()> {
    // host.modbus_read(register, count, type?) -> table | nil
    let f = lua.create_function(
        move |lua, (register, count, reg_type): (u16, u16, Option<String>)| {
            let rt = reg_type.as_deref().unwrap_or("holding");
            let result = {
                let mut c = client.lock().unwrap();
                match rt {
                    "input" => c.read_input_registers(register, count),
                    _ => c.read_holding_registers(register, count),
                }
            };
            match result {
                Ok(values) => {
                    let t = lua.create_table()?;
                    for (i, v) in values.iter().enumerate() {
                        t.set(i + 1, *v)?;
                    }
                    Ok(Value::Table(t))
                }
                Err(e) => {
                    tracing::warn!("modbus_read failed: {}", e);
                    Ok(Value::Nil)
                }
            }
        },
    )?;
    host.set("modbus_read", f)?;
    Ok(())
}

fn register_write(
    lua: &Lua,
    host: &Table,
    client: Arc<Mutex<ModbusClient>>,
) -> LuaResult<()> {
    // host.modbus_write(register, value) -> bool
    let f = lua.create_function(move |_, (register, value): (u16, u16)| {
        let result = client.lock().unwrap().write_register(register, value);
        match result {
            Ok(()) => Ok(true),
            Err(e) => {
                tracing::warn!("modbus_write failed: {}", e);
                Ok(false)
            }
        }
    })?;
    host.set("modbus_write", f)?;
    Ok(())
}

fn register_write_multiple(
    lua: &Lua,
    host: &Table,
    client: Arc<Mutex<ModbusClient>>,
) -> LuaResult<()> {
    // host.modbus_write_multiple(register, values_table) -> bool
    let f = lua.create_function(move |_, (register, values): (u16, Table)| {
        // Convert Lua table (1-indexed) to Vec<u16>
        let mut vals = Vec::new();
        let len: i64 = values.raw_len() as i64;
        for i in 1..=len {
            let v: u16 = values.get(i)?;
            vals.push(v);
        }

        let result = client.lock().unwrap().write_multiple_registers(register, &vals);
        match result {
            Ok(()) => Ok(true),
            Err(e) => {
                tracing::warn!("modbus_write_multiple failed: {}", e);
                Ok(false)
            }
        }
    })?;
    host.set("modbus_write_multiple", f)?;
    Ok(())
}
