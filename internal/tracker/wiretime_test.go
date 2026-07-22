package tracker

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWireTimeEncodesISO8601WithThreeFractionDigits(t *testing.T) {
	at := WireTimeOf(time.Date(2026, 7, 22, 9, 5, 3, 120_000_000, time.UTC))
	data, err := json.Marshal(at)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"2026-07-22T09:05:03.120Z"` {
		t.Fatalf("wire format = %s", data)
	}
}

func TestWireTimeQuantizesToMilliseconds(t *testing.T) {
	at := WireTimeOf(time.Date(2026, 1, 2, 3, 4, 5, 999_600_000, time.UTC))
	data, _ := json.Marshal(at)
	if string(data) != `"2026-01-02T03:04:06.000Z"` {
		t.Fatalf("quantized wire format = %s", data)
	}
}

func TestWireTimeParsesPlainAndFractional(t *testing.T) {
	for _, in := range []string{`"2026-07-22T09:05:03Z"`, `"2026-07-22T09:05:03.120Z"`, `"2026-07-22T11:05:03.120+02:00"`} {
		var w WireTime
		if err := json.Unmarshal([]byte(in), &w); err != nil {
			t.Fatalf("parse %s: %v", in, err)
		}
		out, _ := json.Marshal(w)
		var reparsed WireTime
		if err := json.Unmarshal(out, &reparsed); err != nil {
			t.Fatalf("round trip %s → %s: %v", in, out, err)
		}
		if !reparsed.Time.Equal(w.Time) {
			t.Fatalf("round trip drift: %s → %s", in, out)
		}
	}
}

func TestWireTimeRejectsGarbage(t *testing.T) {
	var w WireTime
	if err := json.Unmarshal([]byte(`"yesterday"`), &w); err == nil {
		t.Fatal("garbage date must fail to parse")
	}
	if err := json.Unmarshal([]byte(`12345`), &w); err == nil {
		t.Fatal("non-string date must fail to parse")
	}
}
