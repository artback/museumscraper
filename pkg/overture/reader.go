// Package overture reads museums from the Overture Maps places theme.
//
// Overture is the only open source in this catalogue with a genuinely even
// footprint. Wikidata holds 1,201 museums for the whole of Africa and the
// national registers exist only where a government happens to publish one, so
// every other source is, in part, a measurement of who writes things down.
// Overture is assembled from commercial POI data contributed by its members and
// covers Lagos the way it covers Lyon.
//
// The cost is that it is published as 10.5 GB of Parquet, which is not
// something to download onto a Raspberry Pi once a month. It does not have to
// be: Parquet is columnar, and of the forty-odd columns a museum record needs
// six. Reading only those column chunks, addressed by byte range over HTTP,
// brings a pass over the whole planet to roughly 2 GB — and the release listing
// makes it cheap to know there is nothing new to read at all.
package overture

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"
)

const (
	// coalesceGap is how far apart two wanted column chunks may be before it is
	// worth issuing separate requests for them.
	//
	// Measured against one release file's metadata, per file:
	//
	//	gap        fetched   requests
	//	exact       203 MB      2,816
	//	16 KB       203 MB      1,280
	//	64 KB       226 MB        785
	//	128 KB      227 MB        768
	//	1 MB        452 MB        259
	//
	// 16 KB is the knee for bytes — it merges the chunks that are already
	// adjacent and fetches nothing extra — but the last stretch to 128 KB
	// trades 24 MB for 512 fewer round trips, and at a round trip to us-west-2
	// per request that is the better side of the trade on any connection this
	// runs on. Past 128 KB the gap starts swallowing whole columns we do not
	// want, and 1 MB doubles the transfer for nothing.
	coalesceGap = 128 << 10

	// maxSegments bounds what a reader holds at once. Chunks are fetched per
	// row group and a row group's worth of the columns we want is under a
	// megabyte, so this is generous.
	maxSegments = 32
)

// chunkReaderAt serves parquet's reads out of byte ranges fetched over HTTP.
//
// The access pattern is what makes this worth writing rather than wrapping a
// plain buffer. parquet-go reads pages column by column within a row group, so
// a single read-ahead window thrashes — measured at 380 MB per file against the
// 131 MB the columns actually occupy. Prefetching precisely the chunks a row
// group needs, coalescing the ones that sit near each other, reads what is
// wanted and little else.
type chunkReaderAt struct {
	ctx    context.Context
	client *http.Client
	url    string
	agent  string
	size   int64

	segments []segment

	// read and requests are counters for the crawl's log, which is the only
	// place the cost of this source is visible.
	read     atomic.Int64
	requests atomic.Int64
}

// segment is a byte range held in memory.
type segment struct {
	off  int64
	data []byte
}

// prefetch fetches the given ranges, replacing whatever was held.
func (r *chunkReaderAt) prefetch(ranges []byteRange) error {
	r.segments = r.segments[:0]

	for _, want := range coalesce(ranges) {
		data, err := r.fetch(want.off, want.length)
		if err != nil {
			return err
		}
		r.segments = append(r.segments, segment{off: want.off, data: data})
		if len(r.segments) > maxSegments {
			r.segments = r.segments[1:]
		}
	}
	return nil
}

// ReadAt serves from a prefetched segment, falling back to a direct range
// request for anything outside them — the footer, chiefly.
func (r *chunkReaderAt) ReadAt(p []byte, off int64) (int, error) {
	for _, s := range r.segments {
		if off >= s.off && off+int64(len(p)) <= s.off+int64(len(s.data)) {
			return copy(p, s.data[off-s.off:]), nil
		}
	}

	length := int64(len(p))
	if off+length > r.size {
		length = r.size - off
	}
	if length <= 0 {
		return 0, io.EOF
	}

	data, err := r.fetch(off, length)
	if err != nil {
		return 0, err
	}
	n := copy(p, data)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// fetch performs one range request.
func (r *chunkReaderAt) fetch(off, length int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", r.agent)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+length-1))

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", r.url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %s", r.url, resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, length))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", r.url, err)
	}

	r.read.Add(int64(len(data)))
	r.requests.Add(1)
	return data, nil
}

// byteRange is a span of a file.
type byteRange struct {
	off, length int64
}

// coalesce merges ranges that sit close enough together that one request for
// both beats two requests.
func coalesce(ranges []byteRange) []byteRange {
	if len(ranges) == 0 {
		return nil
	}

	sorted := slices.Clone(ranges)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].off < sorted[j].off })

	merged := []byteRange{sorted[0]}
	for _, next := range sorted[1:] {
		last := &merged[len(merged)-1]
		end := last.off + last.length
		if next.off <= end+coalesceGap {
			if newEnd := next.off + next.length; newEnd > end {
				last.length = newEnd - last.off
			}
			continue
		}
		merged = append(merged, next)
	}
	return merged
}

// wantedRanges returns the byte ranges of the columns this package reads, for
// one row group.
func wantedRanges(group format.RowGroup, columns map[string]bool) []byteRange {
	var ranges []byteRange
	for _, column := range group.Columns {
		if !columns[strings.Join(column.MetaData.PathInSchema, ".")] {
			continue
		}

		off := column.MetaData.DataPageOffset
		if d := column.MetaData.DictionaryPageOffset; d > 0 && d < off {
			off = d
		}
		ranges = append(ranges, byteRange{off: off, length: column.MetaData.TotalCompressedSize})
	}
	return ranges
}

// leafColumns are the parquet leaf paths a museum record is built from. Named
// explicitly because the projection is the entire reason this is affordable:
// these are about a fifth of the file, and the rest — the other source
// identifiers, the phone numbers, the socials, the full geometry — is never
// transferred.
func leafColumns(schema *parquet.Schema) map[string]bool {
	wanted := map[string]bool{}
	for _, path := range schema.Columns() {
		joined := strings.Join(path, ".")
		for _, prefix := range []string{
			"categories.primary",
			"categories.alternate",
			"names.primary",
			"bbox.xmin",
			"bbox.ymin",
			"confidence",
			"websites",
			"addresses",
		} {
			if joined == prefix || strings.HasPrefix(joined, prefix+".") {
				wanted[joined] = true
			}
		}
	}
	return wanted
}
