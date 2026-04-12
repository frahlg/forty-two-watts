//! Minimal sync MQTT 3.1.1 client over TcpStream.
//!
//! Supports CONNECT (with optional username/password), SUBSCRIBE (including wildcards),
//! PUBLISH (QoS 0), PINGREQ keepalive, and non-blocking message buffering.

use std::collections::VecDeque;
use std::io::{self, Read, Write};
use std::net::TcpStream;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

const CONNECT: u8 = 0x10;
const CONNACK: u8 = 0x20;
const PUBLISH: u8 = 0x30;
const SUBSCRIBE: u8 = 0x82; // with QoS 1 flag
const SUBACK: u8 = 0x90;
const PINGREQ: u8 = 0xC0;
const PINGRESP: u8 = 0xD0;

const DEFAULT_KEEPALIVE_S: u16 = 60;
const READ_TIMEOUT: Duration = Duration::from_millis(100);

/// A received MQTT message.
#[derive(Debug, Clone)]
pub struct MqttMessage {
    pub topic: String,
    pub payload: String,
}

/// Thread-safe message queue shared between the reader loop and the Lua host API.
#[derive(Debug, Clone)]
pub struct MessageQueue {
    inner: Arc<Mutex<VecDeque<MqttMessage>>>,
}

impl MessageQueue {
    pub fn new() -> Self {
        Self {
            inner: Arc::new(Mutex::new(VecDeque::new())),
        }
    }

    /// Push a message into the queue.
    pub fn push(&self, msg: MqttMessage) {
        let mut q = self.inner.lock().unwrap();
        // Cap the queue at 1000 messages to prevent unbounded growth
        if q.len() >= 1000 {
            q.pop_front();
        }
        q.push_back(msg);
    }

    /// Drain all messages from the queue.
    pub fn drain(&self) -> Vec<MqttMessage> {
        let mut q = self.inner.lock().unwrap();
        q.drain(..).collect()
    }
}

/// Minimal synchronous MQTT 3.1.1 client.
pub struct MqttClient {
    stream: TcpStream,
    packet_id: u16,
    keepalive_s: u16,
    last_activity: Instant,
    queue: MessageQueue,
}

