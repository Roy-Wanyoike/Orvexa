package main

// Report model + latency statistics. Percentiles use the nearest-rank method
// on the exact sample set (no bucketing error, no estimation) — at these
// sample volumes (<= ~25k per scenario per step) a plain sort is fast enough
// and keeps the evidence auditable.

import (
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"time"
)

// Scenario names.
const (
	scnWebhook        = "webhook"
	scnInteraction    = "interaction"
	scnCustomerCreate = "customer-create"
	scnCustomerList   = "customer-list"
)

// Meta is the run-level context block of the report.
type Meta struct {
	Target          string      `json:"target"`
	StartedAt       string      `json:"started_at"`
	FinishedAt      string      `json:"finished_at,omitempty"`
	RampRPS         []float64   `json:"ramp_rps"`
	StepSeconds     float64     `json:"step_seconds"`
	WarmupSeconds   float64     `json:"warmup_seconds"`
	Scenarios       []string    `json:"scenarios"`
	APIKeys         int         `json:"api_keys"`
	SourceIPsProbed int         `json:"source_ips_probed"`
	SourceIPsUsed   int         `json:"source_ips_used"`
	SourceIPsNote   string      `json:"source_ips_note,omitempty"`
	Seeded          SeedSummary `json:"seeded"`
	GoVersion       string      `json:"go_version"`
	GOMAXPROCS      int         `json:"gomaxprocs"`
	NumCPU          int         `json:"num_cpu"`
	OS              string      `json:"os"`
	Arch            string      `json:"arch"`
	Interrupted     bool        `json:"interrupted"`
}

// SeedSummary records the pre-run seeding (not part of measurements).
type SeedSummary struct {
	Customers    int `json:"customers"`
	Interactions int `json:"interactions"`
}

// Report is the structured output of one loadgen campaign.
type Report struct {
	Meta  Meta   `json:"meta"`
	Steps []Step `json:"steps"`
}

// Step is one measured ramp step.
type Step struct {
	Index     int                     `json:"step"`
	TargetRPS float64                 `json:"target_rps"`
	Duration  float64                 `json:"duration_seconds"`
	StartedAt string                  `json:"started_at"`
	Warmup    bool                    `json:"warmup,omitempty"`
	Scenarios map[string]ScenarioStat `json:"scenarios"`
	Total     ScenarioStat            `json:"total"`
}

// StepTotal aggregates all scenarios of a step.
type StepTotal struct {
	Issued      int            `json:"issued"`
	OK          int            `json:"ok"`
	Errors      int            `json:"errors"`
	ErrorRate   float64        `json:"error_rate"`
	AchievedRPS float64        `json:"achieved_rps"`
	LatencyMS   HistSummary    `json:"latency_ms"`
	Statuses    map[string]int `json:"status_counts"`
	Classes     map[string]int `json:"outcome_counts"`
}

// ScenarioStat is the per-scenario measured block. Step totals use the same
// shape (the aggregate stream plus the achieved rate).
type ScenarioStat struct {
	Issued      int            `json:"issued"`
	OK          int            `json:"ok"`
	Errors      int            `json:"errors"`
	ErrorRate   float64        `json:"error_rate"`
	AchievedRPS float64        `json:"achieved_rps"`
	LatencyMS   HistSummary    `json:"latency_ms"`
	Statuses    map[string]int `json:"status_counts"`
	Classes     map[string]int `json:"outcome_counts"`
}

// HistSummary is the latency summary in milliseconds.
type HistSummary struct {
	N    int     `json:"n"`
	Min  float64 `json:"min"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// Hist accumulates latency samples (milliseconds) with a per-request cost of
// O(1) and an O(n log n) snapshot. Safe for concurrent use.
type Hist struct {
	mu      sync.Mutex
	samples []float64
}

// NewHist preallocates for the expected sample count (optional hint).
func NewHist(capacity int) *Hist {
	if capacity < 16 {
		capacity = 16
	}
	return &Hist{samples: make([]float64, 0, capacity)}
}

// Add records one latency sample in milliseconds.
func (h *Hist) Add(ms float64) {
	h.mu.Lock()
	h.samples = append(h.samples, ms)
	h.mu.Unlock()
}

// Len reports the sample count.
func (h *Hist) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.samples)
}

// Snapshot returns the summary over the exact sample set.
func (h *Hist) Snapshot() HistSummary {
	h.mu.Lock()
	s := make([]float64, len(h.samples))
	copy(s, h.samples)
	h.mu.Unlock()
	return Summarize(s)
}

// Summarize computes the summary over a sample slice (sorted internally).
func Summarize(samples []float64) HistSummary {
	if len(samples) == 0 {
		return HistSummary{}
	}
	sort.Float64s(samples)
	var sum float64
	for _, v := range samples {
		sum += v
	}
	return HistSummary{
		N:    len(samples),
		Min:  samples[0],
		P50:  PercentileSorted(samples, 50),
		P95:  PercentileSorted(samples, 95),
		P99:  PercentileSorted(samples, 99),
		Max:  samples[len(samples)-1],
		Mean: sum / float64(len(samples)),
	}
}

// PercentileSorted is nearest-rank: the smallest value in the sorted sample
// such that at least p% of samples are <= it. Empty input returns 0.
func PercentileSorted(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// printSummary renders the human-readable step table after the JSON is written.
func printSummary(w io.Writer, report *Report) {
	fmt.Fprintf(w, "== loadgen: %s  ramp=%v  keys=%d  source-ips=%d  seeded(customers=%d, interactions=%d)\n",
		report.Meta.Target, report.Meta.RampRPS, report.Meta.APIKeys, report.Meta.SourceIPsUsed,
		report.Meta.Seeded.Customers, report.Meta.Seeded.Interactions)
	if report.Meta.SourceIPsNote != "" {
		fmt.Fprintf(w, "   note: %s\n", report.Meta.SourceIPsNote)
	}
	for _, s := range report.Steps {
		fmt.Fprintf(w, "step %d: target=%.0frps issued=%d ok=%d err=%d (%.2f%%) achieved=%.1frps\n",
			s.Index, s.TargetRPS, s.Total.Issued, s.Total.OK, s.Total.Errors, s.Total.ErrorRate*100, s.Total.AchievedRPS)
		names := make([]string, 0, len(s.Scenarios))
		for name := range s.Scenarios {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			st := s.Scenarios[name]
			fmt.Fprintf(w, "  %-16s n=%-6d p50=%7.2fms p95=%7.2fms p99=%7.2fms max=%7.2fms err=%.2f%%\n",
				name, st.Issued, st.LatencyMS.P50, st.LatencyMS.P95, st.LatencyMS.P99, st.LatencyMS.Max, st.ErrorRate*100)
		}
	}
	if report.Meta.Interrupted {
		fmt.Fprintln(w, "interrupted: results are partial (graceful stop)")
	}
}

// msSince is the latency helper used by the runner.
func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}
