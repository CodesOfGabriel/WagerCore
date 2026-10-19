package workers

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	sqsbroker "junglegaming/internal/adapters/sqs"
	"junglegaming/internal/application"
	"junglegaming/internal/domain"
	"junglegaming/internal/identifier"
	"junglegaming/internal/platform/config"
	"junglegaming/internal/platform/telemetry"
)

func (r *Runtime) consumer(ctx context.Context) {
	for ctx.Err() == nil {
		messages, err := r.broker.Receive(ctx, r.cfg.WorkerConcurrency)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.metrics.Retry("sqs")
			r.logger.Warn("sqs_receive_failed", append([]any{"stage", "receive"}, telemetry.ErrorFields(err)...)...)
			if !pause(ctx, backoff(r.cfg.RetryBase, 1)) {
				return
			}
			continue
		}
		var group sync.WaitGroup
		for _, message := range messages {
			group.Add(1)
			go func(message types.Message) { defer group.Done(); r.consume(ctx, message) }(message)
		}
		group.Wait()
	}
}

func (r *Runtime) consume(root context.Context, message types.Message) {
	ctx, cancel := context.WithTimeout(root, config.ConsumerTimeout)
	defer cancel()
	count, _ := strconv.Atoi(message.Attributes["ApproximateReceiveCount"])
	stage := "create_correlation"
	correlation, err := identifier.New()
	var request sqsbroker.Requested
	var result application.WagerResult
	attrs := []any{"sqsMessageId", aws.ToString(message.MessageId), "receiveCount", count, "correlationId", correlation}
	if err == nil {
		stage = "decode"
		var hash []byte
		request, hash, err = sqsbroker.Decode(aws.ToString(message.Body))
		if err == nil {
			attrs = append(attrs, "messageId", request.MessageID, "walletId", request.Data.WalletID, "providerId", request.Data.ProviderID, "externalTransactionId", request.Data.ExternalTransactionID)
			r.logger.Info("sqs_received", attrs...)
			stage = "process"
			result, err = r.service.Process(ctx, request.Data.WagerRequest, request.Data.IdempotencyKey, application.Trace{CorrelationID: correlation, CausationID: request.MessageID}, &application.IncomingMessage{Consumer: "wager-transactions", ID: request.MessageID, Hash: hash})
			if err == nil || result.Status == domain.Failed {
				attrs = append(attrs, "transactionId", result.TransactionID, "status", result.Status, "idempotentReplay", result.IdempotentReplay)
				r.logger.Info("sqs_processed", attrs...)
				if result.Status == domain.Failed && err == nil {
					err = application.ErrPermanentInfrastructure
				}
			}
		}
	}
	if err == nil {
		r.faults.Hit("after_sqs_commit", request.Data.ExternalTransactionID)
		stage = "acknowledge"
		err = r.broker.Delete(ctx, message.ReceiptHandle)
	}
	if err == nil {
		r.logger.Info("sqs_completed", attrs...)
		return
	}
	r.metrics.Retry("sqs")
	delay := int32((backoff(r.cfg.RetryBase, count) + time.Second - 1) / time.Second)
	if root.Err() != nil {
		delay = 0
	}
	failure := append(append([]any{}, attrs...), "stage", stage, "retryDelaySeconds", delay)
	r.logger.Warn("sqs_message_retry", append(failure, telemetry.ErrorFields(err)...)...)
	cleanup, stop := context.WithTimeout(context.Background(), config.CleanupTimeout)
	defer stop()
	if err := r.broker.Visibility(cleanup, message.ReceiptHandle, delay); err != nil {
		r.logger.Warn("sqs_visibility_failed", append(append(attrs, "stage", "change_visibility"), telemetry.ErrorFields(err)...)...)
	}
}
