// internal/cli/output/format.go
package output

import (
	"encoding/json"
)

// FormatJSON converts data to pretty-printed JSON with 2-space indentation.
func FormatJSON(data interface{}) (string, error) {
	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}
