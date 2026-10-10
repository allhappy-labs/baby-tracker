package capacity

import (
	"babytracker/server/internal/diagnostics"
	"bufio"
	"bytes"
	"encoding/json"
)

// Only typed, fixed-domain samples are exported; unrelated server log rows are
// never copied to result artifacts. The 7200-second supervisor bounds retention.
func ParseCapacitySamples(raw []byte) []diagnostics.ResourceSample {
	var result []diagnostics.ResourceSample
	scan := bufio.NewScanner(bytes.NewReader(raw))
	scan.Buffer(make([]byte, 4096), 65536)
	for scan.Scan() {
		var row struct {
			Message string                     `json:"msg"`
			Sample  diagnostics.ResourceSample `json:"sample"`
		}
		if json.Unmarshal(scan.Bytes(), &row) != nil || row.Message != "capacity resource sample" || row.Sample.At.IsZero() {
			continue
		}
		result = append(result, row.Sample)
		if len(result) == 1442 {
			break
		}
	}
	return result
}
