package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/releases"
)

func main() {
	outputPath := flag.String("output", "", "write the official three-release catalog to this path")
	tsvOutputPath := flag.String("tsv-output", "", "write portable tab-separated release rows to this path")
	inputPath := flag.String("input", "", "read an existing catalog instead of fetching GitHub Releases")
	format := flag.String("format", "json", "output format: json or tsv")
	selfTest := flag.Bool("self-test", false, "verify that the helper can start, then exit")
	flag.Parse()
	if *selfTest {
		return
	}

	var catalog releases.Catalog
	var err error
	if *inputPath != "" {
		catalog, err = releases.LoadCatalog(*inputPath)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		catalog, err = releases.NewClient(&http.Client{Timeout: 10 * time.Second}, time.Hour).List(ctx, 3)
	}
	if err != nil {
		log.Fatal(err)
	}
	portableCatalog := releases.PortableCatalog(catalog)

	if *outputPath != "" {
		encoded, encodeErr := json.MarshalIndent(portableCatalog, "", "  ")
		if encodeErr != nil {
			log.Fatal(encodeErr)
		}
		encoded = append(encoded, '\n')
		if writeErr := os.WriteFile(*outputPath, encoded, 0o600); writeErr != nil {
			log.Fatal(writeErr)
		}
	}
	if *tsvOutputPath != "" {
		file, openErr := os.OpenFile(*tsvOutputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if openErr != nil {
			log.Fatal(openErr)
		}
		for _, release := range portableCatalog.Releases {
			if _, writeErr := fmt.Fprintf(file, "%s\t%s\t%s\t%s\n", release.Version, release.Commit, release.Image, release.URL); writeErr != nil {
				_ = file.Close()
				log.Fatal(writeErr)
			}
		}
		if closeErr := file.Close(); closeErr != nil {
			log.Fatal(closeErr)
		}
	}

	switch *format {
	case "json":
		if *outputPath == "" && *tsvOutputPath == "" {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(portableCatalog); err != nil {
				log.Fatal(err)
			}
		}
	case "tsv":
		for _, release := range portableCatalog.Releases {
			fmt.Printf("%s\t%s\t%s\t%s\n", release.Version, release.Commit, release.Image, release.URL)
		}
	default:
		log.Fatalf("unsupported format %q", *format)
	}
}
