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
// FileSource's primitive-arg constructor, but carries no DuckDB engine: a document
// has no tabular schema, only ordered per-page plain text that both the intake
// sample and provenance search read. Like FileSource it is stateless in semantics;
// the optional cache is a pure performance layer over the immutable ref (a PDF
// caches its raw staged bytes, no conversion). A nil cache stages per-request.
type DocumentSource struct {
	objects        *objectstore.Client
	cache          *StageCache
	dataSourceRef  string
	maxObjectBytes int64
}

// NewDocumentSource builds a DocumentSource for one object-store ref. cache is
// optional (nil ⇒ per-request staging). maxObjectBytes caps the staged copy.
func NewDocumentSource(objects *objectstore.Client, cache *StageCache, dataSourceRef string, maxObjectBytes int64) *DocumentSource {
	return &DocumentSource{objects: objects, cache: cache, dataSourceRef: dataSourceRef, maxObjectBytes: maxObjectBytes}
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

	stagedPath, release, err := s.stageDocument(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer release()
	return pdfPages(stagedPath)
}

// stageDocument resolves the staged PDF path from the cache when configured, else a
// fresh per-request download into dir. The returned release is always non-nil (a
// no-op for the per-request and bypass paths) so the caller defers it uniformly.
// The cached artifact is the raw PDF bytes -- pdf.Open reads by content, so the
// extension-less cache path parses identically to a .pdf-suffixed temp file.
func (s *DocumentSource) stageDocument(ctx context.Context, dir string) (string, func(), error) {
	// perRequest stages the raw PDF into the request's own temp dir -- the fallback
	// when the cache is disabled or bypasses.
	perRequest := func() (string, func(), error) {
		staged := filepath.Join(dir, "source.pdf")
		if err := stageBoundedObject(ctx, s.objects, s.dataSourceRef, staged, s.maxObjectBytes); err != nil {
			return "", nil, err
		}
		return staged, func() {}, nil
	}

	if s.cache == nil {
		return perRequest()
	}

	fill := func(ctx context.Context, _, dest string) error {
		return stageBoundedObject(ctx, s.objects, s.dataSourceRef, dest, s.maxObjectBytes)
	}
	cachePath, release, err := s.cache.Acquire(ctx, s.dataSourceRef, s.maxObjectBytes, fill)
	if errors.Is(err, errCacheBypass) {
		return perRequest()
	}
	if err != nil {
		return "", nil, err
	}
	return cachePath, release, nil
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
