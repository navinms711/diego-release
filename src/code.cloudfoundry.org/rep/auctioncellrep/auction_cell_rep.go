package auctioncellrep

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/executor"
	"code.cloudfoundry.org/lager/v3"
	"code.cloudfoundry.org/rep"
	"code.cloudfoundry.org/rep/evacuation/evacuation_context"
)

//go:generate counterfeiter . AuctionCellClient

type AuctionCellClient interface {
	State(logger lager.Logger) (models.CellState, bool, error)
	Perform(logger lager.Logger, traceID string, work models.Work) (models.Work, error)
	Reset() error
}

var ErrCellUnhealthy = errors.New("internal cell healthcheck failed")
var ErrCellIdMismatch = errors.New("workload cell ID does not match this cell")
var ErrNotEnoughMemory = errors.New("not enough memory for container and additional memory allocation")

type AuctionCellRep struct {
	cellID                   string
	cellIndex                int
	repURL                   string
	stackPathMap             rep.StackPathMap
	rootFSProviders          models.RootFSProviders
	containerMetricsProvider rep.ContainerMetricsProvider
	zone                     string
	client                   executor.Client
	evacuationReporter       evacuation_context.EvacuationReporter
	placementTags            []string
	optionalPlacementTags    []string
	enableContainerProxy     bool
	proxyMemoryAllocation    int
	allocator                BatchContainerAllocator
	startTime                time.Time
	cachePath                string
}

func New(
	cellID string,
	cellIndex int,
	repURL string,
	preloadedStackPathMap rep.StackPathMap,
	containerMetricsProvider rep.ContainerMetricsProvider,
	arbitraryRootFSes []string,
	zone string,
	client executor.Client,
	evacuationReporter evacuation_context.EvacuationReporter,
	placementTags []string,
	optionalPlacementTags []string,
	proxyMemoryAllocation int,
	enableContainerProxy bool,
	allocator BatchContainerAllocator,
	cachePath string,
) *AuctionCellRep {
	return &AuctionCellRep{
		cellID:                   cellID,
		cellIndex:                cellIndex,
		repURL:                   repURL,
		stackPathMap:             preloadedStackPathMap,
		rootFSProviders:          rootFSProviders(preloadedStackPathMap, arbitraryRootFSes),
		containerMetricsProvider: containerMetricsProvider,
		zone:                     zone,
		client:                   client,
		evacuationReporter:       evacuationReporter,
		placementTags:            placementTags,
		optionalPlacementTags:    optionalPlacementTags,
		enableContainerProxy:     enableContainerProxy,
		proxyMemoryAllocation:    proxyMemoryAllocation,
		allocator:                allocator,
		startTime:                time.Now(),
		cachePath:                cachePath,
	}
}

func rootFSProviders(preloaded rep.StackPathMap, arbitrary []string) models.RootFSProviders {
	rootFSProviders := models.RootFSProviders{}
	for _, scheme := range arbitrary {
		rootFSProviders[scheme] = models.ArbitraryRootFSProvider{}
	}

	stacks := make([]string, 0, len(preloaded))
	for stack := range preloaded {
		stacks = append(stacks, stack)
	}
	rootFSProviders[models.PreloadedRootFSScheme] = models.NewFixedSetRootFSProvider(stacks...)
	rootFSProviders[models.PreloadedOCIRootFSScheme] = models.NewFixedSetRootFSProvider(stacks...)

	return rootFSProviders
}

func rootFSURLFromPath(rootfsPath string, stackPathMap rep.StackPathMap) string {
	url, err := url.Parse(rootfsPath)
	if err != nil {
		return rootfsPath
	}

	for k, v := range stackPathMap {
		if rootfsPath == v {
			return fmt.Sprintf("%s:%s", models.PreloadedRootFSScheme, k)
		} else if url.Path == v {
			return fmt.Sprintf("%s:%s?%s", models.PreloadedOCIRootFSScheme, k, url.RawQuery)
		}
	}
	return rootfsPath
}

