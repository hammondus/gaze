// Command gaze-devserver is a throwaway ingest endpoint: it logs what an
// agent posts and answers with a directive, to prove the reporting path
// before gaze-server exists. Delete it when stage 4 lands.
//
// It accepts any bearer token, which is exactly why it must never be more
// than a development tool.
package main

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"

	"github.com/hammondus/gaze/internal/report"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9090", "listen `address`")
	gen := flag.Int("gen", 0, "configuration `generation` to send back; 0 sends no directive")
	sampleS := flag.Int("sample", 0, "sample interval in `seconds` to direct, 0 to leave alone")
	reportS := flag.Int("report", 0, "report interval in `seconds` to direct, 0 to leave alone")
	flag.Parse()

	http.HandleFunc("POST /api/v1/reports", func(w http.ResponseWriter, r *http.Request) {
		body := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer zr.Close()
			body = zr
		}
		var reports []report.Report
		if err := json.NewDecoder(body).Decode(&reports); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		for _, rep := range reports {
			log.Printf("report: host=%s schema=%d gen=%d agent=%s span=%s..%s samples=%d cpu(mean)=%.1f%% top=%d containers=%d",
				rep.Host.Hostname, rep.Schema, rep.Generation, rep.Version,
				rep.Start.Format("15:04:05"), rep.End.Format("15:04:05"),
				rep.Samples, rep.CPU.Mean, len(rep.Top), len(rep.Containers))
		}

		if *gen == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(report.Directive{
			Generation:    *gen,
			SampleSeconds: *sampleS,
			ReportSeconds: *reportS,
		})
	})

	log.Printf("gaze-devserver listening on %s (throwaway: accepts any token)", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
