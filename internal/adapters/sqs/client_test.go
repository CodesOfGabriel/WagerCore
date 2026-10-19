package sqsbroker

import (
	"context"
	"testing"

	"go.uber.org/fx/fxtest"

	"junglegaming/internal/platform/config"
)

func TestProductionPreservesAWSSessionCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "session-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "session-secret")
	t.Setenv("AWS_SESSION_TOKEN", "session-token")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	cfg := config.Defaults()
	cfg.Environment = "production"
	cfg.SQSEndpoint = ""
	cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey = "session-access", "session-secret"
	broker, err := NewBroker(fxtest.NewLifecycle(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := broker.Client.Options().Credentials.Retrieve(context.Background())
	if err != nil || credentials.AccessKeyID != "session-access" || credentials.SecretAccessKey != "session-secret" || credentials.SessionToken != "session-token" {
		t.Fatal("AWS SDK credential chain did not preserve the session credentials")
	}
}
