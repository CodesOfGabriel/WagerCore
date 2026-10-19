//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"junglegaming/internal/domain"
)

func pauseDependency(t *testing.T, service string) func() {
	t.Helper()
	run := func(action string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", "compose", "-p", "jungle-tests", "-f", "../../docker-compose.test.yml", action, service)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", action, service, err, output)
		}
		return nil
	}
	if err := run("pause"); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			if err := run("unpause"); err != nil {
				t.Error(err)
			} else {
				restored = true
			}
		}
	}
	t.Cleanup(restore)
	return restore
}

func TestPostgresOutagePreservesIdempotency(t *testing.T) {
	db := newDatabase(t, true)
	p := startServer(t, db, "WORKER_ROLES=publisher")
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, p, internal, "100.00")
	r := request(w, domain.Bet, "25.00")
	key := newID()
	restore := pauseDependency(t, "postgres-test")
	var ready map[string]string
	expect(t, p, "GET", "/health/ready", "", "", nil, 503, &ready)
	if ready["postgres"] != "unavailable" || ready["sqs"] != "up" {
		t.Fatalf("readiness=%v", ready)
	}
	expect(t, p, "GET", "/health/live", "", "", nil, 200, nil)
	wager(t, p, provider, r, key, 503)
	restore()
	eventually(t, 5*time.Second, "PostgreSQL readiness recovered", func() bool {
		status, _, err := call(p.URL, "GET", "/health/ready", "", "", nil)
		return err == nil && status == 200
	})
	if db.count(t, "SELECT count(*) FROM wager_transactions WHERE external_transaction_id=$1", r.ExternalTransactionID) != 0 {
		t.Fatal("outage created a terminal financial result")
	}
	first := wager(t, p, provider, r, key, 200)
	replay := wager(t, p, provider, r, key, 200)
	if first.IdempotentReplay || !replay.IdempotentReplay || first.TransactionID != replay.TransactionID {
		t.Fatal("retry did not preserve financial identity")
	}
	db.assertBalance(t, w, 7500, 2)
	eventually(t, 5*time.Second, "publisher recovered after database outage", func() bool { return db.count(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL") == 0 })
}

func TestSQSOutageRetainsCommittedEvents(t *testing.T) {
	q := queues(t)
	db := newDatabase(t, true)
	p := startServer(t, db, "WORKER_ROLES=publisher")
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, p, internal, "100.00")
	eventually(t, 5*time.Second, "opening published", func() bool { return db.count(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL") == 0 })
	restore := pauseDependency(t, "localstack-test")
	var ready map[string]string
	expect(t, p, "GET", "/health/ready", "", "", nil, 503, &ready)
	if ready["sqs"] != "unavailable" || ready["postgres"] != "up" {
		t.Fatalf("readiness=%v", ready)
	}
	r := request(w, domain.Bet, "25.00")
	result := wager(t, p, provider, r, newID(), 200)
	db.assertBalance(t, w, 7500, 2)
	if db.count(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL") != 2 {
		t.Fatal("confirmed events were lost")
	}
	eventually(t, 8*time.Second, "publication timeout observed", func() bool {
		for _, entry := range p.entries() {
			if entry["msg"] == "outbox_publish_retry" && entry["stage"] == "publish" {
				return true
			}
		}
		return false
	})
	restore()
	eventually(t, 8*time.Second, "outbox recovered after SQS outage", func() bool { return db.count(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL") == 0 })
	delivered := map[string]bool{}
	eventually(t, 5*time.Second, "all confirmed events delivered", func() bool {
		for _, message := range q.receive(t, q.events) {
			var body struct {
				EventID string `json:"eventId"`
			}
			if json.Unmarshal([]byte(*message.Body), &body) == nil {
				delivered[body.EventID] = true
			}
			q.delete(t, q.events, message)
		}
		return len(delivered) == 4
	})
	if db.count(t, "SELECT count(*) FROM wallet_ledger WHERE transaction_id=$1", result.TransactionID) != 1 {
		t.Fatal("outbox retry duplicated movement")
	}
}

func TestSchemaFailureCanBeRetriedAfterRepair(t *testing.T) {
	for _, code := range []string{"42P01", "42703", "42883"} {
		t.Run(code, func(t *testing.T) {
			q := queues(t)
			db := newDatabase(t, true)
			p := startServer(t, db, "WORKER_ROLES=consumer", "RETRY_BASE=1s")
			internal, provider := token(t, "internal-service"), token(t, "provider-a")
			w := openAPIWallet(t, p, internal, "100.00")
			_, err := db.conn.Exec(db.ctx, `CREATE FUNCTION fail_test_schema() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'injected schema failure' USING ERRCODE='`+code+`'; END $$;
 CREATE TRIGGER fail_test_schema BEFORE INSERT ON wallet_ledger FOR EACH ROW EXECUTE FUNCTION fail_test_schema()`)
			if err != nil {
				t.Fatal(err)
			}
			r, key := request(w, domain.Bet, "25.00"), newID()
			expect(t, p, "POST", "/wagering/transactions", provider, key, r, 503, nil)
			message := envelope(r, key)
			q.send(t, message)
			eventually(t, 5*time.Second, "schema failure retried by SQS", func() bool {
				for _, entry := range p.entries() {
					if entry["msg"] == "sqs_message_retry" && entry["messageId"] == message.MessageID && entry["sqlState"] == code {
						return true
					}
				}
				return false
			})
			db.assertBalance(t, w, 10000, 1)
			if db.count(t, "SELECT count(*) FROM wager_transactions WHERE external_transaction_id=$1", r.ExternalTransactionID) != 0 || db.count(t, "SELECT count(*) FROM inbox") != 0 || db.count(t, "SELECT count(*) FROM outbox") != 2 {
				t.Fatal("schema failure consumed identity or committed side effects")
			}
			if _, err := db.conn.Exec(db.ctx, "DROP TRIGGER fail_test_schema ON wallet_ledger"); err != nil {
				t.Fatal(err)
			}
			eventually(t, 10*time.Second, "same SQS operation completes after schema repair", func() bool {
				return db.count(t, "SELECT count(*) FROM inbox WHERE message_id=$1 AND completed_at IS NOT NULL", message.MessageID) == 1
			})
			replay := wager(t, p, provider, r, key, 200)
			if replay.Status != domain.Processed || !replay.IdempotentReplay {
				t.Fatalf("replay after repair: %+v", replay)
			}
			db.assertBalance(t, w, 7500, 2)
			if db.count(t, "SELECT count(*) FROM wallet_ledger WHERE transaction_id=$1", replay.TransactionID) != 1 {
				t.Fatal("operation not applied exactly once")
			}
		})
	}
}

func TestPermanentInfrastructureFailureIsAudited(t *testing.T) {
	q := queues(t)
	db := newDatabase(t, true)
	p := startServer(t, db, "WORKER_ROLES=consumer")
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, p, internal, "100.00")
	_, err := db.conn.Exec(db.ctx, `CREATE FUNCTION fail_test_ledger() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'injected invariant failure' USING ERRCODE='23514'; END $$;
 CREATE TRIGGER fail_test_ledger BEFORE INSERT ON wallet_ledger FOR EACH ROW EXECUTE FUNCTION fail_test_ledger()`)
	if err != nil {
		t.Fatal(err)
	}
	r := request(w, domain.Bet, "25.00")
	key := newID()
	first := wager(t, p, provider, r, key, 500)
	if first.Status != domain.Failed || first.FailureCode != domain.InfrastructureFailure || first.Balance != nil {
		t.Fatalf("failed result=%+v", first)
	}
	if _, err := db.conn.Exec(db.ctx, "DROP TRIGGER fail_test_ledger ON wallet_ledger"); err != nil {
		t.Fatal(err)
	}
	replay := wager(t, p, provider, r, key, 500)
	if !replay.IdempotentReplay || replay.TransactionID != first.TransactionID {
		t.Fatal("terminal infrastructure failure was reapplied")
	}
	// A reentrega continua até a DLQ, mesmo com a auditoria da inbox já confirmada.
	message := envelope(r, key)
	q.send(t, message)
	eventually(t, 20*time.Second, "failed transaction reaches DLQ", func() bool {
		for _, received := range q.receive(t, q.dlq) {
			var body struct {
				MessageID string `json:"messageId"`
			}
			_ = json.Unmarshal([]byte(*received.Body), &body)
			q.delete(t, q.dlq, received)
			if body.MessageID == message.MessageID {
				return true
			}
		}
		return false
	})
	db.assertBalance(t, w, 10000, 1)
	if db.count(t, "SELECT count(*) FROM wallet_ledger WHERE transaction_id=$1", first.TransactionID) != 0 || db.count(t, "SELECT count(*) FROM outbox") != 2 || db.count(t, "SELECT count(*) FROM inbox WHERE completed_at IS NOT NULL") != 1 {
		t.Fatal("failed operation left financial side effects or incomplete audit")
	}
	wager(t, p, provider, request(w, domain.Bet, "1.00"), newID(), 200)
	db.assertBalance(t, w, 9900, 2)
}

func waitForWalletLock(t *testing.T, db *database) {
	t.Helper()
	eventually(t, 3*time.Second, "request waiting for wallet lock", func() bool {
		var waiting bool
		if err := db.conn.QueryRow(db.ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))", int(db.conn.PgConn().PID())).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting
	})
}

func waitForGracefulExit(t *testing.T, p *serverProcess) {
	t.Helper()
	select {
	case <-p.done:
		if p.err != nil {
			t.Fatalf("shutdown failed: %v; logs=%v", p.err, p.entries())
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("shutdown did not complete: %v", p.entries())
	}
	stages := []string{"http_stopping", "http_stopped", "workers_stopped", "postgres_closed"}
	index := 0
	for _, entry := range p.entries() {
		if index < len(stages) && entry["msg"] == stages[index] {
			index++
		}
	}
	if index != len(stages) {
		t.Fatalf("shutdown order=%v", p.entries())
	}
}

func TestShutdownCompletesInFlightHTTPRequest(t *testing.T) {
	db := newDatabase(t, true)
	p := startServer(t, db)
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, p, internal, "100.00")
	lock, err := db.conn.Begin(db.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(db.ctx)
	if _, err := lock.Exec(db.ctx, "SELECT id FROM wallets WHERE id=$1 FOR UPDATE", w.id); err != nil {
		t.Fatal(err)
	}
	r := request(w, domain.Bet, "25.00")
	key := newID()
	type response struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan response, 1)
	go func() {
		status, body, err := call(p.URL, "POST", "/wagering/transactions", provider, key, r)
		done <- response{status, body, err}
	}()
	waitForWalletLock(t, db)
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, "HTTP draining", func() bool {
		for _, entry := range p.entries() {
			if entry["msg"] == "http_stopping" {
				return true
			}
		}
		return false
	})
	if err := lock.Rollback(db.ctx); err != nil {
		t.Fatal(err)
	}
	outcome := <-done
	if outcome.err != nil || outcome.status != 200 {
		t.Fatalf("in-flight request: %d %s %v", outcome.status, outcome.body, outcome.err)
	}
	waitForGracefulExit(t, p)
	db.assertBalance(t, w, 7500, 2)
	restarted := startServer(t, db)
	if !wager(t, restarted, provider, r, key, 200).IdempotentReplay {
		t.Fatal("committed request was not replayed after shutdown")
	}
}

func TestShutdownReleasesInFlightSQSMessage(t *testing.T) {
	q := queues(t)
	db := newDatabase(t, true)
	api := startServer(t, db)
	internal := token(t, "internal-service")
	w := openAPIWallet(t, api, internal, "100.00")
	consumer := startServer(t, db, "WORKER_ROLES=consumer", "SQS_VISIBILITY_TIMEOUT=30")
	lock, err := db.conn.Begin(db.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(db.ctx)
	if _, err := lock.Exec(db.ctx, "SELECT id FROM wallets WHERE id=$1 FOR UPDATE", w.id); err != nil {
		t.Fatal(err)
	}
	message := envelope(request(w, domain.Bet, "25.00"), newID())
	q.send(t, message)
	waitForWalletLock(t, db)
	if err := consumer.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitForGracefulExit(t, consumer)
	if err := lock.Rollback(db.ctx); err != nil {
		t.Fatal(err)
	}
	if db.count(t, "SELECT count(*) FROM inbox") != 0 {
		t.Fatal("canceled treatment committed an inbox row")
	}
	recovered := startServer(t, db, "WORKER_ROLES=consumer")
	eventually(t, 5*time.Second, "message visibility released on shutdown", func() bool {
		for _, entry := range recovered.entries() {
			if entry["msg"] == "sqs_completed" && entry["messageId"] == message.MessageID {
				return true
			}
		}
		return false
	})
	db.assertBalance(t, w, 7500, 2)
}

func TestShutdownRecoversUnconfirmedPublication(t *testing.T) {
	q := queues(t)
	db := newDatabase(t, true)
	api := startServer(t, db)
	internal := token(t, "internal-service")
	w := openAPIWallet(t, api, internal, "100.00")
	target, err := url.Parse(brokerEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	sent := make(chan string, 1)
	release := make(chan struct{})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".SendMessage") {
			forward.ServeHTTP(w, r)
			return
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		request := r.Clone(r.Context())
		request.RequestURI = ""
		request.URL.Scheme = target.Scheme
		request.URL.Host = target.Host
		request.Host = target.Host
		request.Body = io.NopCloser(bytes.NewReader(payload))
		response, err := transport.RoundTrip(request)
		if err != nil {
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return
		}
		var params struct{ MessageBody string }
		var event struct {
			EventID string `json:"eventId"`
		}
		_ = json.Unmarshal(payload, &params)
		_ = json.Unmarshal([]byte(params.MessageBody), &event)
		if response.StatusCode == 200 {
			select {
			case sent <- event.EventID:
			default:
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(body)
	}))
	defer proxy.Close()
	defer close(release)
	publisher := startServer(t, db, "WORKER_ROLES=publisher", "WORKER_CONCURRENCY=1", "SQS_ENDPOINT="+proxy.URL)
	var eventID string
	select {
	case eventID = <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not send event")
	}
	if eventID == "" {
		t.Fatal("event ID missing")
	}
	if err := publisher.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitForGracefulExit(t, publisher)
	if db.count(t, "SELECT count(*) FROM outbox WHERE event_id=$1 AND published_at IS NULL", eventID) != 1 {
		t.Fatal("interrupted publication was marked complete")
	}
	recovered := startServer(t, db, "WORKER_ROLES=publisher")
	eventually(t, 5*time.Second, "unconfirmed publication recovered", func() bool { return db.count(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL") == 0 })
	if db.count(t, "SELECT attempts FROM outbox WHERE event_id=$1", eventID) < 2 {
		t.Fatal("event was not resent")
	}
	recovered.stop(t)
	delivered := map[string]int{}
	eventually(t, 5*time.Second, "opening events delivered", func() bool {
		for _, message := range q.receive(t, q.events) {
			var body struct {
				EventID string `json:"eventId"`
			}
			_ = json.Unmarshal([]byte(*message.Body), &body)
			delivered[body.EventID]++
			q.delete(t, q.events, message)
		}
		return len(delivered) == 2
	})
	if delivered[eventID] != 1 {
		t.Fatalf("stable FIFO event identity lost: %v", delivered)
	}
	db.assertBalance(t, w, 10000, 1)
}

func TestPermanentReferenceFailureIsAudited(t *testing.T) {
	db := newDatabase(t, true)
	api := startServer(t, db)
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, api, internal, "100.00")
	bet := request(w, domain.Bet, "5.00")
	win := request(w, domain.Win, "10.00")
	win.ReferenceExternalTransactionID = bet.ExternalTransactionID
	key := newID()
	pending := wager(t, api, provider, win, key, 202)
	wager(t, api, provider, bet, newID(), 200)
	_, err := db.conn.Exec(db.ctx, `CREATE FUNCTION fail_test_reference_ledger() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'injected reference invariant failure' USING ERRCODE='23514'; END $$;
 CREATE TRIGGER fail_test_reference_ledger BEFORE INSERT ON wallet_ledger FOR EACH ROW EXECUTE FUNCTION fail_test_reference_ledger()`)
	if err != nil {
		t.Fatal(err)
	}
	worker := startServer(t, db, "WORKER_ROLES=references")
	eventually(t, 5*time.Second, "permanent reference failure audited", func() bool { return db.status(t, pending.TransactionID) == domain.Failed })
	worker.stop(t)
	if _, err := db.conn.Exec(db.ctx, "DROP TRIGGER fail_test_reference_ledger ON wallet_ledger"); err != nil {
		t.Fatal(err)
	}
	replay := wager(t, api, provider, win, key, 500)
	if !replay.IdempotentReplay || replay.Status != domain.Failed {
		t.Fatalf("reference failure reapplied: %+v", replay)
	}
	db.assertBalance(t, w, 9500, 2)
	if db.count(t, "SELECT count(*) FROM wallet_ledger WHERE transaction_id=$1", pending.TransactionID) != 0 {
		t.Fatal("failed reference left a movement")
	}
}

func TestPermanentFailureAtCommitIsAudited(t *testing.T) {
	db := newDatabase(t, true)
	api := startServer(t, db)
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, api, internal, "100.00")
	_, err := db.conn.Exec(db.ctx, `CREATE FUNCTION fail_test_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'injected deferred invariant failure' USING ERRCODE='23514'; END $$;
 CREATE CONSTRAINT TRIGGER fail_test_commit AFTER UPDATE ON wager_transactions
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.kind='BET' AND NEW.status='PROCESSED') EXECUTE FUNCTION fail_test_commit()`)
	if err != nil {
		t.Fatal(err)
	}
	r := request(w, domain.Bet, "25.00")
	key := newID()
	failed := wager(t, api, provider, r, key, 500)
	if failed.Status != domain.Failed {
		t.Fatalf("commit failure not audited: %+v", failed)
	}
	db.assertBalance(t, w, 10000, 1)
	if db.count(t, "SELECT count(*) FROM outbox") != 2 || db.count(t, "SELECT count(*) FROM wallet_ledger WHERE transaction_id=$1", failed.TransactionID) != 0 {
		t.Fatal("failed commit left ledger or outbox changes")
	}
	if _, err := db.conn.Exec(db.ctx, "DROP TRIGGER fail_test_commit ON wager_transactions"); err != nil {
		t.Fatal(err)
	}
	if !wager(t, api, provider, r, key, 500).IdempotentReplay {
		t.Fatal("failed commit was not terminal")
	}
}
