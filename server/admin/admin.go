// Package admin exposes minimal health, status, and Prometheus-style metrics
// endpoints for operators.
package admin

import (
	"encoding/json"
	"fmt"
	"net/http"

	"pebbledb/sql/engine"
)

func Handler(database *engine.Engine) (http.Handler, error) {
	if database == nil {
		return nil, fmt.Errorf("admin: SQL engine is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, request *http.Request) {
		if err := database.Health(request.Context()); err != nil {
			http.Error(writer, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = writer.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/status", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(database.Diagnostics())
	})
	mux.HandleFunc("/metrics", func(writer http.ResponseWriter, _ *http.Request) {
		diagnostics := database.Diagnostics()
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintf(writer, "pebbledb_lsm_memtable_entries %d\n", diagnostics.Storage.MemtableEntries)
		_, _ = fmt.Fprintf(writer, "pebbledb_lsm_memtable_bytes %d\n", diagnostics.Storage.MemtableBytes)
		_, _ = fmt.Fprintf(writer, "pebbledb_lsm_sstables %d\n", diagnostics.Storage.SSTables)
		_, _ = fmt.Fprintf(writer, "pebbledb_lsm_cache_bytes %d\n", diagnostics.Storage.CacheBytes)
		_, _ = fmt.Fprintf(writer, "pebbledb_lsm_cache_hits_total %d\n", diagnostics.Storage.CacheHits)
		_, _ = fmt.Fprintf(writer, "pebbledb_lsm_cache_misses_total %d\n", diagnostics.Storage.CacheMisses)
		_, _ = fmt.Fprintf(writer, "pebbledb_lsm_bloom_rejections_total %d\n", diagnostics.Storage.BloomRejections)
		_, _ = fmt.Fprintf(writer, "pebbledb_lsm_background_compactions_total %d\n", diagnostics.Storage.BackgroundCompactions)
		_, _ = fmt.Fprintf(writer, "pebbledb_raft_term %d\n", diagnostics.Consensus.Term)
		_, _ = fmt.Fprintf(writer, "pebbledb_raft_commit_index %d\n", diagnostics.Consensus.CommitIndex)
		_, _ = fmt.Fprintf(writer, "pebbledb_ranges %d\n", len(diagnostics.Ranges))
	})
	return mux, nil
}
