// Package resources keeps bounded, in-memory local-day process statistics.
package resources

import (
	"context"
	"math/bits"
	"sync"
	"time"
)

const Every = time.Minute
const MaxCycles = 128

// Reading is process resident memory, cumulative CPU seconds and lifetime RSS high water.
type Reading struct {
	RSS, HWM uint64
	CPU      float64
}

type Cycle struct {
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	PeakRSSMB       float64   `json:"peak_rss_MB"`
	PeakExact       bool      `json:"peak_exact"`
	BlockRefs       *int64    `json:"block_refs,omitempty"`
	Budget          int64     `json:"budget"`
	CPUSeconds      float64   `json:"cpu_s"`
	Seconds         float64   `json:"seconds"`
	CoverageSeconds float64   `json:"coverage_seconds"`
}

type Day struct {
	Date            string    `json:"date"`
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	WindowSeconds   float64   `json:"window_seconds"`
	CoverageSeconds float64   `json:"coverage_seconds"`
	Complete        bool      `json:"complete"`
	Samples         uint64    `json:"samples"`
	RSSMedianMB     *float64  `json:"rss_median_MB"`
	CPUSeconds      float64   `json:"cpu_s"`
	NonGCSeconds    float64   `json:"non_gc_seconds"`
	NonGCCPUSeconds float64   `json:"cpu_non_gc_s"`
	GCSeconds       float64   `json:"gc_seconds"`
	GCCPUSeconds    float64   `json:"gc_cpu_s"`
	GCCycles        []Cycle   `json:"gc_cycles"`
	GCCut           bool      `json:"gc_cycles_cut"`
}

// The first 256 buckets are decimal MB; the remaining buckets double in width.
type histogram [312]uint64

func bucket(n uint64) int {
	if n < 256_000_000 {
		return int(n / 1_000_000)
	}
	return 256 + bits.Len64(n/256_000_000) - 1
}
func (h *histogram) add(n uint64) { h[bucket(n)]++ }
func (h *histogram) median() *float64 {
	var n uint64
	for _, c := range h {
		n += c
	}
	if n == 0 {
		return nil
	}
	rank := func(r uint64) float64 {
		var seen uint64
		for i, c := range h {
			seen += c
			if seen > r {
				if i < 256 {
					return float64(i) + 0.5
				}
				return 384 * float64(uint64(1)<<uint(i-256))
			}
		}
		panic("histogram rank")
	}
	v := (rank((n-1)/2) + rank(n/2)) / 2
	return &v
}

type daily struct {
	Day
	rss          histogram
	clockChanged bool
}
type Collector struct {
	mu    sync.Mutex
	days  map[string]*daily
	last  time.Time
	prev  Reading
	valid bool
	gc    *Cycle
	gcHWM uint64
}

func New() *Collector { return &Collector{days: make(map[string]*daily)} }
func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
func (c *Collector) prune(now time.Time) {
	today := midnight(now)
	yesterday := today.AddDate(0, 0, -1)
	for k, d := range c.days {
		if d.Start.Before(yesterday) || !d.Start.Before(today.AddDate(0, 0, 1)) {
			delete(c.days, k)
		}
	}
}
func (c *Collector) day(t time.Time) *daily {
	start := midnight(t)
	key := start.Format("2006-01-02")
	d := c.days[key]
	if d == nil {
		end := start.AddDate(0, 0, 1)
		d = &daily{Day: Day{Date: key, Start: start, End: end, WindowSeconds: end.Sub(start).Seconds(), GCCycles: []Cycle{}}}
		c.days[key] = d
	}
	return d
}

