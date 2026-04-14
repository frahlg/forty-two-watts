package drivers

import "encoding/json"

func marshalMessages(msgs []MQTTMessage) ([]byte, error) {
	return json.Marshal(msgs)
}

// jsonUnmarshal is a test-accessible alias so we don't pull encoding/json
// into every test file.
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
