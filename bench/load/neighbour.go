package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const minimumNeighbourP95Samples = 100

type neighbourSmallStats struct {
	N                int      `json:"n"`
	P50MS            *float64 `json:"p50_ms"`
	P95MS            *float64 `json:"p95_ms"`
	Throughput       float64  `json:"throughput_ops_per_sec"`
	P95MinSamples    int      `json:"p95_min_samples"`
	P95Interpretable bool     `json:"p95_interpretable"`
	Note             string   `json:"note"`
}

type neighbourReport struct {
	ImportedRows     int                 `json:"imported_rows"`
	Readers          int                 `json:"readers"`
	ImportDurationMS float64             `json:"import_duration_ms"`
	SmallOps         neighbourSmallStats `json:"small_ops"`
	StartBarrier     string              `json:"start_barrier"`
	SampleWindow     string              `json:"sample_window"`
	OverlapCaveat    string              `json:"overlap_caveat"`
}

type neighbourSample struct {
	duration  time.Duration
	completed time.Time
}

type neighbourReaderResult struct {
	index   int
	samples []neighbourSample
	err     error
}

type neighbourImportResult struct {
	started  time.Time
	finished time.Time
	err      error
}

// A shared release channel opens only after the importer and every reader have
// reached it. It synchronizes client starts, not execution inside the server.
// Readers finish any request already in flight after the import ends, but only
// completions in the importer's HTTP interval contribute samples.
func runNeighbour(parent context.Context, base *url.URL, readers []*worker, library int, runID, password string, knownIDs map[int64]bool) (neighbourReport, error) {
	var output neighbourReport
	client := newAPIClient(base, 1)
	defer client.http.CloseIdleConnections()
	email := fmt.Sprintf("load-%s-neighbour@example.invalid", runID)
	token, userID, err := client.authenticate(parent, email, password)
	if err != nil {
		return output, fmt.Errorf("importer setup: %w", err)
	}
	if knownIDs[userID] {
		return output, fmt.Errorf("importer setup: account ID is not unique")
	}
	importer := newHistoryWorker(client, token, userID, library)
	if err := importer.checkEmpty(parent); err != nil {
		return output, fmt.Errorf("importer precondition: %w", err)
	}
	payload, err := json.Marshal(importer.historyPacket())
	if err != nil {
		return output, fmt.Errorf("construct importer packet: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	start := make(chan struct{})
	ready := make(chan struct{}, len(readers)+1)
	done := make(chan struct{})
	readerResults := make(chan neighbourReaderResult, len(readers))
	importResult := make(chan neighbourImportResult, 1)

	for i, reader := range readers {
		go func(i int, reader *worker) {
			var samples []neighbourSample
			ready <- struct{}{}
			<-start
			for {
				select {
				case <-done:
					readerResults <- neighbourReaderResult{index: i, samples: samples}
					return
				default:
				}
				began := time.Now()
				if err := reader.changeHistory(ctx); err != nil {
					readerResults <- neighbourReaderResult{index: i, samples: samples, err: err}
					cancel()
					return
				}
				completed := time.Now()
				samples = append(samples, neighbourSample{duration: completed.Sub(began), completed: completed})
			}
		}(i, reader)
	}
	go func() {
		ready <- struct{}{}
		<-start
		began := time.Now()
		_, err := client.request(ctx, http.MethodPost, "/resource/history", token, payload, http.StatusNoContent)
		finished := time.Now()
		close(done)
		if err != nil {
			cancel()
		}
		importResult <- neighbourImportResult{started: began, finished: finished, err: err}
	}()
	for range len(readers) + 1 {
		<-ready
	}
	close(start)
	imported := <-importResult
	results := make([]neighbourReaderResult, 0, len(readers))
	for range readers {
		results = append(results, <-readerResults)
	}
	// Prefer the originating reader failure over the importer cancellation it caused.
	for _, result := range results {
		if result.err != nil && !errors.Is(result.err, context.Canceled) {
			return output, fmt.Errorf("reader %d: %w", result.index+1, result.err)
		}
	}
	if imported.err != nil {
		return output, fmt.Errorf("import history: %w", imported.err)
	}
	for _, result := range results {
		if result.err != nil {
			return output, fmt.Errorf("reader %d: %w", result.index+1, result.err)
		}
	}
	elapsed := imported.finished.Sub(imported.started)
	if elapsed <= 0 {
		return output, fmt.Errorf("invalid importer duration")
	}

	// Verify only after all timed requests finish. The importer's empty device
	// must pull all cards; readers' probes also validate their latest changes.
	probe, err := json.Marshal(syncPacket{History: []historyRow{}, Timestamp: importer.clock})
	if err != nil {
		return output, err
	}
	body, err := client.request(parent, http.MethodPost, "/resource/history", token, probe, http.StatusOK)
	if err != nil {
		return output, fmt.Errorf("importer persisted-state check: %w", err)
	}
	if err := importer.checkHistory(body); err != nil {
		return output, fmt.Errorf("importer persisted-state check: %w", err)
	}
	if err := verifyWorkers(parent, readers); err != nil {
		return output, fmt.Errorf("reader persisted-state check: %w", err)
	}

	var samples []time.Duration
	for _, result := range results {
		for _, sample := range result.samples {
			if !sample.completed.Before(imported.started) && !sample.completed.After(imported.finished) {
				samples = append(samples, sample.duration)
			}
		}
	}
	output.ImportedRows = library
	output.Readers = len(readers)
	output.ImportDurationMS = float64(elapsed) / float64(time.Millisecond)
	output.SmallOps = neighbourSmallStats{
		N:                len(samples),
		Throughput:       float64(len(samples)) / elapsed.Seconds(),
		P95MinSamples:    minimumNeighbourP95Samples,
		P95Interpretable: len(samples) >= minimumNeighbourP95Samples,
	}
	if len(samples) > 0 {
		stats := summarize(samples, elapsed)
		output.SmallOps.P50MS = &stats.P50MS
		if output.SmallOps.P95Interpretable {
			output.SmallOps.P95MS = &stats.P95MS
		}
	}
	if !output.SmallOps.P95Interpretable {
		output.SmallOps.Note = "Fewer than 100 reader completions in the import window; do not draw percentile conclusions."
	} else {
		output.SmallOps.Note = "Percentiles describe this single client-side trial; repeat trials before drawing conclusions."
	}
	output.StartBarrier = "Importer and all readers are released together only after every goroutine is ready; importer authentication and packet construction are outside the timed interval."
	output.SampleWindow = "Only successful reader operations completed between importer HTTP start and finish are counted; later in-flight operations are awaited and verified but excluded."
	output.OverlapCaveat = "Client-side timing does not prove that server-side writes overlapped."
	return output, nil
}
