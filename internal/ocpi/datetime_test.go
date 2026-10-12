package ocpi

import (
	"testing"
	"time"
)

func TestFormatTimeFitsTheOCPIDateTime(t *testing.T) {
	got := FormatTime(time.Date(2026, 10, 5, 4, 48, 33, 887156999, time.FixedZone("ICT", 7*3600)))
	if got != "2026-10-04T21:48:33.887Z" || len(got) > 25 {
		t.Fatalf("FormatTime = %q (len %d), want UTC string(25) with millisecond precision", got, len(got))
	}
}
