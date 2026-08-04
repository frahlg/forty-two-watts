package appwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

func u32(v uint32) *uint32 { return &v }

func body(t *testing.T, v any) cbor.RawMessage {
	t.Helper()
	b, err := MarshalBody(v)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	return b
}

func control(env Envelope, flags uint8) Frame {
	return Frame{Lane: LaneControl, Flags: flags, Envelope: env}
}

func TestFrameRoundTrip(t *testing.T) {
	in := control(Envelope{
		Type: "cmd.ack",
		ID:   u32(42),
		Body: body(t, map[string]any{"leaseId": "x", "expiresAtMs": 1000}),
	}, 0)

	wire, err := Encode(in, 512)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if out.Envelope.Type != "cmd.ack" {
		t.Errorf("type = %q, want cmd.ack", out.Envelope.Type)
	}
	if out.Envelope.ID == nil || *out.Envelope.ID != 42 {
		t.Errorf("id = %v, want 42", out.Envelope.ID)
	}
	if !bytes.Equal(out.Envelope.Body, in.Envelope.Body) {
		t.Errorf("body = %x, want %x", out.Envelope.Body, in.Envelope.Body)
	}
	if out.Lane != LaneControl {
		t.Errorf("lane = %d, want %d", out.Lane, LaneControl)
	}
}

func TestFrameHeader(t *testing.T) {
	wire, err := Encode(control(Envelope{Type: "tick"}, 0), 512)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	if wire[0] != FrameVersion {
		t.Errorf("version = %d, want %d", wire[0], FrameVersion)
	}
	if wire[1] != LaneControl {
		t.Errorf("lane = %d, want %d", wire[1], LaneControl)
	}
	if wire[3] != 0 {
		t.Errorf("reserved = %d, want 0", wire[3])
	}
}

func TestFrameCarriesTruncationFlag(t *testing.T) {
	wire, err := Encode(control(Envelope{Type: "delta"}, FlagTrunc), 512)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	f, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !f.Truncated() {
		t.Error("truncation flag was lost")
	}

	wire, err = Encode(control(Envelope{Type: "delta"}, 0), 512)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if f, err := Decode(wire); err != nil || f.Truncated() {
		t.Errorf("unset flag decoded as truncated (err %v)", err)
	}
}

// The product claim, not a formatting detail. A 1 Hz stream whose frame size
// tracks what changed leaks the household's load pattern to the relay operator
// through otherwise perfect encryption.
func TestPaddingIsConstantRegardlessOfContent(t *testing.T) {
	fields := map[string]int{}
	for i := range 30 {
		fields[string(rune('a'+i))] = i * 137
	}

	payloads := []any{
		nil,
		map[string]any{"seq": 1, "fields": map[string]int{}},
		map[string]any{"seq": 2, "fields": map[string]int{"2": 11400, "3": 6200, "4": -4200, "5": 875, "6": 13400}},
		map[string]any{"seq": 3, "fields": fields},
	}

	for _, p := range payloads {
		env := Envelope{Type: "delta"}
		if p != nil {
			env.Body = body(t, p)
		}
		wire, err := Encode(control(env, 0), 512)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if len(wire) != 512 {
			t.Errorf("frame is %d bytes, want 512 whatever the payload", len(wire))
		}
	}
}

func TestPaddingIsZeroed(t *testing.T) {
	wire, err := Encode(control(Envelope{Type: "tick"}, 0), 512)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	declared := int(binary.BigEndian.Uint16(wire[4:6]))
	padding := wire[HeaderBytes+declared:]
	if len(padding) == 0 {
		t.Fatal("expected padding after a short payload")
	}
	for i, b := range padding {
		if b != 0 {
			t.Fatalf("padding byte %d is %#x; residue from an earlier frame would leak", i, b)
		}
	}
}

// A lane 0 frame at any other size makes frame length a function of what
// happened in the house, which is exactly what the padding exists to stop. The
// box emits that stream, so the refusal belongs in its encoder rather than in
// whatever calls it.
func TestLaneZeroTakesOnlyAControlBucket(t *testing.T) {
	if _, err := Encode(control(Envelope{Type: "tick"}, 0), 1024); !errors.Is(err, ErrFrameBucket) {
		t.Fatalf("err = %v, want ErrFrameBucket", err)
	}
	for _, bucket := range ControlBuckets {
		if _, err := Encode(control(Envelope{Type: "tick"}, 0), bucket); err != nil {
			t.Fatalf("bucket %d: %v", bucket, err)
		}
	}
	// Bulk steps freely; it already leaks that a transfer is happening.
	if _, err := Encode(Frame{Lane: LaneBulk, Envelope: Envelope{Type: "snap"}}, 4096); err != nil {
		t.Fatalf("bulk: %v", err)
	}
}

