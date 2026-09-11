package main

// Unit tests for the statistics/report layer: exact nearest-rank percentiles,
// summary math, concurrent histogram accumulation, and the report shape.

import (
	"encoding/json"
	"math"
	"sync"
	"testing"
)

func TestPercentileSorted(t *testing.T) {
	empty := []float64{}
	if got := PercentileSorted(empty, 50); got != 0 {
		t.Fatalf("empty p50 = %v, want 0", got)
	}
	one := []float64{7}
	for _, p := range []float64{0, 1, 50, 95, 99, 100} {
		if got := PercentileSorted(one, p); got != 7 {
			t.Fatalf("single-sample p%v = %v, want 7", p, got)
		}
	}
	// 1..100 sorted: p50 -> 50th value? nearest-rank ceil(0.5*100)=50 -> 50.
	hundred := make([]float64, 100)
	for i := range hundred {
		hundred[i] = float64(i + 1)
	}
	cases := map[float64]float64{
		0:   1,
		50:  50,
		95:  95,
		99:  99,
		100: 100,
	}
	for p, want := range cases {
		if got := PercentileSorted(hundred, p); got != want {
			t.Fatalf("p%v = %v, want %v", p, got, want)
		}
	}
	// nearest-rank with ceil: p50 of 1..101 -> ceil(0.5*101)=51 -> 51
	odd := make([]float64, 101)
	for i := range odd {
		odd[i] = float64(i + 1)
	}
	if got := PercentileSorted(odd, 50); got != 51 {
		t.Fatalf("p50 of 1..101 = %v, want 51", got)
	}
}

func TestSummarize(t *testing.T) {
	s := Summarize([]float64{3, 1, 2}) // sorts to 1,2,3
	if s.N != 3 || s.Min != 1 || s.Max != 3 || s.P50 != 2 || s.P95 != 3 || s.P99 != 3 {
		t.Fatalf("summary = %+v", s)
	}
	if math.Abs(s.Mean-2) > 1e-9 {
		t.Fatalf("mean = %v, want 2", s.Mean)
	}
	if zero := Summarize(nil); zero.N != 0 {
		t.Fatalf("empty summary = %+v", zero)
	}
}

func TestHistConcurrentAdd(t *testing.T) {
	h := NewHist(0)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				h.Add(float64(i%100) + 0.5)
			}
		}()
	}
	wg.Wait()
	s := h.Snapshot()
	if s.N != 8000 {
		t.Fatalf("N = %d, want 8000", s.N)
	}
	if s.Min != 0.5 || s.Max != 99.5 {
		t.Fatalf("min/max = %v/%v, want 0.5/99.5", s.Min, s.Max)
	}
	// 80 copies of each value v in {0.5..99.5}: nearest-rank over the exact
	// multiplicities (verified by hand).
	if s.P50 != 49.5 || s.P95 != 94.5 || s.P99 != 98.5 {
		t.Fatalf("percentiles = %+v, want p50=49.5 p95=94.5 p99=98.5", s)
	}
}

// TestReportJSONShape: the structured output must marshal the fields the
// docs/slo.md table is generated from.
func TestReportJSONShape(t *testing.T) {
	rep := &Report{
		Meta: Meta{
			Target: "http://127.0.0.1:18080", RampRPS: []float64{50}, StepSeconds: 60,
			Scenarios: []string{scnWebhook}, APIKeys: 4,
		},
		Steps: []Step{{
			Index: 1, TargetRPS: 50, Duration: 60, Scenarios: map[string]ScenarioStat{
				scnWebhook: {Issued: 3000, OK: 2999, Errors: 1, ErrorRate: 1.0 / 3000,
					LatencyMS: HistSummary{N: 3000, Min: 1, P50: 2, P95: 5, P99: 9, Max: 20, Mean: 2.5},
					Statuses:  map[string]int{"202": 2999, "500": 1},
					Classes:   map[string]int{classOK: 2999, classServerError: 1}},
			},
			Total: ScenarioStat{Issued: 3000, OK: 2999, Errors: 1},
		}},
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	step := m["steps"].([]any)[0].(map[string]any)
	sc := step["scenarios"].(map[string]any)[scnWebhook].(map[string]any)
	lat := sc["latency_ms"].(map[string]any)
	for _, key := range []string{"p50", "p95", "p99", "min", "max", "mean", "n"} {
		if _, ok := lat[key]; !ok {
			t.Fatalf("latency_ms missing %q", key)
		}
	}
	if _, ok := sc["error_rate"]; !ok {
		t.Fatal("scenario missing error_rate")
	}
	if _, ok := sc["status_counts"]; !ok {
		t.Fatal("scenario missing status_counts")
	}
}
