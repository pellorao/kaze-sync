package resources

import (
	"context"
	"testing"
	"time"
)

func TestHistogramMedian(t *testing.T) {
	var h histogram
	if h.median() != nil {
		t.Fatal("empty median")
	}
	for _, n := range []uint64{10_100_000, 20_100_000, 30_100_000, 40_100_000} {
		h.add(n)
	}
	if got := *h.median(); got != 25.5 {
		t.Fatalf("even median %v", got)
	}
	h.add(20_900_000)
	if got := *h.median(); got != 20.5 {
		t.Fatalf("odd median %v", got)
	}
	h.add(^uint64(0)) // overflow bucket must be safe
}

func TestDaysCoverageAndRestart(t *testing.T) {
	start := time.Date(2026, 11, 16, 23, 59, 0, 0, time.UTC)
	c := New()
	c.Observe(start, Reading{RSS: 30_000_000, CPU: 100})
	c.Observe(start.Add(time.Minute), Reading{RSS: 40_000_000, CPU: 102})
	c.Observe(start.Add(2*time.Minute), Reading{RSS: 40_000_000, CPU: 1})
	ds := c.Days(start.Add(2 * time.Minute))
	if len(ds) != 2 || ds[0].CPUSeconds != 2 || ds[0].CoverageSeconds != 60 || ds[1].CPUSeconds != 0 || ds[1].CoverageSeconds != 0 {
		t.Fatalf("days: %+v", ds)
	}
	if ds[0].Complete || ds[1].Complete {
		t.Fatal("partial day marked complete")
	}
	if len(c.Days(start.AddDate(0, 0, 3))) != 0 {
		t.Fatal("stale days retained")
	}
}

func TestGCIntervalsAcrossMidnight(t *testing.T) {
	start := time.Date(2026, 11, 16, 23, 59, 0, 0, time.UTC)
	c := New()
	c.Observe(start, Reading{RSS: 30_000_000, CPU: 10})
	c.BeginGC(start.Add(10*time.Second), Reading{RSS: 31_000_000, CPU: 11, HWM: 50_000_000}, 64<<20)
	c.Observe(start.Add(time.Minute), Reading{RSS: 60_000_000, CPU: 15, HWM: 60_000_000})
	cycle := c.EndGC(start.Add(70*time.Second), Reading{RSS: 40_000_000, CPU: 16, HWM: 70_000_000}, 123, true)
	c.Observe(start.Add(80*time.Second), Reading{RSS: 35_000_000, CPU: 17})
	ds := c.Days(start.Add(80 * time.Second))
	if cycle.CPUSeconds != 5 || cycle.Seconds != 60 || cycle.CoverageSeconds != 60 || cycle.PeakRSSMB != 70 || !cycle.PeakExact || cycle.BlockRefs == nil || *cycle.BlockRefs != 123 {
		t.Fatalf("cycle %+v", cycle)
	}
	if ds[0].GCSeconds != 50 || ds[1].GCSeconds != 10 || ds[0].GCCPUSeconds != 4 || ds[1].GCCPUSeconds != 1 {
		t.Fatalf("GC split %+v", ds)
	}
	if ds[0].NonGCSeconds != 10 || ds[1].NonGCSeconds != 10 || ds[0].Samples != 1 || ds[1].Samples != 1 {
		t.Fatalf("non-GC split %+v", ds)
	}
}

func TestCompleteDayAndMissingSamples(t *testing.T) {
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, missing := range []bool{false, true} {
		c := New()
		for i := 0; i <= 1440; i++ {
			if missing && i == 600 {
				continue
			}
			c.Observe(start.Add(time.Duration(i)*time.Minute), Reading{RSS: 32_000_000, CPU: float64(i)})
		}
		d := c.Days(start.Add(24 * time.Hour))[0]
		if d.Complete == missing {
			t.Fatalf("missing=%v day=%+v", missing, d)
		}
		if !missing && (d.CoverageSeconds != 86400 || d.CPUSeconds != 1440 || d.Samples != 1440) {
			t.Fatalf("day=%+v", d)
		}
	}
}

func TestReadSelfAndLoopCancellation(t *testing.T) {
	r, err := ReadSelf()
	if err != nil || r.RSS == 0 || r.CPU < 0 {
		t.Fatalf("self %+v %v", r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := New()
	c.Run(ctx)
	c.Failed() // losing a reading must break the CPU/coverage baseline
	c.Observe(time.Now(), Reading{CPU: 100})
	c.Failed()
	c.Observe(time.Now().Add(time.Minute), Reading{CPU: 200})
	for _, d := range c.Days(time.Now().Add(time.Minute)) {
		if d.CPUSeconds != 0 {
			t.Fatal("failed reading bridged")
		}
	}
}

func TestGCBoundedAndUnprovenPeak(t *testing.T) {
	c := New()
	start := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	for i := 0; i < MaxCycles+1; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		c.BeginGC(at, Reading{RSS: 30_000_000, HWM: 100_000_000}, 64<<20)
		cycle := c.EndGC(at.Add(time.Millisecond), Reading{RSS: 40_000_000, HWM: 100_000_000}, 0, false)
		if cycle.PeakExact || cycle.BlockRefs != nil || cycle.PeakRSSMB != 40 {
			t.Fatalf("unproven cycle %+v", cycle)
		}
	}
	d := c.Days(start)[0]
	if len(d.GCCycles) != MaxCycles || !d.GCCut {
		t.Fatalf("unbounded GC: %d %v", len(d.GCCycles), d.GCCut)
	}
}

func TestDSTWindowLength(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	for _, tc := range []struct {
		month      time.Month
		day, hours int
	}{{3, 8, 23}, {11, 1, 25}} {
		start := time.Date(2026, tc.month, tc.day, 0, 0, 0, 0, loc)
		c := New()
		c.Observe(start, Reading{RSS: 1, CPU: 1})
		if d := c.Days(start)[0]; d.WindowSeconds != float64(tc.hours*3600) {
			t.Fatalf("DST window %+v", d)
		}
	}
}

func TestStartJoinsSampler(t *testing.T) {
	c := New()
	stop := c.Start(context.Background())
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sampler did not stop")
	}
}

func TestMidnightInterpolationAndBackwardClock(t *testing.T) {
	start := time.Date(2026, 11, 16, 23, 59, 30, 0, time.UTC)
	c := New()
	c.Observe(start, Reading{RSS: 30_000_000, CPU: 10})
	c.Observe(start.Add(time.Minute), Reading{RSS: 31_000_000, CPU: 12})
	ds := c.Days(start.Add(time.Minute))
	if len(ds) != 2 || ds[0].CPUSeconds != 1 || ds[1].CPUSeconds != 1 || ds[0].CoverageSeconds != 30 || ds[1].CoverageSeconds != 30 {
		t.Fatalf("midnight %+v", ds)
	}
	c.Observe(start.Add(10*time.Second), Reading{RSS: 31_000_000, CPU: 13})
	for _, d := range c.days {
		if !d.clockChanged {
			t.Fatal("backward clock not recorded")
		}
	}
}

func BenchmarkReadSelf(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ReadSelf(); err != nil {
			b.Fatal(err)
		}
	}
}
