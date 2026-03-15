package scraper

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const maxPDFSize = 10 * 1024 * 1024 // 10 MB

// PDF stream patterns
var (
	// Match stream...endstream blocks
	streamPattern = regexp.MustCompile(`(?s)stream\r?\n(.*?)endstream`)

	// Text extraction operators: Tj, TJ, ' , "
	textTjPattern = regexp.MustCompile(`\(([^)]*)\)\s*Tj`)
	textTJPattern = regexp.MustCompile(`\(([^)]*)\)`)

	// BT...ET text blocks
	textBlockPattern = regexp.MustCompile(`(?s)BT\s(.*?)ET`)

	// FlateDecode filter indicator
	flateDecode = regexp.MustCompile(`/Filter\s*/FlateDecode`)

	// Stream length and filter from the object dictionary preceding a stream
	objDictPattern = regexp.MustCompile(`(?s)<<(.*?)>>\s*stream`)
)

// PDFText holds extracted text from a PDF document.
type PDFText struct {
	Text     string
	NumPages int
}

// FetchAndExtractPDF downloads a PDF and extracts its text content.
func (f *Fetcher) FetchAndExtractPDF(ctx context.Context, pdfURL string) (*PDFText, error) {
	if pdfURL == "" {
		return nil, fmt.Errorf("empty PDF URL")
	}

	// Ensure HTTPS
	if strings.HasPrefix(pdfURL, "http://") {
		pdfURL = "https://" + pdfURL[7:]
	}
	if !strings.HasPrefix(pdfURL, "https://") {
		pdfURL = "https://" + pdfURL
	}

	// Rate limit
	select {
	case <-f.limiter.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	req, err := http.NewRequestWithContext(ctx, "GET", pdfURL, nil)
	if err != nil {
		return nil, fmt.Errorf("pdf request %s: %w", pdfURL, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/pdf")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pdf fetch %s: %w", pdfURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("pdf fetch %s: HTTP %d", pdfURL, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPDFSize))
	if err != nil {
		return nil, fmt.Errorf("pdf read %s: %w", pdfURL, err)
	}

	return ExtractPDFText(data)
}

// ExtractPDFText extracts readable text from raw PDF bytes.
// It handles both uncompressed and FlateDecode-compressed streams.
func ExtractPDFText(data []byte) (*PDFText, error) {
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		return nil, fmt.Errorf("not a valid PDF")
	}

	var allText strings.Builder
	numPages := bytes.Count(data, []byte("/Type /Page"))

	// Find all stream blocks and their preceding dictionaries
	content := string(data)

	// Find object dictionaries and their associated streams
	dictMatches := objDictPattern.FindAllStringSubmatchIndex(content, -1)
	streamMatches := streamPattern.FindAllStringSubmatchIndex(content, -1)

	// Build a set of stream offsets that are FlateDecode compressed
	flateStreams := make(map[int]bool)
	for _, dm := range dictMatches {
		dict := content[dm[2]:dm[3]]
		if flateDecode.MatchString(dict) {
			// Find the stream that starts right after this dict
			streamStart := dm[1] // end of ">>\nstream"
			for _, sm := range streamMatches {
				if sm[0] >= dm[0] && sm[0] <= streamStart+5 {
					flateStreams[sm[0]] = true
					break
				}
			}
		}
	}

	for _, sm := range streamMatches {
		streamData := content[sm[2]:sm[3]]
		isFlate := flateStreams[sm[0]]

		var textContent string
		if isFlate {
			decompressed, err := decompressZlib([]byte(streamData))
			if err != nil {
				continue // Skip streams we can't decompress
			}
			textContent = string(decompressed)
		} else {
			textContent = streamData
		}

		// Extract text from BT...ET blocks
		extracted := extractTextFromContent(textContent)
		if extracted != "" {
			allText.WriteString(extracted)
			allText.WriteString("\n")
		}
	}

	text := strings.TrimSpace(allText.String())
	if text == "" {
		return nil, fmt.Errorf("no extractable text found in PDF")
	}

	return &PDFText{
		Text:     text,
		NumPages: numPages,
	}, nil
}

// extractTextFromContent pulls text from PDF content stream operators.
func extractTextFromContent(content string) string {
	var result strings.Builder

	// Find BT...ET text blocks
	blocks := textBlockPattern.FindAllStringSubmatch(content, -1)
	for _, block := range blocks {
		blockContent := block[1]

		// Extract Tj strings: (text) Tj
		tjMatches := textTjPattern.FindAllStringSubmatch(blockContent, -1)
		for _, m := range tjMatches {
			text := decodePDFString(m[1])
			if text != "" {
				result.WriteString(text)
				result.WriteString(" ")
			}
		}

		// Extract TJ arrays: [(text1) 100 (text2)] TJ
		if strings.Contains(blockContent, "TJ") {
			// Find array content between [ and ] TJ
			tjArrayPattern := regexp.MustCompile(`\[(.*?)\]\s*TJ`)
			arrayMatches := tjArrayPattern.FindAllStringSubmatch(blockContent, -1)
			for _, am := range arrayMatches {
				strMatches := textTJPattern.FindAllStringSubmatch(am[1], -1)
				for _, sm := range strMatches {
					text := decodePDFString(sm[1])
					if text != "" {
						result.WriteString(text)
					}
				}
				result.WriteString(" ")
			}
		}

		// Handle ' and " operators (text showing with line movement)
		lines := strings.Split(blockContent, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasSuffix(line, "'") || strings.HasSuffix(line, "\"") {
				// Extract parenthesized string before the operator
				pMatches := textTJPattern.FindAllStringSubmatch(line, -1)
				for _, pm := range pMatches {
					text := decodePDFString(pm[1])
					if text != "" {
						result.WriteString(text)
						result.WriteString(" ")
					}
				}
			}
		}
	}

	return strings.TrimSpace(result.String())
}

// decodePDFString handles basic PDF string escape sequences.
func decodePDFString(s string) string {
	s = strings.ReplaceAll(s, "\\n", "\n")
	s = strings.ReplaceAll(s, "\\r", "\r")
	s = strings.ReplaceAll(s, "\\t", "\t")
	s = strings.ReplaceAll(s, "\\(", "(")
	s = strings.ReplaceAll(s, "\\)", ")")
	s = strings.ReplaceAll(s, "\\\\", "\\")

	// Filter out non-printable characters and binary data
	var b strings.Builder
	printable := 0
	total := 0
	for _, r := range s {
		total++
		if r >= 32 && r < 127 || r == '\n' || r == '\r' || r == '\t' || r >= 160 {
			b.WriteRune(r)
			printable++
		}
	}

	// If less than half the characters are printable, it's likely binary data
	if total > 0 && float64(printable)/float64(total) < 0.5 {
		return ""
	}

	return b.String()
}

// decompressZlib decompresses zlib/FlateDecode data.
func decompressZlib(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(r, 5*1024*1024)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

