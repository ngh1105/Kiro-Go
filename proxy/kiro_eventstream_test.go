package proxy

import (
	"bytes"
	"errors"
	"testing"
)

// TestParseEventStreamRejectsOversizedTotalLength verifies parseEventStream
// rejects a frame whose totalLength exceeds the sanity cap BEFORE the
// make([]byte, remaining) allocation. A corrupt/malicious (or bit-flipped)
// 32-bit totalLength near 2^32 would otherwise drive a multi-GB allocation:
// either an alloc panic (net/http recovers per-request → connection dropped
// with no terminal event → client hang) or a multi-GB buffer held to the 5-min
// client timeout (memory-pressure DoS). The cap keeps the test itself from
// allocating gigabytes: it feeds totalLength = cap+1.
func TestParseEventStreamRejectsOversizedTotalLength(t *testing.T) {
	over := maxEventStreamMessageBytes + 1
	prelude := make([]byte, 12) // prelude: total_len(4) + headers_len(4) + crc(4)
	prelude[0] = byte(over >> 24)
	prelude[1] = byte(over >> 16)
	prelude[2] = byte(over >> 8)
	prelude[3] = byte(over)
	// headers_len bytes (4-7) and crc (8-11) left zero.

	err := parseEventStream(bytes.NewReader(prelude), &KiroStreamCallback{})
	if !errors.Is(err, errEventStreamFrameTooLarge) {
		t.Fatalf("oversized totalLength must be rejected before the multi-GB make; got %v", err)
	}
}
