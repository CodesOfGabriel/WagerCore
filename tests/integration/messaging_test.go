//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	broker "junglegaming/internal/adapters/sqs"
	"junglegaming/internal/application"
	"junglegaming/internal/domain"
)

type testBroker struct {
	client             *sqs.Client
	input, events, dlq string
}

func queues(t *testing.T) *testBroker {
	t.Helper()
	q := &testBroker{client: sqs.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", "")}, func(o *sqs.Options) { o.BaseEndpoint = aws.String(brokerEndpoint()); o.RetryMaxAttempts = 2 })}
	for name, dest := range map[string]*string{"wager-transactions.fifo": &q.input, "integration-events.fifo": &q.events, "wager-transactions-dlq.fifo": &q.dlq} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := q.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		*dest = aws.ToString(result.QueueUrl)
	}
	q.drain(t, q.input)
	q.drain(t, q.dlq)
	q.drain(t, q.events)
	return q
}
func (q *testBroker) receive(t *testing.T, url string) []types.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	result, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(url), MaxNumberOfMessages: 10, WaitTimeSeconds: 1, VisibilityTimeout: 1, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll}})
	if err != nil {
		t.Fatal(err)
	}
	return result.Messages
}
func (q *testBroker) delete(t *testing.T, url string, message types.Message) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: message.ReceiptHandle}); err != nil {
		t.Fatal(err)
	}
}
func (q *testBroker) drain(t *testing.T, url string) {
	t.Helper()
	for range 100 {
		messages := q.receive(t, url)
		if len(messages) == 0 {
			return
		}
		for _, m := range messages {
			q.delete(t, url, m)
		}
	}
	t.Fatal("queue did not drain")
}
func (q *testBroker) send(t *testing.T, message broker.Requested) {
	t.Helper()
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = q.client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(q.input), MessageBody: aws.String(string(payload)), MessageGroupId: aws.String(message.Data.WalletID), MessageDeduplicationId: aws.String(newID())})
	if err != nil {
		t.Fatal(err)
	}
}
func envelope(r application.WagerRequest, key string) broker.Requested {
	return broker.Requested{MessageID: newID(), Type: "WagerTransactionRequested", OccurredAt: time.Now().UTC(), Data: broker.RequestedData{WagerRequest: r, IdempotencyKey: key}}
}
func (db *database) status(t *testing.T, id string) domain.TransactionStatus {
	t.Helper()
	var status domain.TransactionStatus
	if err := db.conn.QueryRow(db.ctx, "SELECT status FROM wager_transactions WHERE id=$1", id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestSQSInboxAndCrossChannelIdempotency(t *testing.T) {
	q := queues(t)
	db := newDatabase(t, true)
	servers := []*serverProcess{startServer(t, db, "WORKER_ROLES=consumer"), startServer(t, db, "WORKER_ROLES=consumer"), startServer(t, db, "WORKER_ROLES=consumer")}
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, servers[0], internal, "100.00")
	r := request(w, domain.Bet, "10.00")
	key := newID()
	m := envelope(r, key)
	httpDone := make(chan error, 1)
	go func() {
		status, data, err := call(servers[1].URL, "POST", "/wagering/transactions", provider, key, r)
		if err == nil && status != 200 {
			err = fmt.Errorf("status=%d body=%s", status, data)
		}
		httpDone <- err
	}()
	q.send(t, m)
	q.send(t, m)
	other := m
	other.MessageID = newID()
	q.send(t, other)
	if err := <-httpDone; err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "two completed inbox identities", func() bool { return db.count(t, "SELECT count(*) FROM inbox WHERE completed_at IS NOT NULL") == 2 })
	db.assertBalance(t, w, 9000, 2)
	if db.count(t, "SELECT count(*) FROM wager_transactions WHERE kind='BET'") != 1 || db.count(t, "SELECT count(*) FROM outbox") != 4 {
		t.Fatal("HTTP/SQS duplicate applied more than once")
	}
	changed := m
	changed.Data.ExternalTransactionID = newID()
	changed.Data.Money = money("1.00")
	q.send(t, changed)
	opening := envelope(request(w, domain.Opening, "1.00"), newID())
	q.send(t, opening)
	dlq := map[string]bool{}
	eventually(t, 25*time.Second, "invalid OPENING and inbox hash conflict reach DLQ", func() bool {
		for _, message := range q.receive(t, q.dlq) {
			var body broker.Requested
			if json.Unmarshal([]byte(aws.ToString(message.Body)), &body) == nil {
				dlq[body.MessageID] = true
			}
			q.delete(t, q.dlq, message)
		}
		return dlq[m.MessageID] && dlq[opening.MessageID]
	})
	db.assertBalance(t, w, 9000, 2)
	if db.count(t, "SELECT count(*) FROM inbox") != 2 {
		t.Fatal("invalid messages left committed inbox rows")
	}
	for _, p := range servers {
		p.stop(t)
	}
}

