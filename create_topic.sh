#!/bin/bash
# Ensures the ingestion topic exists before MinIO tries to publish to it.
set -eu

: "${KAFKA_BROKER:?KAFKA_BROKER must be set}"
: "${KAFKA_TOPIC:?KAFKA_TOPIC must be set}"

echo "Waiting for Kafka to be ready..."
until /opt/kafka/bin/kafka-topics.sh --bootstrap-server "$KAFKA_BROKER" --list >/dev/null 2>&1; do
  echo '...waiting for kafka...'
  sleep 2
done

/opt/kafka/bin/kafka-topics.sh --create --if-not-exists \
  --topic "$KAFKA_TOPIC" \
  --replication-factor 1 \
  --partitions 1 \
  --bootstrap-server "$KAFKA_BROKER"

echo "Kafka topic '$KAFKA_TOPIC' ensured."