func TestEncodeRefusesToGrowTheBucket(t *testing.T) {
	fields := map[string]int{}
	for i := range 400 {
		fields[string(rune('A'+i%26))+string(rune('a'+i/26))] = i
	}

	_, err := Encode(control(Envelope{Type: "snap", Body: body(t, fields)}, 0), 512)
	if !errors.Is(err, ErrFrameExceedsBucket) {
		t.Fatalf("err = %v, want ErrFrameExceedsBucket; growing the bucket defeats the padding", err)
	}
}

// A service worker can pin a bundle for a long time. A hard version wall turns
// that into a white screen, so unknown things are ignored, not fatal.
func TestUnknownKeysAndTypesSurvive(t *testing.T) {
	// Hand-built, because Encode cannot produce a key it does not know:
	// {"t": "delta", "b": {"seq": 1}, "futureField": "from a newer box"}.
	payload, err := cbor.Marshal(map[string]any{
		"t":           "delta",
		"b":           map[string]int{"seq": 1},
		"futureField": "from a newer box",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	wire := make([]byte, 512)
	wire[0] = FrameVersion
	binary.BigEndian.PutUint16(wire[4:6], uint16(len(payload)))
	copy(wire[HeaderBytes:], payload)

	f, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if f.Envelope.Type != "delta" {
		t.Errorf("type = %q, want delta", f.Envelope.Type)
	}

	var decoded map[string]int
	if err := cbor.Unmarshal(f.Envelope.Body, &decoded); err != nil {
		t.Fatalf("body: %v", err)
	}
	if decoded["seq"] != 1 {
		t.Errorf("body = %v, want seq 1", decoded)
	}
}

func TestDecodesATypeItHasNeverHeardOf(t *testing.T) {
	wire, err := Encode(control(Envelope{Type: "some.future.type", Body: body(t, map[string]int{"x": 1})}, 0), 512)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	f, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if f.Envelope.Type != "some.future.type" {
		t.Errorf("type = %q", f.Envelope.Type)
	}
}

func TestMalformedFrames(t *testing.T) {
	valid := func(t *testing.T) []byte {
		t.Helper()
		wire, err := Encode(control(Envelope{Type: "tick"}, 0), 512)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		return wire
	}

	withPayload := func(payload []byte) []byte {
		wire := make([]byte, 512)
		wire[0] = FrameVersion
		binary.BigEndian.PutUint16(wire[4:6], uint16(len(payload)))
		copy(wire[HeaderBytes:], payload)
		return wire
	}

	tests := []struct {
		name  string
		frame func(t *testing.T) []byte
		want  error
	}{
		{"shorter than its header", func(*testing.T) []byte { return make([]byte, 3) }, ErrFrameShort},
		{"unparseable version", func(t *testing.T) []byte {
			wire := valid(t)
			wire[0] = 99
			return wire
		}, ErrFrameVersion},
		{"declared length overruns the frame", func(t *testing.T) []byte {
			wire := valid(t)
			binary.BigEndian.PutUint16(wire[4:6], 600)
			return wire
		}, ErrFrameTruncated},
		{"payload is not CBOR", func(*testing.T) []byte {
			return withPayload([]byte{0xff, 0xff, 0xff, 0xff})
		}, ErrFrameCBOR},
		{"envelope is an array", func(*testing.T) []byte {
			return withPayload([]byte{0x82, 0x01, 0x02}) // [1, 2]
		}, ErrFrameCBOR},
		{"type is not a string", func(*testing.T) []byte {
			return withPayload([]byte{0xa1, 0x61, 0x74, 0x01}) // {"t": 1}
		}, ErrFrameEnvelope},
		{"no type at all", func(*testing.T) []byte {
			return withPayload([]byte{0xa1, 0x61, 0x62, 0x01}) // {"b": 1}
		}, ErrFrameEnvelope},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.frame(t)); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBulkBucketFor(t *testing.T) {
	tests := []struct {
		payload int
		bucket  int
		ok      bool
	}{
		{100, 1024, true},
		{1018, 1024, true},
		{1019, 4096, true}, // 1019 + 6 > 1024
		{5000, 16384, true},
		{20000, 0, false},
	}

	for _, tc := range tests {
		bucket, ok := BulkBucketFor(tc.payload)
		if bucket != tc.bucket || ok != tc.ok {
			t.Errorf("BulkBucketFor(%d) = %d, %v; want %d, %v", tc.payload, bucket, ok, tc.bucket, tc.ok)
		}
	}
}

func TestEnvelopeKeyOrderMatchesTheReference(t *testing.T) {
	// t, id, b — the order the app emits, so a frame built here is byte-equal
	// to one built there from the same values. See interop_test.go for the
	// same claim checked against the app's own bytes.
	wire, err := Encode(control(Envelope{Type: "a", ID: u32(1), Body: cbor.RawMessage{0x02}}, 0), 256)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := []byte{0xa3, 0x61, 0x74, 0x61, 0x61, 0x62, 0x69, 0x64, 0x01, 0x61, 0x62, 0x02}
	if got := wire[HeaderBytes : HeaderBytes+len(want)]; !bytes.Equal(got, want) {
		t.Fatalf("payload = %x, want %x", got, want)
	}
}
