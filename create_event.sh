mc alias set local http://minio:9000 minioadmin minioadmin ;
mc admin config set local/museum notify_kafka:1 enable=on brokers=kafka:9092 topic=raw.museum.ingestion.events.v1 ;
mc event add local/museum arn:minio:sqs::1:kafka --event put --prefix raw_data
