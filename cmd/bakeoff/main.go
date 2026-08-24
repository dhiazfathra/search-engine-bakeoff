package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"time"
)

const topK = 10

type shapeResult struct {
	Shape       Shape   `json:"shape"`
	Supported   bool    `json:"supported"`
	Recall10    float64 `json:"recall_at_10"` // -1 => NOT MEASURED
	MRR10       float64 `json:"mrr_at_10"`    // -1 => NOT MEASURED
	P50ms       float64 `json:"p50_ms"`
	P95ms       float64 `json:"p95_ms"`
	P99ms       float64 `json:"p99_ms"`
	MaxMs       float64 `json:"max_ms"`
	QPS         float64 `json:"qps"`
	Samples     int     `json:"samples"`
	Concurrency int     `json:"concurrency"`
}

type engineResult struct {
	Engine       string        `json:"engine"`
	CorpusSize   int           `json:"corpus_size"`
	BuildSeconds float64       `json:"index_build_seconds"`
	IndexBytes   int64         `json:"index_bytes"`
	Shapes       []shapeResult `json:"shapes"`
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	i := int(math.Ceil(p/100*float64(len(xs)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(xs) {
		i = len(xs) - 1
	}
	return xs[i]
}

// judge computes recall@10 and MRR@10 for one query result.
func judge(got []int, rel []int) (float64, float64) {
	set := map[int]bool{}
	for _, id := range rel {
		set[id] = true
	}
	hits, rr := 0, 0.0
	for i, id := range got {
		if set[id] {
			hits++
			if rr == 0 {
				rr = 1 / float64(i+1)
			}
		}
	}
	return float64(hits) / float64(len(rel)), rr
}

func runShape(ctx context.Context, e Engine, qs []Query, conc, reps int) shapeResult {
	res := shapeResult{Shape: qs[0].Shape, Supported: true, Recall10: -1, MRR10: -1, Concurrency: conc}

	// correctness pass (single-threaded, not timed)
	if qs[0].Relevant != nil {
		var rSum, mSum float64
		for _, q := range qs {
			got, err := e.Search(ctx, q, topK)
			if errors.Is(err, errUnsupported) {
				res.Supported = false
				return res
			}
			if err != nil {
				log.Fatalf("%s search: %v", e.Name(), err)
			}
			r, m := judge(got, q.Relevant)
			rSum += r
			mSum += m
		}
		res.Recall10 = rSum / float64(len(qs))
		res.MRR10 = mSum / float64(len(qs))
	} else if _, err := e.Search(ctx, qs[0], topK); errors.Is(err, errUnsupported) {
		res.Supported = false
		return res
	}

	iters := len(qs) * 8
	if iters < 400 {
		iters = 400
	}
	var lat []float64
	var total time.Duration
	for rep := 0; rep < reps; rep++ {
		var mu sync.Mutex
		local := []float64{}
		start := time.Now()
		var wg sync.WaitGroup
		for w := 0; w < conc; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				buf := []float64{}
				for i := w; i < iters; i += conc {
					q := qs[i%len(qs)]
					t0 := time.Now()
					if _, err := e.Search(ctx, q, topK); err != nil {
						log.Fatalf("%s search: %v", e.Name(), err)
					}
					buf = append(buf, float64(time.Since(t0).Microseconds())/1000)
				}
				mu.Lock()
				local = append(local, buf...)
				mu.Unlock()
			}(w)
		}
		wg.Wait()
		d := time.Since(start)
		if rep == 0 {
			continue // warm-up, discarded
		}
		lat = append(lat, local...)
		total += d
	}
	sort.Float64s(lat)
	res.P50ms, res.P95ms, res.P99ms = pct(lat, 50), pct(lat, 95), pct(lat, 99)
	res.MaxMs = pct(lat, 100)
	res.Samples = len(lat)
	res.QPS = float64(len(lat)) / total.Seconds()
	return res
}

func main() {
	var (
		n      = flag.Int("n", 100000, "corpus size (documents)")
		engine = flag.String("engine", "", "postgres|elasticsearch|meilisearch")
		conc   = flag.Int("c", 8, "query concurrency")
		reps   = flag.Int("reps", 4, "repetitions; the first is discarded as warm-up")
		out    = flag.String("out", "", "write JSON result here")
		pgDSN  = flag.String("pg", "postgres://bakeoff:bakeoff@localhost:55433/bakeoff?sslmode=disable", "")
		esURL  = flag.String("es", "http://localhost:9201", "")
		msURL  = flag.String("ms", "http://localhost:7701", "")
		msKey  = flag.String("mskey", "bakeoffmasterkey", "")
		phase  = flag.String("phase", "both", "load|query|both; splitting them lets the "+
			"caller hold the shared benchmark lock for the query phase only")
		state = flag.String("state", "", "file the load phase writes build metrics to and "+
			"the query phase reads them back from")
	)
	flag.Parse()

	ctx := context.Background()
	var e Engine
	switch *engine {
	case "postgres":
		pg, err := newPG(*pgDSN)
		if err != nil {
			log.Fatal(err)
		}
		e = pg
	case "elasticsearch":
		e = &esEngine{url: *esURL}
	case "meilisearch":
		e = &meiliEngine{url: *msURL, key: *msKey}
	default:
		log.Fatalf("unknown engine %q", *engine)
	}

	p := buildPlan(*n)
	log.Printf("engine=%s n=%d queries=%d", e.Name(), *n, len(p.queries))

	r := engineResult{Engine: e.Name(), CorpusSize: *n}
	if *phase != "query" {
		t0 := time.Now()
		if err := e.Load(ctx, *n, p); err != nil {
			log.Fatalf("load: %v", err)
		}
		r.BuildSeconds = time.Since(t0).Seconds()
		size, err := e.IndexBytes(ctx)
		if err != nil {
			log.Fatalf("index size: %v", err)
		}
		r.IndexBytes = size
		log.Printf("built in %.1fs, index %d bytes", r.BuildSeconds, size)
		if *state != "" {
			b, _ := json.Marshal(r)
			if err := os.WriteFile(*state, b, 0o644); err != nil {
				log.Fatal(err)
			}
		}
		if *phase == "load" {
			return
		}
	} else if *state != "" {
		b, err := os.ReadFile(*state)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.Unmarshal(b, &r); err != nil {
			log.Fatal(err)
		}
	}

	byShape := map[Shape][]Query{}
	for _, q := range p.queries {
		byShape[q.Shape] = append(byShape[q.Shape], q)
	}
	for _, s := range shapes {
		qs := byShape[s]
		if len(qs) == 0 {
			continue
		}
		sr := runShape(ctx, e, qs, *conc, *reps)
		r.Shapes = append(r.Shapes, sr)
		log.Printf("  %-7s supported=%v recall@10=%.3f mrr@10=%.3f p50=%.2fms p99=%.2fms qps=%.0f",
			s, sr.Supported, sr.Recall10, sr.MRR10, sr.P50ms, sr.P99ms, sr.QPS)
	}

	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
	if *out != "" {
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