func TestReferenceWorkerResumesAfterRestart(t *testing.T) {
	db := newDatabase(t, true)
	p := startServer(t, db)
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, p, internal, "100.00")
	bet := request(w, domain.Bet, "25.00")
	refund := request(w, domain.Refund, "25.00")
	refund.ReferenceExternalTransactionID = bet.ExternalTransactionID
	pending := wager(t, p, provider, refund, newID(), 202)
	p.stop(t)
	worker := startServer(t, db, "WORKER_ROLES=references")
	wager(t, worker, provider, bet, newID(), 200)
	eventually(t, 5*time.Second, "pending reversal resumed", func() bool { return db.status(t, pending.TransactionID) == domain.Processed })
	db.assertBalance(t, w, 10000, 3)
	if db.count(t, "SELECT count(*) FROM outbox WHERE event_type='WagerTransactionPendingReference'") != 1 || db.count(t, "SELECT count(*) FROM wallet_ledger WHERE transaction_id=$1", pending.TransactionID) != 1 {
		t.Fatal("pending recovery duplicated events or movement")
	}
	worker.stop(t)
	worker = startServer(t, db, "WORKER_ROLES=references", "REFERENCE_TTL=250ms", "REFERENCE_MAX_ATTEMPTS=3")
	missing := request(w, domain.Refund, "1.00")
	missing.ReferenceExternalTransactionID = "never-arrives-" + newID()
	expired := wager(t, worker, provider, missing, newID(), 202)
	eventually(t, 5*time.Second, "missing reference expired", func() bool { return db.status(t, expired.TransactionID) == domain.Rejected })
	var failure string
	if err := db.conn.QueryRow(db.ctx, "SELECT failure_code FROM wager_transactions WHERE id=$1", expired.TransactionID).Scan(&failure); err != nil || failure != "REFERENCE_NOT_FOUND" {
		t.Fatalf("failure=%s err=%v", failure, err)
	}
	db.assertBalance(t, w, 10000, 3)
}

func TestCrashAfterFinancialCommitReplaysOriginalResult(t *testing.T) {
	db := newDatabase(t, true)
	healthy := startServer(t, db)
	standby := startServer(t, db)
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, healthy, internal, "100.00")
	r := request(w, domain.Bet, "25.00")
	key := newID()
	crashing := startServer(t, db, "FAULT_POINT=after_financial_commit", "FAULT_TRANSACTION="+r.ExternalTransactionID)
	if status, _, err := call(crashing.URL, "POST", "/wagering/transactions", provider, key, r); err == nil {
		t.Fatalf("request unexpectedly completed: %d", status)
	}
	crashing.crashed(t)
	wager(t, healthy, provider, request(w, domain.Bet, "10.00"), newID(), 200)
	replay := wager(t, standby, provider, r, key, 200)
	if !replay.IdempotentReplay || replay.Balance.MinorUnits() != 7500 {
		t.Fatalf("commit result not recovered: %+v", replay)
	}
	db.assertBalance(t, w, 6500, 3)
	if db.count(t, "SELECT count(*) FROM wallet_ledger WHERE transaction_id=$1", replay.TransactionID) != 1 {
		t.Fatal("recovery duplicated ledger")
	}
}

