package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ledongthuc/pdf"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/objectstore"
)

// errUnsupportedDocument is the document analog of errUnsupportedFormat: a ref
// whose extension this reader does not own (only .pdf today).
var errUnsupportedDocument = errors.New("unsupported document format")

// documentSampleMaxRunes bounds the plain-text sample the intake path derives
// from a document's first page, so a large document does not inflate the
// introspection response body.
const documentSampleMaxRunes = 4000

// DocumentSource reads a single document object (a PDF today) out of the object
// store and answers introspection over its extracted per-page text. It mirrors
// FileSource's primitive-arg constructor and stateless-per-request staging, but
// carries no DuckDB engine: a document has no tabular schema, only ordered
// per-page plain text that both the intake sample and provenance search read.
type DocumentSource struct {
	objects        *objectstore.Client
	dataSourceRef  string
	maxObjectBytes int64
}

// NewDocumentSource builds a DocumentSource for one object-store ref.
// maxObjectBytes caps the staged copy.
func NewDocumentSource(objects *objectstore.Client, dataSourceRef string, maxObjectBytes int64) *DocumentSource {
	return &DocumentSource{objects: objects, dataSourceRef: dataSourceRef, maxObjectBytes: maxObjectBytes}
}

var _ datasource.DataSource = (*DocumentSource)(nil)

// Kind reports the source shape. This reader handles only documents.
func (s *DocumentSource) Kind() datasource.SourceKind { return datasource.KindDocument }

// Introspect stages the document and validates it parses as a precondition,
// returning a document schema. Fields stays empty: candidate extractable fields
// are LLM-derived in the orchestrator (the sandbox holds no Claude client), so
// the sandbox only confirms the document is a supported, readable format.
func (s *DocumentSource) Introspect(ctx context.Context) (*datasource.Schema, error) {
	if _, err := s.Pages(ctx); err != nil {
		return nil, err
	}
	return &datasource.Schema{Kind: datasource.KindDocument}, nil
}

// Pages stages the document into an isolated temp file and returns its ordered
// per-page plain text (page index -> text). This is the deterministic substrate
// for both the intake sample and provenance; it runs no LLM. A ref this reader
// does not own returns the unsupported sentinel.
func (s *DocumentSource) Pages(ctx context.Context) ([]string, error) {
	if !isDocumentRef(s.dataSourceRef) {
		return nil, fmt.Errorf("%w: %q", errUnsupportedDocument, s.dataSourceRef)
	}

	dir, err := os.MkdirTemp("", "arborette-document-")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(dir)

	stagedPath := filepath.Join(dir, "source.pdf")
	if err := stageBoundedObject(ctx, s.objects, s.dataSourceRef, stagedPath, s.maxObjectBytes); err != nil {
		return nil, err
	}
	return pdfPages(stagedPath)
}

// pdfPages reads a staged PDF into ordered per-page plain text. Fonts are cached
// across pages (mirroring the library's own whole-document pass) so a shared
// charmap is parsed once. Page.GetPlainText recovers internally from a malformed
// content stream and returns an error, so a bad page fails the read rather than
// panicking.
func pdfPages(path string) ([]string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open pdf: %w", err)
	}
	defer f.Close()

	pages := make([]string, 0, r.NumPage())
	fonts := make(map[string]*pdf.Font)
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		for _, name := range p.Fonts() {
			if _, ok := fonts[name]; !ok {
				fnt := p.Font(name)
				fonts[name] = &fnt
			}
		}
		text, err := p.GetPlainText(fonts)
		if err != nil {
			return nil, fmt.Errorf("extract page %d text: %w", i, err)
		}
		pages = append(pages, text)
	}
	return pages, nil
}

// documentSample derives a bounded plain-text sample from the first page, so the
// orchestrator's field-introspection LLM call has grounding without the intake
// response carrying the whole document. The cut is rune-safe.
func documentSample(pages []string) string {
	if len(pages) == 0 {
		return ""
	}
	runes := []rune(pages[0])
	if len(runes) > documentSampleMaxRunes {
		runes = runes[:documentSampleMaxRunes]
	}
	return string(runes)
}
