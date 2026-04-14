package drivers

import "encoding/json"

func marshalMessages(msgs []MQTTMessage) ([]byte, error) {
	return json.Marshal(msgs)
}
