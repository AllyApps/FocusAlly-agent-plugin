package tracker

import (
	"fmt"
	"time"
)

// wireLayout matches the backend's SyncJSON encoding: ISO8601 UTC with
// exactly 3 fractional digits (millisecond precision).
const wireLayout = "2006-01-02T15:04:05.000Z"

// WireTime is a time.Time that marshals as ISO8601 UTC with exactly
// 3 fractional digits, quantized to whole milliseconds — the wire
// format the FocusAlly backend's SyncJSON decoder expects.
type WireTime struct {
	time.Time
}

func Now() WireTime { return WireTimeOf(time.Now()) }

func WireTimeOf(t time.Time) WireTime {
	return WireTime{t.UTC().Round(time.Millisecond)}
}

func (w WireTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + w.UTC().Round(time.Millisecond).Format(wireLayout) + `"`), nil
}

func (w *WireTime) UnmarshalJSON(data []byte) error {
	s := string(data)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return fmt.Errorf("wiretime: not a JSON string: %s", s)
	}
	s = s[1 : len(s)-1]
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			*w = WireTimeOf(t)
			return nil
		}
	}
	return fmt.Errorf("wiretime: invalid ISO8601 date: %s", s)
}
