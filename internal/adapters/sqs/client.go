package sqsbroker

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"junglegaming/internal/platform/config"
)

type Broker struct {
	Client                       *sqs.Client
	RequestURL, EventURL, DLQURL string
	visibility                   int32
}

func NewBroker(lifecycle fx.Lifecycle, cfg config.Config) (*Broker, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Transport: transport, Timeout: 25 * time.Second}
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.AWSRegion), awsconfig.WithHTTPClient(client), awsconfig.WithRetryMaxAttempts(2)}
	if cfg.AWSAccessKeyID != "" {
		options = append(options, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey, "")))
	}
	awsConfig, err := awsconfig.LoadDefaultConfig(context.Background(), options...)
	if err != nil {
		return nil, errors.New("invalid AWS configuration")
	}
	broker := &Broker{Client: sqs.NewFromConfig(awsConfig, func(options *sqs.Options) {
		if cfg.SQSEndpoint != "" {
			options.BaseEndpoint = aws.String(cfg.SQSEndpoint)
		}
	}), visibility: cfg.VisibilityTimeout}
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			for name, destination := range map[string]*string{"wager-transactions.fifo": &broker.RequestURL, "integration-events.fifo": &broker.EventURL, "wager-transactions-dlq.fifo": &broker.DLQURL} {
				response, err := broker.Client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
				if err != nil {
					transport.CloseIdleConnections()
					return errors.New("SQS queues are unavailable")
				}
				*destination = aws.ToString(response.QueueUrl)
			}
			return broker.Ready(ctx)
		},
		OnStop: func(context.Context) error { transport.CloseIdleConnections(); return nil },
	})
	return broker, nil
}

func (b *Broker) Ready(ctx context.Context) error {
	for _, url := range []string{b.RequestURL, b.EventURL, b.DLQURL} {
		if _, err := b.Client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(url), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}}); err != nil {
			return err
		}
	}
	return nil
}

func (b *Broker) Receive(ctx context.Context, limit int) ([]types.Message, error) {
	if limit > 10 {
		limit = 10
	}
	response, err := b.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(b.RequestURL), MaxNumberOfMessages: int32(limit), WaitTimeSeconds: 20, VisibilityTimeout: b.visibility,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
	})
	if err != nil {
		return nil, err
	}
	return response.Messages, nil
}

func (b *Broker) Delete(ctx context.Context, receipt *string) error {
	_, err := b.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(b.RequestURL), ReceiptHandle: receipt})
	return err
}
func (b *Broker) Visibility(ctx context.Context, receipt *string, seconds int32) error {
	_, err := b.Client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(b.RequestURL), ReceiptHandle: receipt, VisibilityTimeout: seconds})
	return err
}
func (b *Broker) Publish(ctx context.Context, eventID, walletID string, payload []byte) error {
	_, err := b.Client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(b.EventURL), MessageBody: aws.String(string(payload)), MessageGroupId: aws.String(walletID), MessageDeduplicationId: aws.String(eventID)})
	return err
}
func (b *Broker) DLQSize(ctx context.Context) (int64, error) {
	response, err := b.Client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(b.DLQURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages}})
	if err != nil {
		return 0, err
	}
	var size int64
	for _, digit := range response.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)] {
		if digit >= '0' && digit <= '9' {
			size = size*10 + int64(digit-'0')
		}
	}
	return size, nil
}
