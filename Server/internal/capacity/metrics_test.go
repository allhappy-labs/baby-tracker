package capacity

import (
	"strings"
	"testing"
)

func TestCapacityLogAggregationBoundedAndExplicit(t *testing.T) {
	r := ParseCapacitySamples([]byte(`{"msg":"other","secret":"never export"}
{"msg":"capacity resource sample","sample":{"at":"2026-10-03T00:00:00Z","alloc_total_bytes":{"available":true,"value":0},"cpu_total_ns":{"available":false},"pools":{},"capacity":{"phases":{}}}}
`))
	if len(r) != 1 || !r[0].AllocTotal.Available || r[0].CPUTotal.Available {
		t.Fatalf("samples=%+v", r)
	}
	if len(ParseCapacitySamples([]byte(strings.Repeat(`{"msg":"capacity resource sample","sample":{"at":"2026-10-03T00:00:00Z"}}`+"\n", 2000)))) != 1442 {
		t.Fatal("sample bound missing")
	}
}
