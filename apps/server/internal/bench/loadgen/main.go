// Command loadgen is the small closed-loop HTTP load generator behind the M0
// baseline (benchmarks/README.md). It has no dependencies beyond the standard
// library so the benchmark runs anywhere the server builds.
//
// Closed loop: each of -c workers sends its next request as soon as the
// previous response is fully read. Latency is therefore service latency under
// that concurrency, not latency at a fixed arrival rate, and it understates
// queueing delay when the target stalls (coordinated omission). Compare runs
// of the same shape; do not read absolute tail latencies as an SLO.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type headers []string

func (h *headers) String() string     { return strings.Join(*h, ", ") }
func (h *headers) Set(v string) error { *h = append(*h, v); return nil }

type result struct {
	Name        string  `json:"name"`
	URL         string  `json:"url"`
	Concurrency int     `json:"concurrency"`
	DurationS   float64 `json:"duration_s"`
	Requests    int     `json:"requests"`
	Errors      int     `json:"errors"`
	RPS         float64 `json:"rps"`
	MeanMs      float64 `json:"mean_ms"`
	P50Ms       float64 `json:"p50_ms"`
	P95Ms       float64 `json:"p95_ms"`
	P99Ms       float64 `json:"p99_ms"`
	MaxMs       float64 `json:"max_ms"`
}

func main() {
	var hdrs headers
	url := flag.String("url", "", "target URL")
	name := flag.String("name", "", "label copied into the result")
	conc := flag.Int("c", 16, "concurrent connections")
	dur := flag.Duration("duration", 5*time.Second, "measured duration")
	warm := flag.Duration("warmup", 2*time.Second, "unmeasured warm-up before the measured window")
	flag.Var(&hdrs, "H", "request header 'Name: value' (repeatable)")
	flag.Parse()
	if *url == "" || *conc < 1 {
		fmt.Fprintln(os.Stderr, "usage: loadgen -url URL [-c N] [-duration D] [-warmup D] [-H 'K: V']")
		os.Exit(2)
	}

	tr := &http.Transport{
		MaxIdleConns:        *conc,
		MaxIdleConnsPerHost: *conc,
		DisableCompression:  true,
		IdleConnTimeout:     time.Minute,
	}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	defer tr.CloseIdleConnections()

	var lastFailure atomic.Value // string: why the most recent request was not a 200
	do := func() (time.Duration, bool) {
		req, err := http.NewRequest(http.MethodGet, *url, nil)
		if err != nil {
			lastFailure.Store(err.Error())
			return 0, false
		}
		for _, h := range hdrs {
			k, v, _ := strings.Cut(h, ":")
			req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
		}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			lastFailure.Store(err.Error())
			return time.Since(start), false
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastFailure.Store(fmt.Sprintf("%s: %s", resp.Status, strings.TrimSpace(string(body))))
		}
		return time.Since(start), resp.StatusCode == http.StatusOK
	}

	// Fail fast with a clear message when the target is not what we expect,
	// instead of reporting a fast run of 401s as a result.
	if _, ok := do(); !ok {
		fmt.Fprintf(os.Stderr, "loadgen: %s did not return 200 (%v)\n", *url, lastFailure.Load())
		os.Exit(1)
	}

	type shard struct {
		lat    []time.Duration
		errors int
	}
	run := func(d time.Duration, record bool) ([]shard, time.Duration) {
		shards := make([]shard, *conc)
		var wg sync.WaitGroup
		start := time.Now()
		deadline := start.Add(d)
		for i := range *conc {
			wg.Go(func() {
				s := &shards[i]
				for time.Now().Before(deadline) {
					lat, ok := do()
					if !record {
						continue
					}
					if !ok {
						s.errors++
						continue
					}
					s.lat = append(s.lat, lat)
				}
			})
		}
		wg.Wait()
		return shards, time.Since(start)
	}

	run(*warm, false)
	shards, elapsed := run(*dur, true)
	if f := lastFailure.Load(); f != nil {
		fmt.Fprintf(os.Stderr, "loadgen: last non-200 response: %v\n", f)
	}

	var all []time.Duration
	errs := 0
	for _, s := range shards {
		all = append(all, s.lat...)
		errs += s.errors
	}
	slices.Sort(all)
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	pick := func(p float64) float64 {
		if len(all) == 0 {
			return 0
		}
		rank := int(math.Ceil(p/100*float64(len(all)))) - 1
		return ms(all[min(max(rank, 0), len(all)-1)])
	}
	var sum time.Duration
	for _, d := range all {
		sum += d
	}
	res := result{
		Name: *name, URL: *url, Concurrency: *conc, DurationS: elapsed.Seconds(),
		Requests: len(all), Errors: errs, RPS: float64(len(all)) / elapsed.Seconds(),
		P50Ms: pick(50), P95Ms: pick(95), P99Ms: pick(99),
	}
	if len(all) > 0 {
		res.MeanMs = ms(sum) / float64(len(all))
		res.MaxMs = ms(all[len(all)-1])
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
}
