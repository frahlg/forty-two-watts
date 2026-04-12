use super::HostContext;
use crate::mqtt::client::{MessageQueue, MqttClient};
use mlua::{Lua, Result as LuaResult, Table, Value};
use std::sync::{Arc, Mutex};

/// Register MQTT host functions:
/// - host.mqtt_subscribe(topic) -> bool
/// - host.mqtt_messages() -> table of {topic, payload}
/// - host.mqtt_publish(topic, payload) -> bool
pub fn register(lua: &Lua, host: &Table, ctx: &HostContext) -> LuaResult<()> {
    if let (Some(client), Some(queue)) = (&ctx.mqtt_client, &ctx.mqtt_queue) {
        register_subscribe(lua, host, client.clone())?;
        register_messages(lua, host, client.clone(), queue.clone())?;
        register_publish(lua, host, client.clone())?;
    } else {
        // No MQTT connection -- register stubs
        let sub_stub = lua.create_function(|_, _topic: String| {
            tracing::debug!("mqtt_subscribe() called but no MQTT connection configured");
            Ok(false)
        })?;
        host.set("mqtt_subscribe", sub_stub)?;

        let msg_stub = lua.create_function(|lua, ()| {
            let t = lua.create_table()?;
            Ok(t)
        })?;
        host.set("mqtt_messages", msg_stub)?;

        let pub_stub = lua.create_function(|_, (_topic, _payload): (String, String)| {
            tracing::debug!("mqtt_publish() called but no MQTT connection configured");
            Ok(false)
        })?;
        host.set("mqtt_publish", pub_stub)?;
    }

    Ok(())
}

fn register_subscribe(
    lua: &Lua,
    host: &Table,
    client: Arc<Mutex<MqttClient>>,
) -> LuaResult<()> {
    let f = lua.create_function(move |_, topic: String| {
        match client.lock().unwrap().subscribe(&topic) {
            Ok(()) => {
                tracing::info!("MQTT subscribed to '{}'", topic);
                Ok(true)
            }
            Err(e) => {
                tracing::warn!("MQTT subscribe '{}' failed: {}", topic, e);
                Ok(false)
            }
        }
    })?;
    host.set("mqtt_subscribe", f)?;
    Ok(())
}

fn register_messages(
    lua: &Lua,
    host: &Table,
    client: Arc<Mutex<MqttClient>>,
    queue: MessageQueue,
) -> LuaResult<()> {
    let f = lua.create_function(move |lua, ()| {
        // Pump the client to process any pending packets
        if let Ok(mut c) = client.lock() {
            let _ = c.pump();
        }

        let messages = queue.drain();
        let result = lua.create_table()?;
        for (i, msg) in messages.iter().enumerate() {
            let entry = lua.create_table()?;
            entry.set("topic", msg.topic.as_str())?;
            entry.set("payload", msg.payload.as_str())?;
            result.set(i + 1, entry)?;
        }
        Ok(Value::Table(result))
    })?;
    host.set("mqtt_messages", f)?;
    Ok(())
}

fn register_publish(
    lua: &Lua,
    host: &Table,
    client: Arc<Mutex<MqttClient>>,
) -> LuaResult<()> {
    let f = lua.create_function(move |_, (topic, payload): (String, String)| {
        match client.lock().unwrap().publish(&topic, payload.as_bytes()) {
            Ok(()) => Ok(true),
            Err(e) => {
                tracing::warn!("MQTT publish '{}' failed: {}", topic, e);
                Ok(false)
            }
        }
    })?;
    host.set("mqtt_publish", f)?;
    Ok(())
}
