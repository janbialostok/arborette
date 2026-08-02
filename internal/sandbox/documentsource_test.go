package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/datasource"
)

// minimalPDF builds a valid single-page PDF whose content stream draws text, with
// a correct byte-offset xref table so the reader can resolve every object. The
// text is a single token so extraction spacing does not affect the assertion.
func minimalPDF(t *testing.T, text string) []byte {
	t.Helper()
	stream := "BT /F1 24 Tf 72 700 Td (" + text + ") Tj ET\n"
	objs := []string{
		"<</Type/Catalog/Pages 2 0 R>>",
		"<</Type/Pages/Kids[3 0 R]/Count 1>>",
		"<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>",
		"<</Length " + strconv.Itoa(len(stream)) + ">>\nstream\n" + stream + "endstream",
		"<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>",
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, body := range objs {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xrefOff := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xrefOff)
	return buf.Bytes()
}

func TestPDFPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.pdf")
	if err := os.WriteFile(path, minimalPDF(t, "EffectiveDateMarker"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	pages, err := pdfPages(path)
	if err != nil {
		t.Fatalf("pdfPages: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want 1", len(pages))
	}
	if !strings.Contains(pages[0], "EffectiveDateMarker") {
		t.Fatalf("page text missing the drawn token: %q", pages[0])
	}
}

func TestPDFPagesMalformed(t *testing.T) {
	// A .pdf-suffixed file whose bytes are not a valid PDF must surface a parse
	// error (mapped to a 400 by writeStageErr), not panic or return empty text.
	path := filepath.Join(t.TempDir(), "corrupt.pdf")
	if err := os.WriteFile(path, []byte("this is not a pdf at all"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := pdfPages(path); err == nil {
		t.Fatal("expected a parse error for a malformed PDF")
	}
}

func TestDocumentSample(t *testing.T) {
	if s := documentSample(nil); s != "" {
		t.Fatalf("empty pages should sample to empty, got %q", s)
	}
	long := strings.Repeat("x", documentSampleMaxRunes+500)
	if s := documentSample([]string{long, "second"}); len(s) != documentSampleMaxRunes {
		t.Fatalf("sample len = %d, want %d (first page, capped)", len(s), documentSampleMaxRunes)
	}
}

func TestDocumentSourceUnsupportedExtension(t *testing.T) {
	// The extension check precedes any download, so this needs no object store.
	src := NewDocumentSource(nil, nil, "notes.txt", 1<<20)
	if _, err := src.Pages(context.Background()); !errors.Is(err, errUnsupportedDocument) {
		t.Fatalf("error = %v, want errUnsupportedDocument", err)
	}
}

// TestDocumentSourceIntegration stages a PDF in MinIO and exercises the reader and
// the /document/text route end to end.
func TestDocumentSourceIntegration(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	key := putObject(t, ctx, client, ".pdf", minimalPDF(t, "IntegrationMarker"))

	src := NewDocumentSource(client, nil, key, 512<<20)
	if src.Kind() != datasource.KindDocument {
		t.Fatalf("kind = %q, want document", src.Kind())
	}
	pages, err := src.Pages(ctx)
	if err != nil {
		t.Fatalf("pages: %v", err)
	}
	if len(pages) != 1 || !strings.Contains(pages[0], "IntegrationMarker") {
		t.Fatalf("unexpected pages: %q", pages)
	}

	// A tiny object cap rejects the staged PDF before it parses (the over-limit
	// staging bound), rather than truncating it to wrong text.
	if _, err := NewDocumentSource(client, nil, key, 10).Pages(ctx); !errors.Is(err, errObjectTooLarge) {
		t.Fatalf("over-limit error = %v, want errObjectTooLarge", err)
	}

	// The /document/text route returns the same per-page text.
	srv := NewServer(client, nil, nil, testLimiter(), 512<<20, 1<<20, "1GiB", 50)
	body, _ := json.Marshal(DocumentTextRequest{DataSourceRef: key})
	req := httptest.NewRequest(http.MethodPost, "/document/text", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var resp DocumentTextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Pages) != 1 || !strings.Contains(resp.Pages[0], "IntegrationMarker") {
		t.Fatalf("route pages mismatch: %q", resp.Pages)
	}

	// Introspect over the same PDF reports the document kind plus a sample.
	introReq, _ := json.Marshal(IntrospectRequest{DataSourceRef: key})
	ireq := httptest.NewRequest(http.MethodPost, "/introspect", bytes.NewReader(introReq))
	irec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(irec, ireq)
	if irec.Code != http.StatusOK {
		t.Fatalf("introspect status = %d (body %q)", irec.Code, irec.Body.String())
	}
	var intro IntrospectResponse
	if err := json.Unmarshal(irec.Body.Bytes(), &intro); err != nil {
		t.Fatalf("decode introspect: %v", err)
	}
	if intro.Schema.Kind != string(datasource.KindDocument) || !strings.Contains(intro.Sample, "IntegrationMarker") {
		t.Fatalf("introspect did not report a document with a sample: %+v", intro)
	}
}
