// SPDX-License-Identifier: MIT

package local

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUsageAllocationAndHardLinkIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Link(path, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	var identity FileID
	for _, path := range []string{path, alias} {
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		u, err := UsageOf(path, fi)
		if err != nil || !u.IdentityKnown || u.Links != 2 || !u.AllocationKnown || u.Allocated < 0 {
			t.Fatalf("usage of %s: %+v, %v", path, u, err)
		}
		if identity != (FileID{}) && u.ID != identity {
			t.Fatalf("hard link identities differ: %+v and %+v", identity, u.ID)
		}
		identity = u.ID
	}
}

func TestUsageCountsSparseAllocationAndDoesNotFollowLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows sparse files need FSCTL_SET_SPARSE; covered by the native Windows test")
	}
	root := t.TempDir()
	path := filepath.Join(root, "sparse")
	if err := os.WriteFile(path, []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 16<<20); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	u, err := UsageOf(path, fi)
	if err != nil || !u.AllocationKnown || u.Allocated >= fi.Size() {
		t.Fatalf("sparse usage: %+v, %v", u, err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	li, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	lu, err := UsageOf(link, li)
	if err != nil || !lu.IdentityKnown || lu.ID == u.ID || lu.Allocated >= fi.Size() {
		t.Fatalf("followed symbolic link: %+v, %v", lu, err)
	}
}
