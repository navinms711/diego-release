// download-droplets pre-warms a new cell's droplet cache by copying cached
// files from a same-AZ donor cell before the rep starts accepting work.  It is
// invoked as a background daemon from the BOSH pre-start script so the rep
// starts immediately with an empty cache; within 1-2 heartbeat cycles (~30-60s)
// the files are on disk and the rep advertises them via CachedDropletHashes.
package main

import (
	"archive/tar"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"code.cloudfoundry.org/bbs"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/lager/v3"
	"code.cloudfoundry.org/rep/cmd/rep/config"
)

var configFilePath = flag.String("config", "", "Path to the rep JSON config file.")

func main() {
	flag.Parse()

	if *configFilePath == "" {
		log.Fatal("--config is required")
	}

	repConfig, err := config.NewRepConfig(*configFilePath)
	if err != nil {
		log.Fatalf("failed to load rep config: %v", err)
	}

	logger := lager.NewLogger("download-droplets")
	logger.RegisterSink(lager.NewWriterSink(os.Stdout, lager.DEBUG))

	cachePath := repConfig.ExecutorConfig.CachePath
	if cachePath == "" {
		logger.Info("no-cache-path-configured-exiting")
		return
	}

	bbsClient, err := bbs.NewClientWithConfig(bbs.ClientConfig{
		URL:      repConfig.BBSAddress,
		IsTLS:    true,
		CAFile:   repConfig.CaCertFile,
		CertFile: repConfig.CertFile,
		KeyFile:  repConfig.KeyFile,
	})
	if err != nil {
		logger.Error("failed-to-create-bbs-client", err)
		os.Exit(1)
	}

	httpClient, err := newMTLSClient(repConfig.CaCertFile, repConfig.CertFile, repConfig.KeyFile)
	if err != nil {
		logger.Error("failed-to-create-http-client", err)
		os.Exit(1)
	}

	cells, err := bbsClient.Cells(logger, "")
	if err != nil {
		logger.Error("failed-to-list-cells", err)
		os.Exit(1)
	}

	donorURL := findDonor(logger, httpClient, cells, repConfig.Zone, repConfig.CellID)
	if donorURL == "" {
		logger.Info("no-eligible-donor-found")
		return
	}

	logger.Info("downloading-from-donor", lager.Data{"donor": donorURL})
	if err := downloadAndExtract(logger, httpClient, donorURL, cachePath); err != nil {
		logger.Error("download-failed", err, lager.Data{"donor": donorURL})
		os.Exit(1)
	}
	logger.Info("download-complete")
}

// findDonor calls /state on each same-zone cell (excluding self) and returns
// the RepUrl of the first one that has a non-empty CachedDropletHashes list.
func findDonor(
	logger lager.Logger,
	httpClient *http.Client,
	cells []*models.CellPresence,
	myZone, myID string,
) string {
	for _, cell := range cells {
		if cell.CellId == myID {
			continue
		}
		if cell.Zone != myZone {
			continue
		}
		repURL := cell.RepUrl
		if repURL == "" {
			continue
		}

		hashes, err := fetchCachedDropletHashes(httpClient, repURL)
		if err != nil {
			logger.Info("skipping-candidate", lager.Data{
				"cell_id": cell.CellId,
				"reason":  err.Error(),
			})
			continue
		}
		if len(hashes) > 0 {
			logger.Info("found-donor", lager.Data{
				"cell_id":    cell.CellId,
				"hash_count": len(hashes),
			})
			return repURL
		}
	}
	return ""
}

// fetchCachedDropletHashes calls GET {repURL}/state and returns
// CachedDropletHashes from the JSON response.
func fetchCachedDropletHashes(httpClient *http.Client, repURL string) ([]string, error) {
	url := repURL + "/state"
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("cell at %s is unhealthy", repURL)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d", url, resp.StatusCode)
	}

	var state struct {
		CachedDropletHashes []string `json:"cached_droplet_hashes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, fmt.Errorf("decode state from %s: %w", repURL, err)
	}
	return state.CachedDropletHashes, nil
}

// downloadAndExtract calls GET {repURL}/serve_droplets, receives a tar stream,
// and writes each file atomically into cachePath.
func downloadAndExtract(logger lager.Logger, httpClient *http.Client, repURL, cachePath string) error {
	url := repURL + "/serve_droplets"
	resp, err := httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		logger.Info("donor-has-empty-cache")
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d", url, resp.StatusCode)
	}

	tr := tar.NewReader(resp.Body)
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}

		destPath := filepath.Join(cachePath, filepath.Base(hdr.Name))

		// skip if already present (idempotent re-runs)
		if _, err := os.Lstat(destPath); err == nil {
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return fmt.Errorf("draining tar entry %s: %w", hdr.Name, err)
			}
			continue
		}

		tmpPath := destPath + ".download-tmp"
		if err := writeAtomically(tmpPath, destPath, tr); err != nil {
			logger.Error("failed-to-write-file", err, lager.Data{"file": hdr.Name})
			continue
		}
		count++
	}
	logger.Info("extracted-files", lager.Data{"count": count})
	return nil
}

// writeAtomically writes r to tmpPath then renames to destPath so the rep's
// heartbeat scanner never sees a partial file.
func writeAtomically(tmpPath, destPath string, r io.Reader) error {
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, destPath)
}

func newMTLSClient(caFile, certFile, keyFile string) (*http.Client, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("failed to parse CA cert from %s", caFile)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12,
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		Timeout:   10 * time.Minute,
	}, nil
}
