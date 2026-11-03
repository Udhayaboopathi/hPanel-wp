package wpimport

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// MergeWPvividParts stores the merged archive uncompressed, so
// the declared sizes of the parts are preflighted against
// MaxExtractedBytes before anything is written. This test forges
// central-directory size fields (4 parts × ~4 GB declared) so the
// declared total crosses the cap without writing gigabytes.
func TestMergeWPvividPartsRejectsDeclaredBomb(t *testing.T) {
	dir := t.TempDir()
	var parts []MergePart
	for _, kind := range []string{"plugins", "themes", "uploads", "others"} {
		p := filepath.Join(dir, "x-"+kind+".zip")
		writeZip(t, p, map[string]string{"a.txt": "tiny"})
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		// Central directory record: PK\x01\x02, uncompressed size
		// is the little-endian uint32 at offset 24.
		idx := bytes.Index(data, []byte("PK\x01\x02"))
		if idx < 0 {
			t.Fatal("central directory not found in test zip")
		}
		binary.LittleEndian.PutUint32(data[idx+24:], 0xFFFFFFFF)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, MergePart{Kind: kind, Path: p})
	}
	dst := filepath.Join(dir, "merged.zip")
	if _, err := MergeWPvividParts(dst, parts); err == nil {
		t.Fatal("expected the declared-size cap to refuse the merge")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("refused merge must not leave an output file (stat err=%v)", err)
	}
}
