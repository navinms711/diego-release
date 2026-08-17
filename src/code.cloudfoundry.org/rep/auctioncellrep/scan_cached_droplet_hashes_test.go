package auctioncellrep

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanCachedDropletHashesIncludesExpandedDirectories(t *testing.T) {
	dir := t.TempDir()

	// Regular file: <32hex>-<ts>-<n>
	regularHash := "d35685fb33b7865ac0ad7a421656e528"
	if err := os.WriteFile(filepath.Join(dir, regularHash+"-1786960165419201156-1"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}

	// Expanded directory: <32hex>-<ts>-<n>.d  (buildpack app droplet format)
	dirHash := "24f165aaf2bba6b056b74d601cce247f"
	if err := os.Mkdir(filepath.Join(dir, dirHash+"-1786960165419201157-1.d"), 0755); err != nil {
		t.Fatal(err)
	}

	// Bare directory (no .d suffix): must be ignored
	if err := os.Mkdir(filepath.Join(dir, "somedir"), 0755); err != nil {
		t.Fatal(err)
	}

	// In-flight download temp: must be ignored
	otherHash := "cb74e82b24ab1a2d2e61844fcdcd9bfd"
	if err := os.WriteFile(filepath.Join(dir, otherHash+"-9999-1.download-tmp"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}

	hashes := scanCachedDropletHashes(dir)

	found := map[string]bool{}
	for _, h := range hashes {
		found[h] = true
	}

	if !found[regularHash] {
		t.Errorf("expected regular file hash %s to be included, got %v", regularHash, hashes)
	}
	if !found[dirHash] {
		t.Errorf("expected .d directory hash %s to be included, got %v", dirHash, hashes)
	}
	if found[otherHash] {
		t.Errorf("expected .download-tmp hash %s to be excluded, got %v", otherHash, hashes)
	}
	if len(hashes) != 2 {
		t.Errorf("expected exactly 2 hashes (file + .d dir), got %d: %v", len(hashes), hashes)
	}
}

func TestScanCachedDropletHashesDotDDirectoryWithoutDotDSuffixIgnored(t *testing.T) {
	dir := t.TempDir()

	// A plain directory (no .d suffix) should not be scanned
	plainDirHash := "aaef79f8bbb6d29d02f58036cea7deff"
	if err := os.Mkdir(filepath.Join(dir, plainDirHash+"-1000-1"), 0755); err != nil {
		t.Fatal(err)
	}

	hashes := scanCachedDropletHashes(dir)
	if len(hashes) != 0 {
		t.Errorf("expected no hashes for plain directory, got %v", hashes)
	}
}

func TestScanCachedDropletHashesPrefersMostRecentEntryPerPrefix(t *testing.T) {
	dir := t.TempDir()

	hash := "e57f0f5c22530f2e9570707c317f4963"

	// Older regular file
	oldFile := filepath.Join(dir, hash+"-1000-1")
	if err := os.WriteFile(oldFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}

	// Newer .d directory for the same hash — should win
	newDir := filepath.Join(dir, hash+"-2000-2.d")
	if err := os.Mkdir(newDir, 0755); err != nil {
		t.Fatal(err)
	}

	hashes := scanCachedDropletHashes(dir)
	if len(hashes) != 1 || hashes[0] != hash {
		t.Errorf("expected exactly [%s], got %v", hash, hashes)
	}
}
