// rep-disk-attach-server listens for HTTP requests from Diego cell pre-start scripts
// and runs govc vm.disk.attach for the requested disk. Run on a jumpbox with bosh, govc, and jq.
//
// Environment:
//   - PORT: listen port (default 80)
//   - BOSH_DEPLOYMENT: BOSH deployment name (e.g. cf)
//   - BOSH_INSTANCE_GROUP: instance group name for cells (default diego_cell)
//   - DATASTORE: vSphere datastore name for disks (e.g. iscsi-storage)
//   - AUTH_KEY: pre-shared key; requests must send X-Attach-Token: <AUTH_KEY> (optional)
//   - GOVC_*: vCenter credentials (GOVC_URL, GOVC_USERNAME, GOVC_PASSWORD, etc.)
//   - ATTACH_DEVICE_INFO_LOG: if set, append govc device.info JSON for the attached disk to this file (default: attach-device-info.log)
//   - ATTACH_HISTORY_FILE: optional NDJSON; short-form rep_cache_N uses last backing_vmdk for cell index N.
//     Matches lines with cell_instance diego_cell/N or compute/N (6.x vs 10.x), or cell_index field (new writes).
//
// Route: POST /attach-disk/<disk-name-or-label>
//
//	e.g. POST /attach-disk/rep_cache_0
//
// Disk name is used as [DATASTORE] <disk-name>.vmdk. The numeric suffix is the BOSH instance
// index; the server resolves VM CID from bosh instances by matching diego_cell/<uuid> (10.x) or
// compute/<uuid> (6.x) for that index. Attach history matches diego_cell/N, compute/N, or cell_index.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	diskNameRe = regexp.MustCompile(`^rep_cache_(\d+)$`)
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	deployment := os.Getenv("BOSH_DEPLOYMENT_NAME")
	instanceGroup := os.Getenv("BOSH_INSTANCE_GROUP")
	if instanceGroup == "" {
		instanceGroup = "diego_cell"
	}
	datastore := os.Getenv("GOVC_DATASTORE")
	authKey := os.Getenv("ATTACH_SERVER_AUTH_KEY")

	if deployment == "" || datastore == "" {
		log.Fatal("BOSH_DEPLOYMENT and DATASTORE must be set")
	}
	h := &handler{
		deployment:    deployment,
		instanceGroup: instanceGroup,
		datastore:     datastore,
		authKey:       authKey,
	}

	http.HandleFunc("/attach-disk/", h.attachDisk)
	log.Printf("rep-disk-attach-server listening on :%s (deployment=%s instance_group=%s datastore=%s)", port, deployment, instanceGroup, datastore)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

type handler struct {
	deployment    string
	instanceGroup string
	datastore     string
	authKey       string
}

// historyRecord is one line of ATTACH_HISTORY_FILE (NDJSON) for reading (lookup).
type historyRecord struct {
	CellInstance string `json:"cell_instance"`
	CellIndex    string `json:"cell_index"` // optional: stable key across 6.x compute vs 10.x diego_cell
	BackingVMDK  string `json:"backing_vmdk"`
}

// attachHistoryEntry is one line written to ATTACH_HISTORY_FILE after a successful attach.
type attachHistoryEntry struct {
	Timestamp      string `json:"timestamp"`
	BoshDeployment string `json:"bosh_deployment"`
	CellInstance   string `json:"cell_instance"` // e.g. diego_cell/0 — matched bosh instance group + index
	CellIndex      string `json:"cell_index"`    // same numeric index; lookup works after 6→10 rename
	VMCID          string `json:"vm_cid"`
	DiskName       string `json:"disk_name"`
	BlockDevice    string `json:"block_device"`
	BackingVMDK    string `json:"backing_vmdk"`
}

// historyMatchesCellIndex returns true if this history line applies to BOSH cell index cellIndex
// (diego_cell/N and compute/N from 10.x vs 6.x, optional cell_index field, or configured instance group).
func historyMatchesCellIndex(rec historyRecord, cellIndex string, configuredGroup string) bool {
	if rec.BackingVMDK == "" {
		return false
	}
	if rec.CellIndex != "" && rec.CellIndex == cellIndex {
		return true
	}
	for _, prefix := range []string{"diego_cell/", "compute/"} {
		if rec.CellInstance == prefix+cellIndex {
			return true
		}
	}
	if configuredGroup != "" && rec.CellInstance == configuredGroup+"/"+cellIndex {
		return true
	}
	return false
}

