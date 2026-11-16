package webtools

import (
	"encoding/json"
	"os"
)

func getenv(name string) string { return os.Getenv(name) }

func jsonMarshal(v any) (string, error) {
	blob, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(blob), nil
}

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func asString(v any) string {
	s, _ := v.(string)
	return s
}
