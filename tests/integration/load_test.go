//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"junglegaming/internal/domain"
)

type loadReport struct {
	Scenario                              string  `json:"scenario"`
	Processes                             int     `json:"processes"`
	Wallets                               int     `json:"wallets"`
	Requests                              int     `json:"requests"`
	Concurrency                           int     `json:"concurrency"`
	DurationSeconds                       float64 `json:"durationSeconds"`
	RequestsPerSecond                     float64 `json:"requestsPerSecond"`
	P50Ms                                 float64 `json:"p50Ms"`
	P95Ms                                 float64 `json:"p95Ms"`
	P99Ms                                 float64 `json:"p99Ms"`
	Errors                                int     `json:"errors"`
	ReconciliationMismatches              int     `json:"reconciliationMismatches"`
	OutboxPending                         int     `json:"outboxPendingAtEnd"`
	OutboxOldestSeconds                   float64 `json:"outboxOldestSecondsAtEnd"`
	LockWaitSeconds                       float64 `json:"walletLockSecondsTotal"`
	DatabaseStartSeconds                  float64 `json:"databaseTransactionStartSecondsTotal"`
	Conflicts                             float64 `json:"concurrencyConflicts"`
	TargetRequestsPerSecond               int     `json:"targetRequestsPerSecond"`
	EventsProduced                        int     `json:"eventsProduced"`
	EventsPublishedDuringLoad             int     `json:"eventsPublishedDuringLoad"`
	EventProductionRate                   float64 `json:"eventsProducedPerSecond"`
	EventPublicationRate                  float64 `json:"eventsPublishedPerSecondDuringLoad"`
	DrainSeconds                          float64 `json:"outboxDrainSeconds"`
	FinancialToPublishedSeconds           float64 `json:"financialToPublishedSeconds"`
	FinancialToPublishedRequestsPerSecond float64 `json:"financialToPublishedRequestsPerSecond"`
	OutboxPendingAfterDrain               int     `json:"outboxPendingAfterDrain"`
	EventsReceived                        int     `json:"eventsReceived"`
	ClaimAverageMs                        float64 `json:"outboxClaimAverageMs"`
	SendAverageMs                         float64 `json:"outboxSendAverageMs"`
	ConfirmAverageMs                      float64 `json:"outboxConfirmAverageMs"`
	PoolWaitSeconds                       float64 `json:"poolEmptyAcquireWaitSecondsTotal"`
}

func loadSetting(t *testing.T, name string, fallback, max int) int {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > max {
		t.Fatalf("%s must be between 1 and %d", name, max)
	}
	return n
}

