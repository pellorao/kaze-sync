package resources

import (
	"testing"
	"time"
	"unsafe"
)

// setWallBack moves only the wall reading of t back, keeping its monotonic
// reading, as a system clock step does to time.Now() values.
func setWallBack(t time.Time, d time.Duration) time.Time {
	type tm struct {
		wall uint64
		ext  int64
		loc  *time.Location
	}
	p := (*tm)(unsafe.Pointer(&t))
	if p.wall&(1<<63) == 0 {
		panic("no monotonic reading")
	}
	p.wall -= uint64(d/time.Second) << 30
	return t
}

func TestWallClockStepBackWithMonotonicReadings(t *testing.T) {
	a := time.Now()
	b := setWallBack(a.Add(time.Second), time.Hour) // wall earlier than a, monotonic later
	if !b.After(a) || b.Round(0).After(a.Round(0)) {
		t.Fatal("test setup: expected monotonic order forward and wall order backward")
	}
	c := New()
	c.Observe(a, Reading{RSS: 30_000_000, CPU: 10})
	c.Observe(b, Reading{RSS: 30_000_000, CPU: 11})
	if len(c.days) == 0 {
		t.Fatal("no days")
	}
	for _, d := range c.days {
		if !d.clockChanged {
			t.Fatalf("wall clock step back not recorded for %s", d.Date)
		}
	}
}
