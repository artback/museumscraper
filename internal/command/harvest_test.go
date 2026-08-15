package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExampleSourceFileIsValid keeps the worked example honest. It is the
// first thing an operator copies, and source definitions are decoded strictly,
// so a field renamed in extract.Source would leave the example silently
// unusable with nothing else to catch it.
func TestExampleSourceFileIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "harvest-source.json")

	source, err := readSourceFile(path)
	if err != nil {
		t.Fatalf("readSourceFile(%s) error = %v", path, err)
	}
	if err := source.Validate(); err != nil {
		t.Fatalf("the example source does not validate: %v", err)
	}

	if source.Every.Every() != 24*time.Hour {
		t.Errorf("example cadence = %s, want 24h", source.Every)
	}
	if len(source.Schema.Fields) == 0 {
		t.Error("the example declares no fields")
	}

	// The placeholder list is the part of a schema most worth demonstrating:
	// it is the operator naming the exact wrong answers a listing page invites.
	title, ok := source.Schema.Field("title")
	if !ok {
		t.Fatal("the example has no title field")
	}
	if len(title.Rules.Placeholders) == 0 {
		t.Error("the example's title field declares no placeholders, which is the rule worth showing")
	}
}

func TestReadSourceFileRejectsUnknownFields(t *testing.T) {
	// A misspelled rule is silently dropped by a lenient decoder, and the check
	// the operator thought they had written is simply absent from every run.
	const typo = `{"name":"a","url":"https://example.org",
	  "schema":{"name":"s","fields":[{"name":"title","kind":"string","requried":true}]}}`

	path := filepath.Join(t.TempDir(), "source.json")
	if err := os.WriteFile(path, []byte(typo), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := readSourceFile(path)
	if err == nil {
		t.Fatal("readSourceFile() accepted a misspelled field")
	}
	if !strings.Contains(err.Error(), "requried") {
		t.Errorf("readSourceFile() error = %v, want it to name the offending field", err)
	}
}

func TestHarvestBucketIsSeparate(t *testing.T) {
	// The enricher geocodes from bucket notifications without checking whether
	// a record has already been enriched, so harvest state must not land in
	// the bucket that carries the notification.
	t.Setenv("HARVEST_BUCKET_NAME", "")
	t.Setenv("MUSEUM_BUCKET_NAME", "museum")

	bucket, err := harvestBucket()
	if err != nil {
		t.Fatalf("harvestBucket() error = %v", err)
	}
	if bucket == "museum" {
		t.Error("harvestBucket() returned the catalogue's own bucket")
	}

	t.Setenv("HARVEST_BUCKET_NAME", "explicit")
	if bucket, err := harvestBucket(); err != nil || bucket != "explicit" {
		t.Errorf("harvestBucket() = %q, %v, want %q", bucket, err, "explicit")
	}
}
