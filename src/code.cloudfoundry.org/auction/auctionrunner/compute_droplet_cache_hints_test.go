package auctionrunner

import (
	"sort"
	"testing"

	"code.cloudfoundry.org/bbs/models"
)

// makeCell builds a minimal *Cell for computeDropletCacheHints testing.
// lrpGuids is the set of processGuids currently running on the cell.
// hashes is the set of cached droplet hashes advertised by the cell.
func makeTestCell(guid string, lrpGuids []string, hashes []string) *Cell {
	lrps := make([]models.SchedulingLRP, 0, len(lrpGuids))
	for _, pg := range lrpGuids {
		key := models.ActualLRPKey{ProcessGuid: pg, Index: 0, Domain: "test"}
		lrps = append(lrps, models.NewSchedulingLRP("inst-"+pg, key, models.Resource{}, models.PlacementConstraint{}))
	}

	state := models.CellState{
		LRPs:                lrps,
		CachedDropletHashes: hashes,
	}
	return &Cell{
		Guid:  guid,
		state: state,
	}
}

func sortedStrings(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

// TestComputeDropletCacheHintsFiltersUniversalHashes verifies that hashes
// present on every cell are excluded because they cannot divert a placement.
func TestComputeDropletCacheHintsFiltersUniversalHashes(t *testing.T) {
	universalHash := "24f165aaf2bba6b056b74d601cce247f" // on all 6 cells
	uniqueHash := "d35685fb33b7865ac0ad7a421656e528"    // only on cell-A (running the LRP)

	zones := map[string]Zone{
		"zone-a": {
			makeTestCell("cell-A", []string{"proc-1"}, []string{universalHash, uniqueHash}),
		},
		"zone-b": {
			makeTestCell("cell-B", []string{}, []string{universalHash}),
			makeTestCell("cell-C", []string{}, []string{universalHash}),
			makeTestCell("cell-D", []string{}, []string{universalHash}),
		},
	}

	hints := computeDropletCacheHints(zones, nil, "proc-1")

	// universalHash must be filtered out (all 4 cells have it — no divert signal)
	// uniqueHash must be retained (only cell-A has it)
	if len(hints) != 1 || hints[0] != uniqueHash {
		t.Errorf("expected hints=[%s], got %v", uniqueHash, hints)
	}
}

// TestComputeDropletCacheHintsNoRunningInstances verifies nil is returned when
// no cell currently runs the given processGuid.
func TestComputeDropletCacheHintsNoRunningInstances(t *testing.T) {
	zones := map[string]Zone{
		"zone-a": {
			makeTestCell("cell-A", []string{"proc-other"}, []string{"aabbccddeeff00112233445566778899"}),
		},
	}

	hints := computeDropletCacheHints(zones, nil, "proc-unknown")
	if hints != nil {
		t.Errorf("expected nil, got %v", hints)
	}
}

// TestComputeDropletCacheHintsAllHashesUniversal verifies that when all hints
// from the running cell are shared by every other cell, computeDropletCacheHints
// returns nil (no discriminating hints).
func TestComputeDropletCacheHintsAllHashesUniversal(t *testing.T) {
	h1 := "24f165aaf2bba6b056b74d601cce247f"
	h2 := "79d682aa49e8f9182daf000acb2de362"

	zones := map[string]Zone{
		"zone-a": {
			makeTestCell("cell-A", []string{"proc-1"}, []string{h1, h2}),
			makeTestCell("cell-B", []string{}, []string{h1, h2}),
		},
	}

	hints := computeDropletCacheHints(zones, nil, "proc-1")
	if len(hints) != 0 {
		t.Errorf("expected no hints (all universal), got %v", hints)
	}
}

// TestComputeDropletCacheHintsEvacuatingCell verifies that a single-instance
// app whose only instance is on the evacuating cell still yields hints, so
// the cache bonus can fire during a BOSH drain (the BOSH upgrade use-case).
func TestComputeDropletCacheHintsEvacuatingCell(t *testing.T) {
	dropletHash := "9deec318c5cb101344d782908969e4f7"

	// The only active cells: one pre-seeded neighbor has the hash; the rest don't.
	zones := map[string]Zone{
		"zone-a": {
			makeTestCell("cell-neighbor", []string{}, []string{dropletHash}),
		},
		"zone-b": {
			makeTestCell("cell-other-1", []string{}, []string{}),
			makeTestCell("cell-other-2", []string{}, []string{}),
		},
	}

	// The evacuating cell is the sole runner of proc-1 and has its droplet cached.
	evacuatingStates := []models.CellState{
		{
			LRPs:                makeTestCell("cell-evac", []string{"proc-1"}, []string{dropletHash}).state.LRPs,
			CachedDropletHashes: []string{dropletHash},
		},
	}

	hints := computeDropletCacheHints(zones, evacuatingStates, "proc-1")

	// dropletHash must be returned: the evacuating cell runs proc-1 and has the
	// hash, the neighbor also has it (1 of 3 active cells) → non-universal.
	if len(hints) != 1 || hints[0] != dropletHash {
		t.Errorf("expected hints=[%s], got %v", dropletHash, hints)
	}
}

// TestComputeDropletCacheHintsMultipleRunningCells verifies that the union of
// discriminating hashes from multiple cells running the LRP is returned.
func TestComputeDropletCacheHintsMultipleRunningCells(t *testing.T) {
	sharedWithAll := "24f165aaf2bba6b056b74d601cce247f"
	uniqueToA := "d35685fb33b7865ac0ad7a421656e528"
	uniqueToB := "9defc60d318a8eb9dd3d8f69f8ebd5a5"

	zones := map[string]Zone{
		"zone-a": {
			makeTestCell("cell-A", []string{"proc-1"}, []string{sharedWithAll, uniqueToA}),
			makeTestCell("cell-B", []string{"proc-1"}, []string{sharedWithAll, uniqueToB}),
			makeTestCell("cell-C", []string{}, []string{sharedWithAll}),
		},
	}

	hints := sortedStrings(computeDropletCacheHints(zones, nil, "proc-1"))
	expected := sortedStrings([]string{uniqueToA, uniqueToB})

	if len(hints) != len(expected) {
		t.Fatalf("expected hints %v, got %v", expected, hints)
	}
	for i := range hints {
		if hints[i] != expected[i] {
			t.Errorf("expected hints[%d]=%s, got %s", i, expected[i], hints[i])
		}
	}
}
