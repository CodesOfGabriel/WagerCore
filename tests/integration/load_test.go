//go:build integration

package integration_test

import (
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
	Scenario                 string  `json:"scenario"`
	Processes                int     `json:"processes"`
	Wallets                  int     `json:"wallets"`
	Requests                 int     `json:"requests"`
	Concurrency              int     `json:"concurrency"`
	DurationSeconds          float64 `json:"durationSeconds"`
	RequestsPerSecond        float64 `json:"requestsPerSecond"`
	P50Ms                    float64 `json:"p50Ms"`
	P95Ms                    float64 `json:"p95Ms"`
	P99Ms                    float64 `json:"p99Ms"`
	Errors                   int     `json:"errors"`
	ReconciliationMismatches int     `json:"reconciliationMismatches"`
	OutboxPending            int     `json:"outboxPendingAtEnd"`
	OutboxOldestSeconds      float64 `json:"outboxOldestSecondsAtEnd"`
	LockWaitSeconds          float64 `json:"walletLockSecondsTotal"`
	DatabaseStartSeconds     float64 `json:"databaseTransactionStartSecondsTotal"`
	Conflicts                float64 `json:"concurrencyConflicts"`
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
			eventually(t, 30*time.Second, "opening events published before measurement", func() bool { return db.count(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL") == 0 })
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
			jobs := make(chan int, count)
			results := make(chan outcome, count)
			for i := range count {
				jobs <- i
			}
			close(jobs)
			start := make(chan struct{})
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
			if err := db.conn.QueryRow(db.ctx, "SELECT count(*),min(occurred_at) FROM outbox WHERE published_at IS NULL").Scan(&report.OutboxPending, &oldest); err != nil {
				t.Fatal(err)
			}
			if oldest != nil {
				report.OutboxOldestSeconds = time.Since(*oldest).Seconds()
			}
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

func metricValue(body, name string) float64 {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") {
			value, _ := strconv.ParseFloat(strings.TrimPrefix(line, name+" "), 64)
			return value
		}
	}
	return 0
}