func (a *AuctionCellRep) State(logger lager.Logger) (models.CellState, bool, error) {
	logger = logger.Session("auction-state")
	logger.Info("providing")

	containers, err := a.client.ListContainers(logger)
	if err != nil {
		logger.Error("failed-to-fetch-containers", err)
		return models.CellState{}, false, err
	}

	totalResources, err := a.client.TotalResources(logger)
	if err != nil {
		logger.Error("failed-to-get-total-resources", err)
		return models.CellState{}, false, err
	}

	availableResources, err := a.client.RemainingResources(logger)
	if err != nil {
		logger.Error("failed-to-get-remaining-resource", err)
		return models.CellState{}, false, err
	}

	volumeDrivers, err := a.client.VolumeDrivers(logger)
	if err != nil {
		logger.Error("failed-to-get-volume-drivers", err)
		return models.CellState{}, false, err
	}

	lrps := []models.SchedulingLRP{}
	tasks := []models.SchedulingTask{}
	startingContainerCount := 0

	for i := range containers {
		container := &containers[i]

		if containerIsStarting(container) {
			startingContainerCount++
		}

		if container.Tags == nil {
			logger.Error("failed-to-extract-container-tags", nil)
			continue
		}

		placementTagsJSON := container.Tags[rep.PlacementTagsTag]
		var placementTags []string
		err := json.Unmarshal([]byte(placementTagsJSON), &placementTags)
		if err != nil {
			logger.Error("cannot-unmarshal-placement-tags", err, lager.Data{"placement-tags": placementTagsJSON})
		}

		volumeDriversJSON := container.Tags[rep.VolumeDriversTag]
		var volumeDrivers []string
		err = json.Unmarshal([]byte(volumeDriversJSON), &volumeDrivers)
		if err != nil {
			logger.Error("cannot-unmarshal-volume-drivers", err, lager.Data{"volume-drivers": volumeDriversJSON})
		}

		resource := models.Resource{MemoryMB: int32(container.MemoryMB), DiskMB: int32(container.DiskMB), MaxPids: int32(container.MaxPids)}
		placementConstraint := models.PlacementConstraint{
			RootFs:        rootFSURLFromPath(container.RootFSPath, a.stackPathMap),
			VolumeDrivers: volumeDrivers,
			PlacementTags: placementTags,
		}

		switch container.Tags[rep.LifecycleTag] {
		case rep.LRPLifecycle:
			key, err := rep.ActualLRPKeyFromTags(container.Tags)
			if err != nil {
				logger.Error("failed-to-extract-key", err)
				continue
			}
			instanceKey, err := rep.ActualLRPInstanceKeyFromContainer(*container, a.cellID)
			if err != nil {
				logger.Error("failed-to-extract-key", err)
				continue
			}
			var state string
			switch container.State {
			case executor.StateRunning:
				state = models.ActualLRPStateRunning
			case executor.StateCompleted:
				state = "SHUTDOWN"
				if container.RunResult.Failed {
					state = "CRASHED"
				}
			default:
				state = models.ActualLRPStateClaimed
			}
			lrp := models.NewSchedulingLRP(instanceKey.InstanceGuid, *key, resource, placementConstraint)
			lrp.State = state
			lrps = append(lrps, lrp)
		case rep.TaskLifecycle:
			domain := container.Tags[rep.DomainTag]
			state := models.Task_Running
			if container.State == executor.StateCompleted {
				state = models.Task_Completed
			}
			task := models.NewSchedulingTask(container.Guid, domain, resource, placementConstraint)
			task.State = state
			task.Failed = container.RunResult.Failed
			tasks = append(tasks, task)
		}
	}

	allocatedProxyMemory := 0
	if a.enableContainerProxy {
		allocatedProxyMemory = a.proxyMemoryAllocation
	}

	state := models.NewCellState(
		a.cellID,
		a.cellIndex,
		a.repURL,
		a.rootFSProviders,
		a.convertResources(availableResources),
		a.convertResources(totalResources),
		lrps,
		tasks,
		a.zone,
		startingContainerCount,
		a.evacuationReporter.Evacuating(),
		volumeDrivers,
		a.placementTags,
		a.optionalPlacementTags,
		allocatedProxyMemory,
	)
	state.StartTime = a.startTime
	state.CachedDropletHashes = scanCachedDropletHashes(a.cachePath)

	healthy := a.client.Healthy(logger)
	if !healthy {
		logger.Error("failed-garden-health-check", nil)
	}

	logger.Info("provided", lager.Data{
		"available-resources": state.AvailableResources,
		"total-resources":     state.TotalResources,
		"num-lrps":            len(state.LRPs),
		"zone":                state.Zone,
		"evacuating":          state.Evacuating,
	})

	return state, healthy, nil
}

// cachedDropletFile records the on-disk name and metadata of one cache entry.
type cachedDropletFile struct {
	name    string
	size    int64
	modTime time.Time
}

