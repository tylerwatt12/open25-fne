package radio

import (
	"fmt"
	"testing"
)

// AABC-E2.3.29/Fig2.3.29-1, PDF39/p20: User Status precedes
// Unit Status. The log adapter retains their full combined 16-bit value.
func TestStatusAndMessageEventsRetainCompleteValues(t *testing.T) {
	for _, kind := range []struct{ native, field, event string }{
		{"Status Update Request", "status", "status"},
		{"Message Update Request", "message", "message"},
	} {
		for _, code := range []int{0, 1, 255, 256, 258, 65535} {
			line := fmt.Sprintf("I: (RF) P25, TSDU (%s), %s = $%04X, srcId = 10001, dstId = 16777212", kind.native, kind.field, code)
			e := ParseEvent(line)
			if e == nil || e.Kind != kind.event || e.Code == nil || *e.Code != code {
				t.Fatalf("complete received value was lost: %s %+v", line, e)
			}
		}
		line := fmt.Sprintf("I: (RF) P25, TSDU (%s), %s = 65536, srcId = 10001", kind.native, kind.field)
		if e := ParseEvent(line); e != nil {
			t.Fatalf("out-of-field value was accepted: %+v", e)
		}
	}
}
