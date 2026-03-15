package scraper

import (
	"testing"
)

func TestExtractPDFText_InvalidPDF(t *testing.T) {
	_, err := ExtractPDFText([]byte("not a PDF"))
	if err == nil {
		t.Error("expected error for non-PDF data")
	}
}

func TestExtractPDFText_ValidPDF(t *testing.T) {
	// Minimal valid PDF with uncompressed text stream
	pdf := buildMinimalPDF("Hello Museum World")

	result, err := ExtractPDFText(pdf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Text == "" {
		t.Error("expected non-empty text")
	}
}

func TestExtractPDFText_PriceExtraction(t *testing.T) {
	pdf := buildMinimalPDF("Admission Adults $25 Students $15")

	result, err := ExtractPDFText(pdf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The text should be extractable
	data := extractFromPlainText(result.Text)
	if data == nil {
		t.Fatal("expected extractable price data")
	}
	if len(data.Offers) < 2 {
		t.Errorf("expected at least 2 offers from PDF text, got %d", len(data.Offers))
	}
}

func TestExtractPDFText_HoursExtraction(t *testing.T) {
	pdf := buildMinimalPDF("Opening Hours Monday-Friday: 10:00-17:00")

	result, err := ExtractPDFText(pdf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data := extractFromPlainText(result.Text)
	if data == nil {
		t.Fatal("expected extractable hours data")
	}
	if data.OpeningHours == "" {
		t.Error("expected opening hours from PDF text")
	}
}

func TestDecodePDFString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello World", "Hello World"},
		{"Price \\(USD\\)", "Price (USD)"},
		{"Line1\\nLine2", "Line1\nLine2"},
		{"Back\\\\slash", "Back\\slash"},
	}

	for _, tt := range tests {
		got := decodePDFString(tt.input)
		if got != tt.want {
			t.Errorf("decodePDFString(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDecodePDFString_BinaryData(t *testing.T) {
	// Binary data should return empty string
	binary := string([]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08})
	result := decodePDFString(binary)
	if result != "" {
		t.Errorf("expected empty string for binary data, got %q", result)
	}
}

// buildMinimalPDF creates a minimal valid PDF containing the given text.
// This produces a PDF with a single page and uncompressed text stream.
func buildMinimalPDF(text string) []byte {
	// This is a minimal valid PDF structure with an uncompressed content stream
	stream := "BT /F1 12 Tf 100 700 Td (" + text + ") Tj ET"
	streamLen := len(stream)

	pdf := "%PDF-1.4\n" +
		"1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj\n" +
		"2 0 obj << /Type /Pages /Kids [3 0 R] /Count 1 >> endobj\n" +
		"3 0 obj << /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >> endobj\n" +
		"4 0 obj << /Length " + itoa(streamLen) + " >>\nstream\n" + stream + "\nendstream\nendobj\n" +
		"5 0 obj << /Type /Font /Subtype /Type1 /BaseFont /Helvetica >> endobj\n" +
		"%%EOF"

	return []byte(pdf)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