// scanCachedDropletHashes returns the unique set of 32-char lowercase hex
// cache-key prefixes found in dir. Each cached file is named
// {md5hex}-{nanoseconds}-{seq}; the prefix is the MD5 of the original cache
// key and is what the auctioneer uses to identify which droplets are on disk.
//
// As a side-effect it merges newly discovered files into saved_cache.json so
// that cacheddownloader's RecoverState on the next rep restart keeps them
// instead of wiping them (RecoverState deletes any file not listed there).
func scanCachedDropletHashes(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	diskFiles := map[string]cachedDropletFile{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 32 {
			continue
		}
		prefix := name[:32]
		if len(name) > 32 && name[32] != '-' {
			continue
		}
		if !isLowercaseHex(prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Keep the most recently modified file per prefix (mirrors Python logic).
		if existing, ok := diskFiles[prefix]; !ok || info.ModTime().After(existing.modTime) {
			diskFiles[prefix] = cachedDropletFile{name: name, size: info.Size(), modTime: info.ModTime()}
		}
	}
	if len(diskFiles) == 0 {
		return nil
	}
	mergeSavedCacheJSON(dir, diskFiles)
	hashes := make([]string, 0, len(diskFiles))
	for h := range diskFiles {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	return hashes
}

// savedCacheJSON mirrors the json.Marshal(*FileCache) layout written by
// cacheddownloader.SaveState so we can read and write saved_cache.json
// without importing cacheddownloader.
type savedCacheJSON struct {
	CachedPath string                      `json:"CachedPath"`
	Entries    map[string]*savedCacheEntry `json:"Entries"`
	OldEntries map[string]*savedCacheEntry `json:"OldEntries"`
	Seq        uint64                      `json:"Seq"`
}

type savedCacheEntry struct {
	Size                  int64           `json:"Size"`
	Access                time.Time       `json:"Access"`
	CachingInfo           savedCachingInfo `json:"CachingInfo"`
	FilePath              string          `json:"FilePath"`
	ExpandedDirectoryPath string          `json:"ExpandedDirectoryPath"`
}

type savedCachingInfo struct {
	ETag         string `json:"ETag"`
	LastModified string `json:"LastModified"`
}

// mergeSavedCacheJSON updates saved_cache.json in dir to reflect the current
// set of on-disk droplet files. It preserves any real ETag/Last-Modified
// that cacheddownloader's own SaveState may have written for already-tracked
// entries, drops stale entries whose files are gone, and synthesises
// placeholder entries for newly discovered files. Written atomically via a
// temp-file + rename so a concurrent SaveState() call cannot observe a
// partial write.
func mergeSavedCacheJSON(dir string, diskFiles map[string]cachedDropletFile) {
	savedPath := filepath.Join(dir, "saved_cache.json")

	// Load whatever SaveState() may have already written so real ETags survive.
	existing := savedCacheJSON{
		Entries:    map[string]*savedCacheEntry{},
		OldEntries: map[string]*savedCacheEntry{},
	}
	if data, err := os.ReadFile(savedPath); err == nil {
		_ = json.Unmarshal(data, &existing)
		if existing.Entries == nil {
			existing.Entries = map[string]*savedCacheEntry{}
		}
		if existing.OldEntries == nil {
			existing.OldEntries = map[string]*savedCacheEntry{}
		}
	}

	// Build set of full paths present on disk for staleness checks.
	onDiskPaths := make(map[string]struct{}, len(diskFiles))
	for _, f := range diskFiles {
		onDiskPaths[filepath.Join(dir, f.name)] = struct{}{}
	}

	// Drop entries whose backing file is no longer on disk.
	for key, entry := range existing.Entries {
		if entry.FilePath != "" {
			if _, ok := onDiskPaths[entry.FilePath]; !ok {
				delete(existing.Entries, key)
			}
		}
	}

	// Synthesise placeholder entries for files not yet tracked.
	for prefix, f := range diskFiles {
		if _, tracked := existing.Entries[prefix]; tracked {
			continue
		}
		existing.Entries[prefix] = &savedCacheEntry{
			Size:     f.size,
			Access:   f.modTime,
			FilePath: filepath.Join(dir, f.name),
		}
	}

	existing.CachedPath = dir

	data, err := json.Marshal(&existing)
	if err != nil {
		return
	}
	tmpPath := savedPath + ".heartbeat-tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return
	}
	// #nosec G104 - best-effort; if rename fails the old file is untouched
	_ = os.Rename(tmpPath, savedPath)
}

func isLowercaseHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (a *AuctionCellRep) Metrics(logger lager.Logger) (*rep.ContainerMetricsCollection, error) {
	var lrpMetrics = []rep.LRPMetric{}
	var taskMetrics = []rep.TaskMetric{}

	logger = logger.Session("metrics-collection")
	logger.Info("starting")
	defer logger.Info("complete")

	containers, err := a.client.ListContainers(logger)
	if err != nil {
		logger.Error("failed-to-fetch-containers", err)
		return nil, err
	}

	metrics := a.containerMetricsProvider.Metrics()

	for _, container := range containers {
		if container.Tags == nil {
			logger.Error("failed-to-extract-container-tags", nil)
			continue
		}

		guid := container.Guid
		containerMetrics, ok := metrics[guid]
		if !ok {
			logger.Info("failed-to-get-metrics-for-container", lager.Data{"guid": guid})
			continue
		}

		switch container.Tags[rep.LifecycleTag] {
		case rep.LRPLifecycle:
			key, err := rep.ActualLRPKeyFromTags(container.Tags)
			if err != nil {
				logger.Error("failed-to-extract-key", err)
				continue
			}
			instanceKey, err := rep.ActualLRPInstanceKeyFromContainer(container, a.cellID)
			if err != nil {
				logger.Error("failed-to-extract-key", err)
				continue
			}

			lrpMetric := rep.LRPMetric{
				ProcessGUID:            key.ProcessGuid,
				Index:                  key.Index,
				InstanceGUID:           instanceKey.InstanceGuid,
				CachedContainerMetrics: *containerMetrics,
			}
			lrpMetrics = append(lrpMetrics, lrpMetric)
		case rep.TaskLifecycle:
			taskMetric := rep.TaskMetric{
				TaskGUID:               container.Guid,
				CachedContainerMetrics: *containerMetrics,
			}
			taskMetrics = append(taskMetrics, taskMetric)
		}
	}

	return &rep.ContainerMetricsCollection{
		CellID: a.cellID,
		LRPs:   lrpMetrics,
		Tasks:  taskMetrics,
	}, nil
}

func containerIsStarting(container *executor.Container) bool {
	return container.State == executor.StateReserved ||
		container.State == executor.StateInitializing ||
		container.State == executor.StateCreated
}

func (a *AuctionCellRep) Perform(logger lager.Logger, traceID string, work models.Work) (models.Work, error) {
	var failedWork = models.Work{}

	logger = logger.Session("auction-work", lager.Data{
		"lrp-starts": len(work.LRPs),
		"tasks":      len(work.Tasks),
		"cell-id":    work.CellID,
	})

	if work.CellID != "" && work.CellID != a.cellID {
		logger.Error("cell-id-mismatch", ErrCellIdMismatch)
		return work, ErrCellIdMismatch
	}

	remainingResources, err := a.client.RemainingResources(logger)
	if err != nil {
		logger.Error("failed-gathering-remaining-reosurces", err)
		return work, err
	}

	var lrpRequests []models.SchedulingLRP
	remainingMemory := int32(remainingResources.MemoryMB)

	sort.SliceStable(work.LRPs, func(i, j int) bool {
		return work.LRPs[i].MemoryMB > work.LRPs[j].MemoryMB
	})

	for _, lrp := range work.LRPs {
		requiredMemory := lrp.MemoryMB
		if a.enableContainerProxy {
			requiredMemory += int32(a.proxyMemoryAllocation)
		}
		if requiredMemory <= remainingMemory {
			remainingMemory -= requiredMemory
			lrpRequests = append(lrpRequests, lrp)
		} else {
			failedWork.LRPs = append(failedWork.LRPs, lrp)
		}
	}

	if a.evacuationReporter.Evacuating() {
		return work, nil
	}

	unallocatedLRPs := a.allocator.BatchLRPAllocationRequest(logger, traceID, a.enableContainerProxy, a.proxyMemoryAllocation, lrpRequests)
	failedWork.LRPs = append(failedWork.LRPs, unallocatedLRPs...)
	failedWork.Tasks = a.allocator.BatchTaskAllocationRequest(logger, traceID, work.Tasks)

	return failedWork, nil
}

func (a *AuctionCellRep) convertResources(resources executor.ExecutorResources) models.Resources {
	return models.Resources{
		MemoryMB:   int32(resources.MemoryMB),
		DiskMB:     int32(resources.DiskMB),
		Containers: resources.Containers,
	}
}

func (a *AuctionCellRep) Reset() error {
	return errors.New("not-a-simulation-rep")
}