// getLastBackingVMDKFromHistory returns the backing_vmdk from the last line matching cell index
// (diego_cell/N, compute/N, cell_index, or configured instance group).
func getLastBackingVMDKFromHistory(historyPath, cellIndex string, configuredInstanceGroup string) string {
	f, err := os.Open(historyPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	var last string
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var rec historyRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if historyMatchesCellIndex(rec, cellIndex, configuredInstanceGroup) {
			last = rec.BackingVMDK
		}
	}
	return last
}

// backingVMDKFromDeviceInfo extracts the backing fileName from govc device.info JSON (devices[0].backing.fileName or .backing.FileName).
func backingVMDKFromDeviceInfo(infoJSON []byte) string {
	var out struct {
		Devices []struct {
			Backing *struct {
				FileName string `json:"fileName"`
			} `json:"backing"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(infoJSON, &out); err != nil {
		return ""
	}
	if len(out.Devices) == 0 || out.Devices[0].Backing == nil {
		return ""
	}
	return out.Devices[0].Backing.FileName
}

// appendAttachHistory appends one NDJSON line. matchedInstancePrefix is diego_cell or compute from bosh row.
func appendAttachHistory(historyPath, deployment, matchedInstancePrefix, cellIndex, vmCID, deviceName, diskPath string, infoJSON []byte) error {
	if historyPath == "" {
		return nil
	}
	backing := backingVMDKFromDeviceInfo(infoJSON)
	if backing == "" {
		backing = diskPath
	}
	absPath, err := filepath.Abs(historyPath)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(absPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	entry := attachHistoryEntry{
		Timestamp:      time.Now().Format("20060102-150405"),
		BoshDeployment: deployment,
		CellInstance:   matchedInstancePrefix + "/" + cellIndex,
		CellIndex:      cellIndex,
		VMCID:          vmCID,
		DiskName:       "rep_cache_" + cellIndex + ".vmdk",
		BlockDevice:    deviceName,
		BackingVMDK:    backing,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

func (h *handler) attachDisk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.authKey != "" {
		token := r.Header.Get("X-Attach-Token")
		if token != h.authKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	rawPath := strings.TrimPrefix(r.URL.Path, "/attach-disk/")
	rawPath = strings.Trim(rawPath, "/")
	if rawPath == "" {
		http.Error(w, "missing disk name or path in URL", http.StatusBadRequest)
		return
	}
	diskName, err := url.PathUnescape(rawPath)
	if err != nil {
		diskName = rawPath
	}

	var cellIndex string
	var diskPath string
	usedDefaultEmptyDisk := false

	if matches := diskNameRe.FindStringSubmatch(diskName); matches != nil {
		// Short form: rep_cache_<index>
		cellIndex = matches[1]
		if path := os.Getenv("ATTACH_HISTORY_FILE"); path != "" {
			if last := getLastBackingVMDKFromHistory(path, cellIndex, h.instanceGroup); last != "" {
				diskPath = last
				log.Printf("[attach] short-form %s: using backing_vmdk from history: %s", diskName, diskPath)
			} else {
				diskPath = "[" + h.datastore + "] " + diskName + ".vmdk"
				usedDefaultEmptyDisk = true
				log.Printf("[attach] short-form %s: no history match for cell index %s (diego_cell/compute/cell_index), using default: %s", diskName, cellIndex, diskPath)
			}
		} else {
			diskPath = "[" + h.datastore + "] " + diskName + ".vmdk"
			usedDefaultEmptyDisk = true
			log.Printf("[attach] short-form %s: ATTACH_HISTORY_FILE not set, using default: %s", diskName, diskPath)
		}
	} else if strings.HasPrefix(diskName, "[") && strings.Contains(diskName, "] ") && strings.HasSuffix(diskName, ".vmdk") {
		// Full datastore path: [datastore] path/to/file.vmdk
		diskPath = diskName
		log.Printf("[attach] full path from request: %s", diskPath)
		cellIndex = r.URL.Query().Get("index")
		if cellIndex == "" {
			var body struct {
				Index interface{} `json:"index"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Index != nil {
				switch v := body.Index.(type) {
				case float64:
					cellIndex = fmt.Sprintf("%.0f", v)
				case string:
					cellIndex = v
				}
			}
		}
		if cellIndex == "" {
			http.Error(w, "index required (query ?index=N or JSON body {\"index\": N}) when using full disk path", http.StatusBadRequest)
			return
		}
	} else {
		http.Error(w, "path must be rep_cache_<index> or full path [datastore] path/to/file.vmdk", http.StatusBadRequest)
		return
	}

	// Resolve VM CID: try diego_cell (10.x) then compute (6.x) then configured BOSH_INSTANCE_GROUP.
	vmCID, matchedPrefix, err := h.getVMCID(cellIndex)
	if err != nil {
		log.Printf("get VM CID for index %s: %v", cellIndex, err)
		http.Error(w, fmt.Sprintf("failed to get VM CID: %v", err), http.StatusInternalServerError)
		return
	}
	if vmCID == "" {
		http.Error(w, "VM CID not found for index "+cellIndex+" (tried diego_cell, compute, "+h.instanceGroup+")", http.StatusNotFound)
		return
	}
	log.Printf("[attach] resolved VM CID for index %s via instance prefix %s", cellIndex, matchedPrefix)

	// govc vm.disk.attach -vm=$VM_CID -disk="[datastore] disk_name.vmdk" or full path
	log.Printf("[attach] running: govc vm.disk.attach -vm=%s -disk=%q", vmCID, diskPath)
	if err := h.attachDiskToVM(vmCID, diskPath); err != nil {
		log.Printf("[attach] govc failed: %v", err)
		http.Error(w, fmt.Sprintf("attach failed: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[attach] govc succeeded; attached %s to VM %s", diskPath, vmCID)

	// Run govc device.info for the attached disk and return/append that output
	deviceName, infoJSON, err := h.getDeviceInfoForAttachedDisk(vmCID, diskPath)
	if err != nil {
		log.Printf("govc device.info for %s: %v (returning status ok with error in body)", diskPath, err)
		infoJSON = []byte(fmt.Sprintf(`{"status":"ok","vm_cid":%q,"disk":%q,"device_info_error":%q}`, vmCID, diskPath, err.Error()))
	} else {
		log.Printf("device info for %s: %s", deviceName, string(infoJSON))
	}

	// Append device info to local log file
	if logPath := h.deviceInfoLogPath(); logPath != "" {
		if err := h.appendDeviceInfoLog(logPath, vmCID, diskPath, deviceName, infoJSON); err != nil {
			log.Printf("append device info log: %v", err)
		}
	}

	// Append to ATTACH_HISTORY_FILE (NDJSON) for future short-form lookups
	if historyPath := os.Getenv("ATTACH_HISTORY_FILE"); historyPath != "" {
		if err := appendAttachHistory(historyPath, h.deployment, matchedPrefix, cellIndex, vmCID, deviceName, diskPath, infoJSON); err != nil {
			log.Printf("append attach history: %v", err)
		} else {
			log.Printf("[attach] appended to %s for %s/%s (cell_index=%s)", historyPath, matchedPrefix, cellIndex, cellIndex)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if usedDefaultEmptyDisk {
		w.WriteHeader(http.StatusAccepted) // 202: cell will treat as new empty disk, format ext4 and mount
	} else {
		w.WriteHeader(http.StatusOK) // 200: pre-warmed disk with REP_CACHE label
	}
	_, _ = w.Write(infoJSON)
}

// instanceGroupPrefixes returns bosh instance name prefixes to try, in order (10.x then 6.x legacy).
func instanceGroupPrefixes(configured string) []string {
	seen := map[string]struct{}{}
	var order []string
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		order = append(order, p)
	}
	add("diego_cell")
	add("compute")
	add(configured)
	return order
}

func (h *handler) getVMCID(cellIndex string) (vmCID string, matchedPrefix string, err error) {
	cmd := exec.Command("bosh", "-d", h.deployment, "instances", "--details", "--json")
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("bosh instances: %w", err)
	}
	var result struct {
		Tables []struct {
			Rows []struct {
				Index    string `json:"index"`
				Instance string `json:"instance"`
				VMCID    string `json:"vm_cid"`
			} `json:"Rows"`
		} `json:"Tables"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", "", fmt.Errorf("parse bosh json: %w", err)
	}
	for _, ig := range instanceGroupPrefixes(h.instanceGroup) {
		prefix := ig + "/"
		for _, t := range result.Tables {
			for _, row := range t.Rows {
				if row.Index == cellIndex && strings.HasPrefix(row.Instance, prefix) && row.VMCID != "" {
					return row.VMCID, ig, nil
				}
			}
		}
	}
	return "", "", nil
}

func (h *handler) attachDiskToVM(vmCID, diskPath string) error {
	cmd := exec.Command("govc", "vm.disk.attach", "-vm="+vmCID, "-disk="+diskPath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if outStr != "" {
		log.Printf("[attach] govc output: %s", outStr)
	}
	if err != nil {
		// Idempotent: if disk is already attached, govc may exit non-zero; treat as success
		if strings.Contains(string(out), "already exists") || strings.Contains(string(out), "already attached") {
			return nil
		}
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

// deviceInfoLogPath returns the path to append device info to (env ATTACH_DEVICE_INFO_LOG or default).
func (h *handler) deviceInfoLogPath() string {
	if p := os.Getenv("ATTACH_DEVICE_INFO_LOG"); p != "" {
		return p
	}
	return "attach-device-info.log"
}

// appendDeviceInfoLog appends a line with timestamp, vmCID, diskPath, deviceName and the info JSON to the file.
func (h *handler) appendDeviceInfoLog(logPath, vmCID, diskPath, deviceName string, infoJSON []byte) error {
	absPath, err := filepath.Abs(logPath)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(absPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	line := fmt.Sprintf("[%s] vm=%s disk=%s device=%s\n%s\n",
		time.Now().Format(time.RFC3339), vmCID, diskPath, deviceName, string(infoJSON))
	_, err = f.WriteString(line)
	return err
}

// govc device.info JSON device list (camelCase or lowercase from govc).
type deviceListOutput struct {
	Devices []struct {
		Name    string `json:"Name"`
		Backing *struct {
			FileName string `json:"FileName"`
			Parent   *struct {
				FileName string `json:"FileName"`
			} `json:"Parent"`
		} `json:"Backing"`
	} `json:"Devices"`
}

// findAttachedDeviceName parses govc device.info JSON and returns the device name whose backing matches diskPath.
func findAttachedDeviceName(out []byte, diskPath string) string {
	var list deviceListOutput
	if err := json.Unmarshal(out, &list); err != nil {
		return ""
	}
	diskPathSuffix := diskPath
	if idx := strings.LastIndex(diskPath, "/"); idx >= 0 {
		diskPathSuffix = diskPath[idx+1:]
	}
	for _, d := range list.Devices {
		if d.Backing == nil {
			continue
		}
		matches := strings.Contains(d.Backing.FileName, diskPathSuffix) || strings.HasSuffix(d.Backing.FileName, diskPath) || d.Backing.FileName == diskPath
		if !matches && d.Backing.Parent != nil {
			matches = strings.Contains(d.Backing.Parent.FileName, diskPathSuffix) || strings.HasSuffix(d.Backing.Parent.FileName, diskPath) || d.Backing.Parent.FileName == diskPath
		}
		if matches {
			return d.Name
		}
	}
	return ""
}

// getDeviceInfoForAttachedDisk runs govc device.info -vm=vmCID -json, finds the disk backing matching diskPath,
// then runs govc device.info -vm=vmCID -json <deviceName> and returns that device's JSON.
func (h *handler) getDeviceInfoForAttachedDisk(vmCID, diskPath string) (deviceName string, infoJSON []byte, err error) {
	cmd := exec.Command("govc", "device.info", "-vm="+vmCID, "-json")
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("govc device.info list: %w", err)
	}
	deviceName = findAttachedDeviceName(out, diskPath)
	if deviceName == "" {
		// Try lowercase keys (some govc versions)
		var m map[string]interface{}
		_ = json.Unmarshal(out, &m)
		if devs, _ := m["devices"].([]interface{}); devs != nil {
			for _, d := range devs {
				dm, _ := d.(map[string]interface{})
				if dm == nil {
					continue
				}
				name, _ := dm["name"].(string)
				backing, _ := dm["backing"].(map[string]interface{})
				if backing != nil {
					fn, _ := backing["fileName"].(string)
					if strings.Contains(fn, diskPath) || strings.HasSuffix(fn, filepath.Base(diskPath)) {
						deviceName = name
						break
					}
				}
			}
		}
	}
	if deviceName == "" {
		return "", out, nil // return full device list if no single match
	}
	cmd2 := exec.Command("govc", "device.info", "-vm="+vmCID, "-json", deviceName)
	cmd2.Env = os.Environ()
	infoJSON, err = cmd2.Output()
	if err != nil {
		return deviceName, nil, fmt.Errorf("govc device.info %s: %w", deviceName, err)
	}
	return deviceName, infoJSON, nil
}
