package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

type settings struct {
	BaseURL          string `json:"base_url"`
	Clients          int    `json:"clients"`
	Library          int    `json:"library"`
	Rounds           int    `json:"rounds"`
	Warmup           int    `json:"warmup"`
	NeighbourLibrary int    `json:"neighbour_library,omitempty"`
}

type statistics struct {
	N          int     `json:"n"`
	P50MS      float64 `json:"p50_ms"`
	P95MS      float64 `json:"p95_ms"`
	Throughput float64 `json:"throughput_ops_per_sec"`
	DurationMS float64 `json:"duration_ms"`
}

type report struct {
	Settings   settings `json:"settings"`
	Operations struct {
		HistoryChanged     statistics `json:"history_changed"`
		HistoryReplay      statistics `json:"history_replay"`
		FavouritesExchange statistics `json:"favourites_exchange"`
	} `json:"operations"`
	Neighbour *neighbourReport `json:"neighbour,omitempty"`
}

type operation func(*worker, context.Context) error

type workerResult struct {
	samples  []time.Duration
	finished time.Time
	err      error
}

// All workers start together and execute their own sequential requests. A
// phase's throughput uses wall time through completion of the last worker,
// rather than summing request latencies across concurrent workers.
func runPhase(parent context.Context, workers []*worker, rounds int, measured bool, op operation) ([]time.Duration, time.Duration, error) {
	if rounds == 0 {
		return nil, 0, nil
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	start := make(chan struct{})
	ready := make(chan struct{}, len(workers))
	results := make(chan workerResult, len(workers))
	for i, w := range workers {
		go func(i int, w *worker) {
			var samples []time.Duration
			if measured {
				samples = make([]time.Duration, 0, rounds)
			}
			ready <- struct{}{}
			<-start
			for range rounds {
				if err := ctx.Err(); err != nil {
					results <- workerResult{err: fmt.Errorf("worker %d: %w", i+1, err)}
					return
				}
				began := time.Now()
				if err := op(w, ctx); err != nil {
					results <- workerResult{err: fmt.Errorf("worker %d: %w", i+1, err)}
					return
				}
				if measured {
					samples = append(samples, time.Since(began))
				}
			}
			results <- workerResult{samples: samples, finished: time.Now()}
		}(i, w)
	}
	for range workers {
		<-ready
	}
	began := time.Now()
	close(start)
	var samples []time.Duration
	if measured {
		samples = make([]time.Duration, 0, len(workers)*rounds)
	}
	var firstErr error
	var lastFinished time.Time
	for range workers {
		result := <-results
		if result.err != nil && firstErr == nil {
			firstErr = result.err
			cancel()
		}
		if result.finished.After(lastFinished) {
			lastFinished = result.finished
		}
		samples = append(samples, result.samples...)
	}
	return samples, lastFinished.Sub(began), firstErr
}

func summarize(samples []time.Duration, elapsed time.Duration) statistics {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	percentile := func(p float64) float64 {
		index := int(math.Ceil(p*float64(len(samples)))) - 1
		return float64(samples[index]) / float64(time.Millisecond)
	}
	return statistics{
		N: len(samples), P50MS: percentile(0.50), P95MS: percentile(0.95),
		Throughput: float64(len(samples)) / elapsed.Seconds(),
		DurationMS: float64(elapsed) / float64(time.Millisecond),
	}
}

func verifyWorkers(ctx context.Context, workers []*worker) error {
	for i, w := range workers {
		if err := w.verifyPersisted(ctx); err != nil {
			return fmt.Errorf("worker %d persisted-state check: %w", i+1, err)
		}
	}
	return nil
}

func execute(args []string) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg settings
	pauseAfterSeed := fs.Bool("pause-after-seed", false, "emit SEEDED and wait for a newline on stdin before sync (memory runner)")
	fs.StringVar(&cfg.BaseURL, "base-url", "http://127.0.0.1:8080", "loopback HTTP server base URL")
	fs.IntVar(&cfg.Clients, "clients", 4, "number of independent clients")
	fs.IntVar(&cfg.Library, "library", 50, "history and favourite rows per client")
	fs.IntVar(&cfg.Rounds, "rounds", 10, "measured operations per client per phase")
	fs.IntVar(&cfg.Warmup, "warmup", 3, "unmeasured operations per client per phase")
	fs.IntVar(&cfg.NeighbourLibrary, "neighbour-library", 0, "optional one-shot neighbouring history import size (0 disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if cfg.Clients <= 0 || cfg.Library <= 0 || cfg.Rounds <= 0 || cfg.Warmup < 0 || cfg.NeighbourLibrary < 0 {
		return fmt.Errorf("clients, library, and rounds must be positive; warmup and neighbour-library must be nonnegative")
	}
	if *pauseAfterSeed && cfg.NeighbourLibrary != 0 {
		return fmt.Errorf("post-restart memory runs must not register an importer")
	}
	// sort_key is a signed 32-bit integer; row IDs occupy the low 32 bits.
	if int64(cfg.Library) > ((1<<31)-1)/2 {
		return fmt.Errorf("library is too large for fixture sort keys")
	}
	if int64(cfg.NeighbourLibrary) > ((1<<31)-1)/2 {
		return fmt.Errorf("neighbour-library is too large for fixture IDs")
	}
	maxInt := int(^uint(0) >> 1)
	if cfg.Clients > maxInt/cfg.Rounds || cfg.Clients > maxInt/cfg.Library || cfg.Rounds > maxInt-cfg.Warmup {
		return fmt.Errorf("requested workload is too large")
	}
	base, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return err
	}
	cfg.BaseURL = strings.TrimRight(base.String(), "/")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := newAPIClient(base, cfg.Clients)
	defer client.http.CloseIdleConnections()
	body, err := client.request(ctx, http.MethodGet, "/", "", nil, http.StatusOK)
	if err != nil {
		return fmt.Errorf("health precondition: %w", err)
	}
	if strings.TrimSpace(string(body)) != "Alive" {
		return fmt.Errorf("health precondition: GET / did not return Alive")
	}

	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate fixture identity: %w", err)
	}
	runID := hex.EncodeToString(nonce[:16])
	password := base64.RawURLEncoding.EncodeToString(nonce[16:])
	workers := make([]*worker, 0, cfg.Clients)
	userIDs := make(map[int64]bool, cfg.Clients)
	for i := range cfg.Clients {
		email := fmt.Sprintf("load-%s-%d@example.invalid", runID, i+1)
		token, userID, err := client.authenticate(ctx, email, password)
		if err != nil {
			return fmt.Errorf("worker %d setup: %w", i+1, err)
		}
		if userIDs[userID] {
			return fmt.Errorf("worker %d setup: account ID is not unique", i+1)
		}
		userIDs[userID] = true
		w := newWorker(client, token, userID, cfg.Library)
		if err := w.checkEmpty(ctx); err != nil {
			return fmt.Errorf("worker %d precondition: %w", i+1, err)
		}
		if err := w.seed(ctx); err != nil {
			return fmt.Errorf("worker %d seed: %w", i+1, err)
		}
		workers = append(workers, w)
	}
	if err := verifyWorkers(ctx, workers); err != nil {
		return fmt.Errorf("fixture precondition: %w", err)
	}
	if *pauseAfterSeed {
		// Keep the fixture's JWTs and device state only in this process. The
		// matrix restarts the server on its original port before releasing us.
		client.http.CloseIdleConnections()
		if _, err := fmt.Fprintln(os.Stdout, "SEEDED"); err != nil {
			return err
		}
		var release [1]byte
		if _, err := io.ReadFull(os.Stdin, release[:]); err != nil || release[0] != '\n' {
			return fmt.Errorf("memory runner did not release the seeded client")
		}
	}

	var output report
	output.Settings = cfg
	phases := []struct {
		name   string
		op     operation
		output *statistics
	}{
		{"history_changed", (*worker).changeHistory, &output.Operations.HistoryChanged},
		{"history_replay", (*worker).replayHistory, &output.Operations.HistoryReplay},
		{"favourites_exchange", (*worker).exchangeFavourites, &output.Operations.FavouritesExchange},
	}
	for _, phase := range phases {
		if _, _, err := runPhase(ctx, workers, cfg.Warmup, false, phase.op); err != nil {
			return fmt.Errorf("%s warmup: %w", phase.name, err)
		}
		samples, elapsed, err := runPhase(ctx, workers, cfg.Rounds, true, phase.op)
		if err != nil {
			return fmt.Errorf("%s: %w", phase.name, err)
		}
		if err := verifyWorkers(ctx, workers); err != nil {
			return fmt.Errorf("%s: %w", phase.name, err)
		}
		*phase.output = summarize(samples, elapsed)
	}
	if cfg.NeighbourLibrary > 0 {
		result, err := runNeighbour(ctx, base, workers, cfg.NeighbourLibrary, runID, password, userIDs)
		if err != nil {
			return fmt.Errorf("neighbour profile: %w", err)
		}
		output.Neighbour = &result
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
}
