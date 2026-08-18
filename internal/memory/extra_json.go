package memory

import "encoding/json"

// unmarshalKeepingExtra decodes data into dest, then stores every JSON
// object key that is not in known as extra. Used so a sidecar field added
// later is not silently dropped on the Go round-trip.
func unmarshalKeepingExtra[T any](data []byte, dest *T, known []string) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, dest); err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for _, key := range known {
		delete(raw, key)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return raw, nil
}

func marshalWithExtra(known any, extra map[string]json.RawMessage) ([]byte, error) {
	body, err := json.Marshal(known)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return body, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	for key, value := range extra {
		if _, exists := obj[key]; !exists {
			obj[key] = value
		}
	}
	return json.Marshal(obj)
}
