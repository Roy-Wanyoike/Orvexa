package main

// The runner: fixed-rate open-loop pacing per ramp step with per-scenario
// accounting. Requests are issued on ticker ticks at each step's target rate;
// each request carries its own timeout; in-flight concurrency is bounded and
// overflow is counted, never silently queued.

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// scenarioAcc is one stream's per-step accumulator.
type scenarioAcc struct {
	hist     *Hist
	issued   int
	ok       int
	errors   int
	statuses map[string]int
	classes  map[string]int
}

func newScenarioAcc() *scenarioAcc {
	return &scenarioAcc{
		hist:     NewHist(32_000),
		statuses: map[string]int{},
		classes:  map[string]int{},
	}
}

// record adds one result (mutex; contention at these rates is negligible).
func (a *scenarioAcc) record(res Result) {
	a.issued++
	if res.ok() {
		a.ok++
	} else {
		a.errors++
	}
	if res.Status != 0 {
		a.statuses[strconv.Itoa(res.Status)]++
	} else {
		a.classes[classTransportErr]++
	}
	a.classes[res.Class]++
	a.hist.Add(res.LatencyMS)
}

// snapshot renders the accumulator into the report shape.
func (a *scenarioAcc) snapshot() ScenarioStat {
	st := ScenarioStat{
		Issued:    a.issued,
		OK:        a.ok,
		Errors:    a.errors,
		Statuses:  a.statuses,
		Classes:   a.classes,
		LatencyMS: a.hist.Snapshot(),
	}
	if a.issued > 0 {
		st.ErrorRate = float64(a.errors) / float64(a.issued)
	}
	return st
}

// runner executes ramp steps.
type runner struct {
	client      *Client
	work        *workload
	names       []string
	maxInflight int
}

func newRunner(c *Client, w *workload, names []string, maxInflight int) *runner {
	if maxInflight < 1 {
		maxInflight = 1
	}
	return &runner{client: c, work: w, names: names, maxInflight: maxInflight}
}

// step issues requests at a fixed rate for dur and blocks until every
// in-flight request has settled (or its timeout fired). Warmup steps are
// marked and excluded from the report by the caller.
func (r *runner) step(ctx context.Context, index int, rps float64, dur time.Duration, warmup bool) (Step, error) {
	started := time.Now()
	accs := make(map[string]*scenarioAcc, len(r.names))
	for _, n := range r.names {
		accs[n] = newScenarioAcc()
	}
	total := newScenarioAcc()

	var (
		accMu    sync.Mutex
		wg       sync.WaitGroup
		overflow int
	)
	inflight := make(chan struct{}, r.maxInflight)

	interval := time.Duration(float64(time.Second) / rps)
	if interval < 100*time.Microsecond {
		interval = 100 * time.Microsecond
	}

	var scnIdx atomic.Uint64
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

issued:
	for {
		select {
		case <-ctx.Done():
			break issued
		case <-ticker.C:
			select {
			case inflight <- struct{}{}:
			default:
				overflow++ // shed locally: never issued, never measured
				continue
			}
			name := r.names[int((scnIdx.Add(1)-1)%uint64(len(r.names)))]
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				defer func() { <-inflight }()
				res := issueOne(ctx, r.client, r.work, name)
				accMu.Lock()
				accs[name].record(res)
				total.record(res)
				accMu.Unlock()
			}(name)
			if time.Since(started) >= dur {
				break issued
			}
		}
	}

	wg.Wait() // requests carry their own timeout; bounded by it
	elapsed := time.Since(started)

	stepOut := Step{
		Index:     index,
		TargetRPS: rps,
		Duration:  elapsed.Seconds(),
		StartedAt: started.UTC().Format(time.RFC3339Nano),
		Warmup:    warmup,
		Scenarios: make(map[string]ScenarioStat, len(accs)),
	}
	for name, acc := range accs {
		stepOut.Scenarios[name] = acc.snapshot()
	}
	stepOut.Total = total.snapshot()
	stepOut.Total.AchievedRPS = float64(stepOut.Total.Issued) / elapsed.Seconds()
	if overflow > 0 {
		stepOut.Total.Classes["inflight_overflow"] = overflow
	}
	return stepOut, nil
}
