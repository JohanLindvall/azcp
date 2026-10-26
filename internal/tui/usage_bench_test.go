// SPDX-License-Identifier: MIT

package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkUsageScan(b *testing.B) {
	root := b.TempDir()
	for i := range 10000 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%05d", i)), []byte("data"), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	backend, base, progress := testBackend(b), loc(b, root), reporter(b)
	b.ReportAllocs()
	for b.Loop() {
		tree, err := backend.scanUsage(b.Context(), base, false, progress)
		if err != nil || tree.root.total.files != 10000 {
			b.Fatalf("scan: %v", err)
		}
	}
	b.ReportMetric(10000, "files/op")
}