impl MqttClient {
    /// Connect to an MQTT broker.
    pub fn connect(
        host: &str,
        port: u16,
        client_id: &str,
        username: Option<&str>,
        password: Option<&str>,
    ) -> io::Result<Self> {
        let addr = format!("{}:{}", host, port);
        let stream = TcpStream::connect_timeout(
            &addr.parse().map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, format!("{}", e)))?,
            Duration::from_secs(5),
        )?;
        stream.set_nodelay(true).ok();
        stream.set_read_timeout(Some(READ_TIMEOUT)).ok();
        stream.set_write_timeout(Some(Duration::from_secs(5))).ok();

        let mut client = Self {
            stream,
            packet_id: 0,
            keepalive_s: DEFAULT_KEEPALIVE_S,
            last_activity: Instant::now(),
            queue: MessageQueue::new(),
        };

        client.send_connect(client_id, username, password)?;
        client.read_connack()?;

        Ok(client)
    }

    /// Get a clone of the message queue (thread-safe handle).
    pub fn message_queue(&self) -> MessageQueue {
        self.queue.clone()
    }

    /// Subscribe to a topic (supports MQTT wildcards: #, +).
    pub fn subscribe(&mut self, topic: &str) -> io::Result<()> {
        self.packet_id = self.packet_id.wrapping_add(1);
        let pid = self.packet_id;

        // Variable header: packet id (2 bytes)
        // Payload: topic length (2) + topic + QoS (1)
        let topic_bytes = topic.as_bytes();
        let remaining = 2 + 2 + topic_bytes.len() + 1;

        let mut packet = Vec::with_capacity(2 + remaining);
        packet.push(SUBSCRIBE);
        encode_remaining_length(&mut packet, remaining);
        packet.extend_from_slice(&pid.to_be_bytes());
        packet.extend_from_slice(&(topic_bytes.len() as u16).to_be_bytes());
        packet.extend_from_slice(topic_bytes);
        packet.push(0); // QoS 0

        self.stream.write_all(&packet)?;
        self.last_activity = Instant::now();

        // Read SUBACK
        self.read_suback()?;
        Ok(())
    }

    /// Publish a message (QoS 0, no packet ID).
    pub fn publish(&mut self, topic: &str, payload: &[u8]) -> io::Result<()> {
        self.publish_retained(topic, false, payload)
    }

    /// Publish with optional retained flag.
    pub fn publish_retained(&mut self, topic: &str, retained: bool, payload: &[u8]) -> io::Result<()> {
        let topic_bytes = topic.as_bytes();
        let remaining = 2 + topic_bytes.len() + payload.len();

        let mut packet = Vec::with_capacity(2 + remaining);
        let flags = PUBLISH | if retained { 0x01 } else { 0x00 };
        packet.push(flags);
        encode_remaining_length(&mut packet, remaining);
        packet.extend_from_slice(&(topic_bytes.len() as u16).to_be_bytes());
        packet.extend_from_slice(topic_bytes);
        packet.extend_from_slice(payload);

        self.stream.write_all(&packet)?;
        self.last_activity = Instant::now();
        Ok(())
    }

    /// Non-blocking read: process any available packets and buffer PUBLISH messages.
    /// Call this periodically (e.g., before each driver poll).
    pub fn pump(&mut self) -> io::Result<()> {
        loop {
            match self.read_packet() {
                Ok(Some((ptype, data))) => {
                    self.handle_packet(ptype, &data);
                }
                Ok(None) => break, // No more data available
                Err(e) if e.kind() == io::ErrorKind::WouldBlock
                    || e.kind() == io::ErrorKind::TimedOut => break,
                Err(e) => return Err(e),
            }
        }

        // Send PINGREQ if needed
        if self.last_activity.elapsed() > Duration::from_secs(self.keepalive_s as u64 / 2) {
            self.send_pingreq()?;
        }

        Ok(())
    }

    /// Drain all buffered messages.
    pub fn drain_messages(&self) -> Vec<MqttMessage> {
        self.queue.drain()
    }

    // -- Private protocol methods --

    fn send_connect(
        &mut self,
        client_id: &str,
        username: Option<&str>,
        password: Option<&str>,
    ) -> io::Result<()> {
        let client_id_bytes = client_id.as_bytes();

        // Connect flags
        let mut flags: u8 = 0x02; // Clean session
        if username.is_some() {
            flags |= 0x80; // Username flag
        }
        if password.is_some() {
            flags |= 0x40; // Password flag
        }

        // Variable header: protocol name + level + flags + keepalive
        let mut var_header = Vec::new();
        // Protocol Name "MQTT"
        var_header.extend_from_slice(&[0x00, 0x04]);
        var_header.extend_from_slice(b"MQTT");
        // Protocol Level (4 = 3.1.1)
        var_header.push(0x04);
        // Connect Flags
        var_header.push(flags);
        // Keep Alive
        var_header.extend_from_slice(&self.keepalive_s.to_be_bytes());

        // Payload
        let mut payload = Vec::new();
        // Client ID
        payload.extend_from_slice(&(client_id_bytes.len() as u16).to_be_bytes());
        payload.extend_from_slice(client_id_bytes);
        // Username
        if let Some(u) = username {
            let ub = u.as_bytes();
            payload.extend_from_slice(&(ub.len() as u16).to_be_bytes());
            payload.extend_from_slice(ub);
        }
        // Password
        if let Some(p) = password {
            let pb = p.as_bytes();
            payload.extend_from_slice(&(pb.len() as u16).to_be_bytes());
            payload.extend_from_slice(pb);
        }

        let remaining = var_header.len() + payload.len();
        let mut packet = Vec::with_capacity(2 + remaining);
        packet.push(CONNECT);
        encode_remaining_length(&mut packet, remaining);
        packet.extend_from_slice(&var_header);
        packet.extend_from_slice(&payload);

        self.stream.write_all(&packet)?;
        self.last_activity = Instant::now();
        Ok(())
    }

    fn read_connack(&mut self) -> io::Result<()> {
        // Temporarily set a longer read timeout for CONNACK
        self.stream.set_read_timeout(Some(Duration::from_secs(5))).ok();

        let mut header = [0u8; 4]; // CONNACK is always 4 bytes
        self.stream.read_exact(&mut header)?;

        self.stream.set_read_timeout(Some(READ_TIMEOUT)).ok();

        if header[0] != CONNACK {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("expected CONNACK (0x20), got 0x{:02x}", header[0]),
            ));
        }
        // header[1] = remaining length (should be 2)
        // header[2] = session present flag
        // header[3] = return code (0 = accepted)
        if header[3] != 0 {
            return Err(io::Error::new(
                io::ErrorKind::ConnectionRefused,
                format!("CONNACK return code: {}", header[3]),
            ));
        }
        Ok(())
    }

    fn read_suback(&mut self) -> io::Result<()> {
        self.stream.set_read_timeout(Some(Duration::from_secs(5))).ok();

        let (ptype, _data) = match self.read_packet()? {
            Some(p) => p,
            None => {
                self.stream.set_read_timeout(Some(READ_TIMEOUT)).ok();
                return Err(io::Error::new(io::ErrorKind::TimedOut, "no SUBACK received"));
            }
        };

        self.stream.set_read_timeout(Some(READ_TIMEOUT)).ok();

        if ptype & 0xF0 != SUBACK {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("expected SUBACK (0x90), got 0x{:02x}", ptype),
            ));
        }
        Ok(())
    }

    fn send_pingreq(&mut self) -> io::Result<()> {
        self.stream.write_all(&[PINGREQ, 0x00])?;
        self.last_activity = Instant::now();
        Ok(())
    }

    /// Read a single MQTT packet. Returns None if no data available (non-blocking).
    fn read_packet(&mut self) -> io::Result<Option<(u8, Vec<u8>)>> {
        let mut first_byte = [0u8; 1];
        match self.stream.read_exact(&mut first_byte) {
            Ok(()) => {}
            Err(e) if e.kind() == io::ErrorKind::WouldBlock
                || e.kind() == io::ErrorKind::TimedOut => return Ok(None),
            Err(e) => return Err(e),
        }

        let ptype = first_byte[0];
        let remaining = decode_remaining_length(&mut self.stream)?;

        let mut data = vec![0u8; remaining];
        if remaining > 0 {
            self.stream.read_exact(&mut data)?;
        }

        Ok(Some((ptype, data)))
    }

    fn handle_packet(&mut self, ptype: u8, data: &[u8]) {
        match ptype & 0xF0 {
            PUBLISH => {
                // Parse PUBLISH: topic length (2) + topic + payload
                if data.len() < 2 {
                    return;
                }
                let topic_len = u16::from_be_bytes([data[0], data[1]]) as usize;
                if data.len() < 2 + topic_len {
                    return;
                }
                let topic = String::from_utf8_lossy(&data[2..2 + topic_len]).to_string();

                // For QoS 0 there's no packet ID, payload starts right after topic
                let payload_start = 2 + topic_len;
                let payload = if payload_start < data.len() {
                    String::from_utf8_lossy(&data[payload_start..]).to_string()
                } else {
                    String::new()
                };

                self.queue.push(MqttMessage { topic, payload });
            }
            PINGRESP => {
                // Keepalive acknowledged, nothing to do
            }
            _ => {
                // Ignore other packet types
            }
        }
    }
}

/// Encode MQTT remaining length into the packet buffer.
fn encode_remaining_length(buf: &mut Vec<u8>, mut length: usize) {
    loop {
        let mut byte = (length % 128) as u8;
        length /= 128;
        if length > 0 {
            byte |= 0x80;
        }
        buf.push(byte);
        if length == 0 {
            break;
        }
    }
}

/// Decode MQTT remaining length from a stream.
fn decode_remaining_length<R: Read>(reader: &mut R) -> io::Result<usize> {
    let mut value: usize = 0;
    let mut multiplier: usize = 1;
    loop {
        let mut byte = [0u8; 1];
        reader.read_exact(&mut byte)?;
        value += (byte[0] & 0x7F) as usize * multiplier;
        if byte[0] & 0x80 == 0 {
            break;
        }
        multiplier *= 128;
        if multiplier > 128 * 128 * 128 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "malformed remaining length",
            ));
        }
    }
    Ok(value)
}
