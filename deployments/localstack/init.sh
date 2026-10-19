#!/bin/sh
set -eu

dlq_url=$(awslocal sqs create-queue --queue-name wager-transactions-dlq.fifo --attributes '{"FifoQueue":"true"}' --query QueueUrl --output text)
dlq_arn=$(awslocal sqs get-queue-attributes --queue-url "$dlq_url" --attribute-names QueueArn --query Attributes.QueueArn --output text)
attributes=$(printf '{"FifoQueue":"true","VisibilityTimeout":"30","ReceiveMessageWaitTimeSeconds":"20","RedrivePolicy":"{\"deadLetterTargetArn\":\"%s\",\"maxReceiveCount\":\"5\"}"}' "$dlq_arn")
request_url=$(awslocal sqs create-queue --queue-name wager-transactions.fifo --attributes "$attributes" --query QueueUrl --output text)
events_url=$(awslocal sqs create-queue --queue-name integration-events.fifo --attributes '{"FifoQueue":"true","ReceiveMessageWaitTimeSeconds":"20"}' --query QueueUrl --output text)

for queue_url in "$dlq_url" "$request_url" "$events_url"; do
    queue_arn=$(awslocal sqs get-queue-attributes --queue-url "$queue_url" --attribute-names QueueArn --query Attributes.QueueArn --output text)
    policy=$(printf '{"Policy":"{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"arn:aws:iam::000000000000:root\"},\"Action\":\"sqs:*\",\"Resource\":\"%s\"}]}"}' "$queue_arn")
    awslocal sqs set-queue-attributes --queue-url "$queue_url" --attributes "$policy"
done
