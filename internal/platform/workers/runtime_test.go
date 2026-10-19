package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	broker "junglegaming/internal/adapters/sqs"
	"junglegaming/internal/platform/config"
	"junglegaming/internal/platform/telemetry"
)

func TestInvalidMessageLogsDecodeAndVisibilityFailure(t *testing.T) {
	operations := make(chan string, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operations <- r.Header.Get("X-Amz-Target")
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.Header().Set("X-Amzn-Requestid", "request-1")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"__type":"AccessDenied","message":"private-broker-credentials"}`)
	}))
	defer server.Close()
	client := sqs.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", "")}, func(o *sqs.Options) { o.BaseEndpoint = aws.String(server.URL); o.RetryMaxAttempts = 1 })
	var logs bytes.Buffer
	runtime := &Runtime{cfg: config.Defaults(), broker: &broker.Broker{Client: client, RequestURL: server.URL + "/queue"}, metrics: telemetry.New(), logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	runtime.consume(context.Background(), types.Message{MessageId: aws.String("transport-1"), Body: aws.String(`{"private-token":`), ReceiptHandle: aws.String("private-receipt"), Attributes: map[string]string{"ApproximateReceiveCount": "2"}})
	logged := logs.String()
	decoder := json.NewDecoder(strings.NewReader(logged))
	var retry, visibility map[string]any
	if err := decoder.Decode(&retry); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&visibility); err != nil {
		t.Fatal(err)
	}
	if retry["msg"] != "sqs_message_retry" || retry["stage"] != "decode" || retry["errorCategory"] != "invalid_input" || retry["sqsMessageId"] != "transport-1" || retry["receiveCount"] != float64(2) || retry["correlationId"] == "" || retry["retryDelaySeconds"] == nil {
		t.Fatalf("retry=%v", retry)
	}
	if visibility["msg"] != "sqs_visibility_failed" || visibility["stage"] != "change_visibility" || visibility["errorCategory"] != "broker" || visibility["sdkOperation"] != "ChangeMessageVisibility" || visibility["errorCode"] != "AccessDenied" || visibility["requestId"] != "request-1" || visibility["correlationId"] != retry["correlationId"] {
		t.Fatalf("visibility=%v", visibility)
	}
	if len(operations) != 1 || !strings.HasSuffix(<-operations, ".ChangeMessageVisibility") {
		t.Fatal("unexpected SQS operation")
	}
	// A mensagem inválida não deve alcançar processamento financeiro nem DeleteMessage.
	if strings.Contains(logged, "private-") {
		t.Fatalf("sensitive details leaked: %s", logs.String())
	}
}

func TestDLQStatsPollIntervalAndShutdown(t *testing.T) {
	calls := make(chan time.Time, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".GetQueueAttributes") {
			t.Error("unexpected broker operation")
		}
		calls <- time.Now()
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = io.WriteString(w, `{"Attributes":{"ApproximateNumberOfMessages":"7"}}`)
	}))
	defer server.Close()
	client := sqs.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", "")}, func(o *sqs.Options) {
		o.BaseEndpoint = aws.String(server.URL)
		o.RetryMaxAttempts = 1
	})
	cfg := config.Defaults()
	cfg.DLQPollInterval = 100 * time.Millisecond
	runtime := &Runtime{cfg: cfg, broker: &broker.Broker{Client: client, DLQURL: server.URL + "/dlq"}, metrics: telemetry.New(), logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); runtime.dlqStats(ctx) }()
	var first time.Time
	select {
	case first = <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("initial DLQ sample missing")
	}
	select {
	case second := <-calls:
		if second.Sub(first) < cfg.DLQPollInterval {
			t.Fatal("DLQ polled before configured interval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled DLQ sample missing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DLQ monitoring did not stop on cancellation")
	}
	response := httptest.NewRecorder()
	runtime.metrics.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(response.Body.String(), "wager_dlq_messages 7\n") {
		t.Fatal("DLQ metric was not updated")
	}
}