func TestCrashAfterSQSCommitRedeliversWithoutMovement(t *testing.T) {
	q := queues(t)
	db := newDatabase(t, true)
	api := startServer(t, db)
	internal := token(t, "internal-service")
	w := openAPIWallet(t, api, internal, "100.00")
	r := request(w, domain.Bet, "25.00")
	m := envelope(r, newID())
	crashing := startServer(t, db, "WORKER_ROLES=consumer", "FAULT_POINT=after_sqs_commit", "FAULT_TRANSACTION="+r.ExternalTransactionID)
	q.send(t, m)
	crashing.crashed(t)
	recovered := startServer(t, db, "WORKER_ROLES=consumer")
	eventually(t, 10*time.Second, "SQS redelivery after committed inbox", func() bool {
		for _, entry := range recovered.entries() {
			if entry["msg"] == "sqs_received" && entry["messageId"] == m.MessageID && entry["receiveCount"].(float64) >= 2 {
				return true
			}
		}
		return false
	})
	eventually(t, 5*time.Second, "replay confirmed", func() bool {
		for _, entry := range recovered.entries() {
			if entry["msg"] == "sqs_completed" && entry["messageId"] == m.MessageID {
				return true
			}
		}
		return false
	})
	db.assertBalance(t, w, 7500, 2)
	if db.count(t, "SELECT count(*) FROM inbox") != 1 || db.count(t, "SELECT count(*) FROM outbox") != 4 {
		t.Fatal("redelivery created duplicate data")
	}
	recovered.stop(t)
}

func TestCrashAfterEventPublicationUsesStableSnapshot(t *testing.T) {
	q := queues(t)
	db := newDatabase(t, true)
	api := startServer(t, db)
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, api, internal, "100.00")
	r := request(w, domain.Bet, "25.00")
	wager(t, api, provider, r, newID(), 200)
	crashing := startServer(t, db, "WORKER_ROLES=publisher", "WORKER_CONCURRENCY=1", "FAULT_POINT=after_event_publish", "FAULT_TRANSACTION="+r.ExternalTransactionID)
	crashing.crashed(t)
	var eventID string
	var original []byte
	if err := db.conn.QueryRow(db.ctx, "SELECT event_id::text,payload FROM outbox WHERE published_at IS NULL AND lease_id IS NOT NULL LIMIT 1").Scan(&eventID, &original); err != nil {
		t.Fatal(err)
	}
	wager(t, api, provider, request(w, domain.Bet, "10.00"), newID(), 200)
	publishers := []*serverProcess{startServer(t, db, "WORKER_ROLES=publisher"), startServer(t, db, "WORKER_ROLES=publisher")}
	eventually(t, 10*time.Second, "outbox recovered by another publisher", func() bool { return db.count(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL") == 0 })
	var current []byte
	var attempts int
	if err := db.conn.QueryRow(db.ctx, "SELECT payload,attempts FROM outbox WHERE event_id=$1", eventID).Scan(&current, &attempts); err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original) || attempts < 2 {
		t.Fatalf("snapshot changed or event was not retried: attempts=%d", attempts)
	}
	sentAgain := false
	for _, p := range publishers {
		for _, entry := range p.entries() {
			if entry["msg"] == "outbox_sent" && entry["eventId"] == eventID {
				sentAgain = true
			}
		}
	}
	if !sentAgain {
		t.Fatal("second publisher did not resend the original event ID")
	}
	delivered := map[string]int{}
	eventually(t, 10*time.Second, "all event snapshots delivered", func() bool {
		for _, m := range q.receive(t, q.events) {
			var body struct {
				EventID string `json:"eventId"`
			}
			if json.Unmarshal([]byte(aws.ToString(m.Body)), &body) == nil {
				delivered[body.EventID]++
			}
			q.delete(t, q.events, m)
		}
		return len(delivered) == 6
	})
	if delivered[eventID] != 1 {
		t.Fatalf("FIFO deduplication for retried event: %v", delivered)
	}
	db.assertBalance(t, w, 6500, 3)
}
