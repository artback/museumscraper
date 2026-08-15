module museum

go 1.25.0

require (
	github.com/artback/museumscraper/extract v0.0.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/joho/godotenv v1.5.1
	github.com/minio/minio-go/v7 v7.0.95
	github.com/segmentio/kafka-go v0.4.49
	golang.org/x/net v0.44.0
	golang.org/x/text v0.29.0
)

require (
	github.com/andybalholm/cascadia v1.3.3 // indirect
	github.com/dlclark/regexp2/v2 v2.5.2 // indirect
	github.com/dop251/goja v0.0.0-20260806115107-493f22071ef6 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-ini/ini v1.67.0 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/goccy/go-json v0.10.5 // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/tinylib/msgp v1.4.0 // indirect
	golang.org/x/crypto v0.42.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.36.0 // indirect
)

// The extraction harness is its own module, usable without the catalogue.
// Replaced rather than version-pinned because it lives in this repository.
replace github.com/artback/museumscraper/extract => ./extract
