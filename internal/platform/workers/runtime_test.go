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
