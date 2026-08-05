// Command gen renders the bundled ground-truth dataset to a CSV file. It is the
// go:generate-style entry point that produces local-import/causal-demo.csv from
// the deterministic generator, so the committed fixture is reproducible from
// source rather than hand-authored.
package main

import (
	"encoding/csv"
	"flag"
	"log"
	"os"

	"github.com/arborette/arborette/internal/verifier/groundtruth"
)

func main() {
	out := flag.String("out", "local-import/causal-demo.csv", "path to write the generated CSV to")
	flag.Parse()

	ds := groundtruth.Generate(groundtruth.DefaultConfig())

	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("gen: create %s: %v", *out, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.WriteAll(ds.Records()); err != nil {
		log.Fatalf("gen: write csv: %v", err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		log.Fatalf("gen: flush csv: %v", err)
	}
	log.Printf("gen: wrote %d rows to %s", len(ds.Rows), *out)
}
