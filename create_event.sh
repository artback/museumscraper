#!/bin/sh
# Configures the MinIO bucket notification that feeds the enricher.
#
# The Kafka notification *target* itself is configured on the minio service via
# the MINIO_NOTIFY_KAFKA_* environment variables in docker-compose.yml, which is
# why this script only has to create the bucket and attach the event rule.
set -eu

: "${MUSEUM_BUCKET_NAME:?MUSEUM_BUCKET_NAME must be set}"
: "${MINIO_ROOT_USER:?MINIO_ROOT_USER must be set}"
: "${MINIO_ROOT_PASSWORD:?MINIO_ROOT_PASSWORD must be set}"

echo "Waiting for MinIO to be ready..."
until mc alias set local "http://minio:9000" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null 2>&1; do
  echo '...waiting for minio...'
  sleep 2
done

mc mb --ignore-existing "local/$MUSEUM_BUCKET_NAME"

# The prefix filter matters: the enricher writes its output back to the same
# bucket under enriched_data/, and without this restriction those writes would
# publish new events and feed the enricher its own output in a loop.
if mc event add "local/$MUSEUM_BUCKET_NAME" arn:minio:sqs::1:kafka \
     --event put --prefix "raw_data/" 2>/dev/null; then
  echo "Bucket notification created for local/$MUSEUM_BUCKET_NAME (prefix raw_data/)."
else
  echo "Bucket notification already present for local/$MUSEUM_BUCKET_NAME."
fi

mc event ls "local/$MUSEUM_BUCKET_NAME"
