package handlers

import (
	"archive/tar"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"code.cloudfoundry.org/lager/v3"
	"code.cloudfoundry.org/locket/metrics/helpers"
)

type serveDroplets struct {
	cachePath string
	metrics   helpers.RequestMetrics
}

func newServeDropletsHandler(cachePath string, metrics helpers.RequestMetrics) *serveDroplets {
	return &serveDroplets{cachePath: cachePath, metrics: metrics}
}

// ServeHTTP streams all files from the cell's download cache as a tar archive.
// The caller (download-droplets on a newly-started cell) extracts the archive
// into its own cache directory so the rep's heartbeat can advertise the hashes
// to the auctioneer before any OLD[N+1] drain begins.
func (h *serveDroplets) ServeHTTP(w http.ResponseWriter, r *http.Request, logger lager.Logger) {
	var deferErr error

	start := time.Now()
	requestType := "ServeDroplets"
	startMetrics(h.metrics, requestType)
	defer stopMetrics(h.metrics, requestType, start, &deferErr)

	logger = logger.Session("serve-droplets").WithTraceInfo(r)

	if h.cachePath == "" {
		logger.Info("no-cache-path-configured")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	entries, err := os.ReadDir(h.cachePath)
	if err != nil {
		logger.Error("failed-to-read-cache-dir", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	tw := tar.NewWriter(w)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip the cache index (recipient must build its own) and any partial
		// download temporaries — neither is usable by a different cell.
		if name == "saved_cache.json" || strings.HasSuffix(name, ".download-tmp") {
			continue
		}
		fullPath := filepath.Join(h.cachePath, name)

		info, err := entry.Info()
		if err != nil {
			logger.Error("failed-to-stat-cache-file", err, lager.Data{"file": name})
			continue
		}

		f, err := os.Open(fullPath)
		if err != nil {
			logger.Error("failed-to-open-cache-file", err, lager.Data{"file": name})
			continue
		}

		hdr := &tar.Header{
			Name:    name,
			Size:    info.Size(),
			Mode:    int64(info.Mode()),
			ModTime: info.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			f.Close()
			logger.Error("failed-to-write-tar-header", err, lager.Data{"file": name})
			break
		}
		if _, err := io.Copy(tw, f); err != nil {
			f.Close()
			logger.Error("failed-to-write-tar-body", err, lager.Data{"file": name})
			break
		}
		f.Close()
	}

	// #nosec G104 - best-effort flush; client detects truncation via tar EOF
	tw.Close()
}