func TestFinancialLoad(t *testing.T) {
	if os.Getenv("WAGER_LOAD_TEST") != "1" {
		t.Skip("run make benchmark to measure load")
	}
	count, concurrency := loadSetting(t, "BENCHMARK_REQUESTS", 300, 1000), loadSetting(t, "BENCHMARK_CONCURRENCY", 20, 50)
	rate := 0
	if os.Getenv("BENCHMARK_RATE") != "" {
		rate = loadSetting(t, "BENCHMARK_RATE", 20, 1000)
	}
	drainTimeout := time.Minute
	if value := os.Getenv("BENCHMARK_DRAIN_TIMEOUT"); value != "" {
		var err error
		drainTimeout, err = time.ParseDuration(value)
		if err != nil || drainTimeout < time.Second || drainTimeout > 2*time.Minute {
			t.Fatal("BENCHMARK_DRAIN_TIMEOUT must be between 1s and 2m")
		}
	}
	q := queues(t)
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	reports := []loadReport{}
	for _, profile := range []struct {
		name               string
		processes, wallets int
	}{
		{"one_process_100_wallets", 1, 100}, {"three_processes_100_wallets", 3, 100}, {"three_processes_one_wallet", 3, 1},
	} {
		t.Run(profile.name, func(t *testing.T) {
			db := newDatabase(t, true)
			servers := make([]*serverProcess, profile.processes)
			for i := range servers {
				servers[i] = startServer(t, db, "WORKER_ROLES=publisher", "DATABASE_MAX_CONNECTIONS=10", "WORKER_INTERVAL=200ms", "LEASE_DURATION=30s")
			}
			wallets := make([]wallet, profile.wallets)
			for i := range wallets {
				wallets[i] = openAPIWallet(t, servers[0], internal, "100000.00")
			}
			eventually(t, drainTimeout, "opening events published before measurement", func() bool { return db.count(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL") == 0 })
			for {
				messages := q.receive(t, q.events)
				if len(messages) == 0 {
					break
				}
				for _, message := range messages {
					q.delete(t, q.events, message)
				}
			}
			baseline := make([][3]float64, len(servers))
			publisherBaseline := loadPublisherMetrics(t, servers, internal)
			for i, server := range servers {
				metrics := string(expect(t, server, "GET", "/metrics", internal, "", nil, 200, nil))
				baseline[i] = [3]float64{metricValue(metrics, "wager_wallet_lock_duration_seconds_sum"), metricValue(metrics, "wager_database_transaction_start_seconds_sum"), metricValue(metrics, "wager_concurrency_conflicts_total")}
			}
			payloads := make([][]byte, count)
			keys := make([]string, count)
			for i := range count {
				r := request(wallets[i%len(wallets)], domain.Bet, "0.01")
				payloads[i], _ = json.Marshal(r)
				keys[i] = newID()
			}
			type outcome struct {
				index, status int
				latency       time.Duration
				err           error
			}
			jobs := make(chan int)
			results := make(chan outcome, count)
			start := make(chan struct{})
			go func() {
				defer close(jobs)
				<-start
				var tick <-chan time.Time
				if rate > 0 {
					ticker := time.NewTicker(time.Second / time.Duration(rate))
					defer ticker.Stop()
					tick = ticker.C
				}
				for i := range count {
					if tick != nil {
						<-tick
					}
					jobs <- i
				}
			}()
			var workers sync.WaitGroup
			for range concurrency {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					for i := range jobs {
						at := time.Now()
						status, _, err := call(servers[i%len(servers)].URL, "POST", "/wagering/transactions", provider, keys[i], payloads[i])
						results <- outcome{i, status, time.Since(at), err}
					}
				}()
			}
			at := time.Now()
			close(start)
			workers.Wait()
			elapsed := time.Since(at)
			close(results)
			report := loadReport{Scenario: profile.name, Processes: profile.processes, Wallets: profile.wallets, Requests: count, Concurrency: concurrency, DurationSeconds: elapsed.Seconds(), RequestsPerSecond: float64(count) / elapsed.Seconds()}
			report.TargetRequestsPerSecond = rate
			latencies := make([]time.Duration, 0, count)
			processed := make([]int64, len(wallets))
			for result := range results {
				latencies = append(latencies, result.latency)
				if result.err != nil || result.status != 200 {
					report.Errors++
				} else {
					processed[result.index%len(wallets)]++
				}
			}
			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			percentile := func(p int) float64 {
				index := (len(latencies)*p+99)/100 - 1
				return float64(latencies[index]) / float64(time.Millisecond)
			}
			report.P50Ms, report.P95Ms, report.P99Ms = percentile(50), percentile(95), percentile(99)
			var oldest *time.Time
			var databaseNow time.Time
			if err := db.conn.QueryRow(db.ctx, "SELECT count(*),min(occurred_at),clock_timestamp() FROM outbox WHERE published_at IS NULL").Scan(&report.OutboxPending, &oldest, &databaseNow); err != nil {
				t.Fatal(err)
			}
			if oldest != nil {
				report.OutboxOldestSeconds = max(0, databaseNow.Sub(*oldest).Seconds())
			}
			report.EventsProduced = 2 * (count - report.Errors)
			report.EventsPublishedDuringLoad = report.EventsProduced - report.OutboxPending
			report.EventProductionRate = float64(report.EventsProduced) / elapsed.Seconds()
			report.EventPublicationRate = float64(report.EventsPublishedDuringLoad) / elapsed.Seconds()
			drainStarted := time.Now()
			eventually(t, drainTimeout, "all load events published", func() bool {
				report.OutboxPendingAfterDrain = db.count(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL")
				return report.OutboxPendingAfterDrain == 0
			})
			report.DrainSeconds = time.Since(drainStarted).Seconds()
			report.FinancialToPublishedSeconds = time.Since(at).Seconds()
			report.FinancialToPublishedRequestsPerSecond = float64(count-report.Errors) / report.FinancialToPublishedSeconds
			publication := loadPublisherMetrics(t, servers, internal)
			average := func(name string) float64 {
				n := publication[name+"_count"] - publisherBaseline[name+"_count"]
				if n == 0 {
					return 0
				}
				return 1000 * (publication[name+"_sum"] - publisherBaseline[name+"_sum"]) / n
			}
			report.ClaimAverageMs = average("wager_outbox_claim_duration_seconds")
			report.SendAverageMs = average("wager_outbox_send_duration_seconds")
			report.ConfirmAverageMs = average("wager_outbox_confirm_duration_seconds")
			report.PoolWaitSeconds = publication["wager_database_pool_empty_acquire_wait_seconds_total"] - publisherBaseline["wager_database_pool_empty_acquire_wait_seconds_total"]
			report.EventsReceived = assertLoadEvents(t, db, q, report.EventsProduced, drainTimeout)
			for i, w := range wallets {
				var stored, calculated, version int64
				err := db.conn.QueryRow(db.ctx, `SELECT balance_minor,version,COALESCE((SELECT SUM(CASE direction WHEN 'CREDIT' THEN amount_minor::numeric ELSE -amount_minor::numeric END) FROM wallet_ledger WHERE wallet_id=$1),0)::bigint FROM wallets WHERE id=$1`, w.id).Scan(&stored, &version, &calculated)
				if err != nil {
					t.Fatal(err)
				}
				if stored != 10000000-processed[i] || stored != calculated || version != 1+processed[i] {
					report.ReconciliationMismatches++
				}
			}
			for i, server := range servers {
				metrics := string(expect(t, server, "GET", "/metrics", internal, "", nil, 200, nil))
				report.LockWaitSeconds += metricValue(metrics, "wager_wallet_lock_duration_seconds_sum") - baseline[i][0]
				report.DatabaseStartSeconds += metricValue(metrics, "wager_database_transaction_start_seconds_sum") - baseline[i][1]
				report.Conflicts += metricValue(metrics, "wager_concurrency_conflicts_total") - baseline[i][2]
				server.stop(t)
			}
			reports = append(reports, report)
			t.Logf("HTTP %.2f req/s; produced %.2f events/s; published during load %.2f events/s; recovery %.2fs; through publication %.2f req/s; received %d events", report.RequestsPerSecond, report.EventProductionRate, report.EventPublicationRate, report.DrainSeconds, report.FinancialToPublishedRequestsPerSecond, report.EventsReceived)
			if report.Errors != 0 || report.ReconciliationMismatches != 0 {
				t.Errorf("load integrity: errors=%d mismatches=%d", report.Errors, report.ReconciliationMismatches)
			}
			// Cada cenário começa com as filas vazias; o consumo de eventos fica fora da medição HTTP.
			for {
				messages := q.receive(t, q.events)
				if len(messages) == 0 {
					break
				}
				for _, message := range messages {
					q.delete(t, q.events, message)
				}
			}
		})
	}
	output := os.Getenv("BENCHMARK_REPORT")
	if output == "" {
		output = filepath.Join("..", "..", ".local", "benchmark.json")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		t.Fatal(err)
	}
	document := struct {
		At                     time.Time    `json:"at"`
		GoVersion              string       `json:"goVersion"`
		OS                     string       `json:"os"`
		Arch                   string       `json:"arch"`
		CPUs                   int          `json:"logicalCPUs"`
		RaceDetector           bool         `json:"raceDetector"`
		DatabasePoolPerProcess int          `json:"databasePoolPerProcess"`
		PublisherInterval      string       `json:"publisherInterval"`
		Results                []loadReport `json:"results"`
	}{time.Now().UTC(), runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), true, 10, "200ms", reports}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	t.Logf("report: %s", output)
}

func TestOutboxSustainedLoad(t *testing.T) {
	// A bounded sustained profile also runs in CI; the larger benchmarks remain opt-in.
	for key, value := range map[string]string{
		"WAGER_LOAD_TEST": "1", "BENCHMARK_REQUESTS": "60", "BENCHMARK_CONCURRENCY": "10",
		"BENCHMARK_RATE": "20", "BENCHMARK_DRAIN_TIMEOUT": "30s",
		"BENCHMARK_REPORT": filepath.Join(t.TempDir(), "outbox-sustained.json"),
	} {
		t.Setenv(key, value)
	}
	TestFinancialLoad(t)
}

func loadPublisherMetrics(t *testing.T, servers []*serverProcess, bearer string) map[string]float64 {
	t.Helper()
	names := []string{"wager_outbox_claim_duration_seconds", "wager_outbox_send_duration_seconds", "wager_outbox_confirm_duration_seconds"}
	values := map[string]float64{}
	for _, server := range servers {
		body := string(expect(t, server, "GET", "/metrics", bearer, "", nil, 200, nil))
		for _, name := range names {
			for _, suffix := range []string{"_sum", "_count"} {
				values[name+suffix] += metricValue(body, name+suffix)
			}
		}
		name := "wager_database_pool_empty_acquire_wait_seconds_total"
		values[name] += metricValue(body, name)
	}
	return values
}

func assertLoadEvents(t *testing.T, db *database, q *testBroker, count int, timeout time.Duration) int {
	t.Helper()
	type event struct {
		sequence int64
		wallet   string
		payload  []byte
	}
	// Opening snapshots were consumed before the timed load.
	rows, err := db.conn.Query(db.ctx, `SELECT o.event_id::text,o.sequence,o.wallet_id::text,o.payload FROM outbox o
	 JOIN wager_transactions w ON w.id::text=o.payload->'data'->>'transactionId' WHERE w.kind='BET' ORDER BY o.sequence`)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]event{}
	for rows.Next() {
		var id string
		var e event
		if err := rows.Scan(&id, &e.sequence, &e.wallet, &e.payload); err != nil {
			t.Fatal(err)
		}
		expected[id] = e
	}
	rows.Close()
	if rows.Err() != nil || len(expected) != count {
		t.Fatalf("load event count=%d want=%d err=%v", len(expected), count, rows.Err())
	}
	seen := map[string]bool{}
	last := map[string]int64{}
	eventually(t, timeout, "all committed load snapshots received from SQS", func() bool {
		for _, message := range q.receive(t, q.events) {
			var envelope struct {
				EventID string `json:"eventId"`
			}
			if err := json.Unmarshal([]byte(*message.Body), &envelope); err != nil {
				t.Fatal(err)
			}
			e, ok := expected[envelope.EventID]
			if !ok || !bytes.Equal([]byte(*message.Body), e.payload) {
				t.Fatalf("unexpected or modified event %s", envelope.EventID)
			}
			if !seen[envelope.EventID] {
				if e.sequence <= last[e.wallet] {
					t.Fatalf("wallet %s events arrived out of order", e.wallet)
				}
				last[e.wallet] = e.sequence
				seen[envelope.EventID] = true
			}
			q.delete(t, q.events, message)
		}
		return len(seen) == len(expected)
	})
	return len(seen)
}

func metricValue(body, name string) float64 {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") {
			value, _ := strconv.ParseFloat(strings.TrimPrefix(line, name+" "), 64)
			return value
		}
	}
	return 0
}