// advance splits CPU at midnight in proportion to elapsed wall time. GC boundary
// readings make attribution independent of the regular sampling interval.
func (c *Collector) advance(now time.Time, r Reading) {
	// Round(0) drops the monotonic reading: Before/Sub would otherwise compare
	// monotonic clocks and never see a wall clock step, which day boundaries use.
	now = now.Round(0)
	// A backward wall clock can otherwise count an interval twice and fill a
	// partial day with duplicate coverage. Keep that day ineligible for export.
	if !c.last.IsZero() && now.Before(c.last) {
		for _, d := range c.days {
			d.clockChanged = true
		}
		c.day(now).clockChanged = true
	}
	if c.valid && now.After(c.last) && now.Sub(c.last) <= 90*time.Second && r.CPU >= c.prev.CPU && r.RSS > 0 {
		seconds := now.Sub(c.last).Seconds()
		delta := r.CPU - c.prev.CPU
		for at := c.last; at.Before(now); {
			d := c.day(at)
			end := now
			if d.End.Before(end) {
				end = d.End
			}
			wall := end.Sub(at).Seconds()
			cpu := delta * wall / seconds
			d.CoverageSeconds += wall
			d.CPUSeconds += cpu
			if c.gc != nil {
				d.GCSeconds += wall
				d.GCCPUSeconds += cpu
				c.gc.CPUSeconds += cpu
				c.gc.CoverageSeconds += wall
			} else {
				d.NonGCSeconds += wall
				d.NonGCCPUSeconds += cpu
			}
			at = end
		}
	}
	c.last, c.prev, c.valid = now, r, r.RSS > 0 && r.CPU >= 0
	c.peak(r)
	c.prune(now)
}
func (c *Collector) peak(r Reading) {
	if c.gc == nil {
		return
	}
	c.gc.PeakRSSMB = max(c.gc.PeakRSSMB, float64(r.RSS)/1e6)
	if r.HWM > c.gcHWM && c.gcHWM > 0 {
		c.gc.PeakRSSMB = max(c.gc.PeakRSSMB, float64(r.HWM)/1e6)
		c.gc.PeakExact = true
	}
}
func (c *Collector) Observe(now time.Time, r Reading) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advance(now, r)
	if r.RSS > 0 && c.gc == nil {
		d := c.day(now)
		d.rss.add(r.RSS)
		d.Samples++
	}
}

// Checkpoint observes transient GC allocations without adding an RSS median sample.
func (c *Collector) Checkpoint() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gc == nil {
		return
	}
	r, err := ReadSelf()
	if err != nil {
		c.valid = false
		return
	}
	c.advance(time.Now(), r)
}
func (c *Collector) Failed() { c.mu.Lock(); c.valid = false; c.mu.Unlock() }
func (c *Collector) BeginGC(now time.Time, r Reading, budget int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advance(now, r)
	c.gc = &Cycle{Start: now, Budget: budget}
	c.gcHWM = r.HWM
	c.peak(r)
}
func (c *Collector) EndGC(now time.Time, r Reading, refs int64, known bool) Cycle {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advance(now, r)
	if c.gc == nil {
		return Cycle{}
	}
	cycle := *c.gc
	if r.RSS == 0 || r.HWM == 0 {
		cycle.PeakExact = false
	}
	cycle.End = now
	cycle.Seconds = now.Sub(cycle.Start).Seconds()
	if known {
		cycle.BlockRefs = &refs
	}
	c.gc = nil
	// A long or suspended GC may cross more than two days. Only retained days
	// receive details; duration coverage remains restricted to valid readings.
	for _, d := range c.days {
		if !cycle.Start.Before(d.End) || !cycle.End.After(d.Start) {
			continue
		}
		if len(d.GCCycles) < MaxCycles {
			d.GCCycles = append(d.GCCycles, cycle)
		} else {
			d.GCCut = true
		}
	}
	return cycle
}
func (c *Collector) Days(now time.Time) []Day {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	out := make([]Day, 0, 2)
	today := midnight(now)
	for _, at := range []time.Time{today.AddDate(0, 0, -1), today} {
		if d := c.days[at.Format("2006-01-02")]; d != nil {
			v := d.Day
			v.RSSMedianMB = d.rss.median()
			v.Complete = !d.clockChanged && !now.Before(v.End) && v.CoverageSeconds >= v.WindowSeconds-0.000001 && v.CoverageSeconds <= v.WindowSeconds+0.000001
			if c.gc != nil && c.gc.Start.Before(v.End) {
				v.Complete = false
			}
			v.GCCycles = append([]Cycle{}, v.GCCycles...)
			out = append(out, v)
		}
	}
	return out
}

// Run owns exactly one ticker and exits on cancellation, even while offline.
func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(Every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r, err := ReadSelf()
			if err != nil {
				c.Failed()
			} else {
				c.Observe(time.Now(), r)
			}
		}
	}
}

// Start takes an initial reading and returns a function that cancels and joins
// the sampler. Callers defer it for all return paths.
func (c *Collector) Start(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	if r, err := ReadSelf(); err == nil {
		c.Observe(time.Now(), r)
	}
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	return func() { cancel(); <-done }
}
