package pathmap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTranslateLongestPrefixWins(t *testing.T) {
	mapper := New([]Rule{
		{From: "/audiobooks", To: "/NetDisk/115-Strm"},
		{From: "/audiobooks/cloud", To: "/NetDisk/CloudNAS"},
	}, nil)

	if got := mapper.Translate("/audiobooks/cloud/book/001.strm"); got != "/NetDisk/CloudNAS/book/001.strm" {
		t.Fatalf("Translate nested = %q", got)
	}
	if got := mapper.Translate("/audiobooks/book/001.strm"); got != "/NetDisk/115-Strm/book/001.strm" {
		t.Fatalf("Translate base = %q", got)
	}
	if got := mapper.Translate("/other/001.strm"); got != "/other/001.strm" {
		t.Fatalf("Translate unmapped = %q", got)
	}
}

func TestTranslateHandlesWindowsSeparators(t *testing.T) {
	mapper := New([]Rule{{From: "D:/Media", To: "/NetDisk"}}, nil)
	if got := mapper.Translate("D:\\Media\\Books\\001.strm"); got != "/NetDisk/Books/001.strm" {
		t.Fatalf("Translate windows path = %q", got)
	}
}

func TestCheckEnforcesRoots(t *testing.T) {
	mapper := New(nil, []string{"/NetDisk"})
	if _, err := mapper.Check("/etc/passwd"); err == nil {
		t.Fatal("Check should reject paths outside the configured roots")
	}
	if got, err := mapper.Check("/NetDisk/115-Strm/a.strm"); err != nil || got != "/NetDisk/115-Strm/a.strm" {
		t.Fatalf("Check inside root = %q, err %v", got, err)
	}
	// A root prefix match must respect path boundaries.
	if _, err := mapper.Check("/NetDiskEvil/a.strm"); err == nil {
		t.Fatal("Check should not treat /NetDiskEvil as inside /NetDisk")
	}
}

// 两侧挂载点不同名是最常见的部署形态：上游看到 /audiobooks/...，AetherLink
// 只挂了 /NetDisk/...。Locate 必须自己找到文件，不能强迫用户手写映射。
func TestLocateFindsFileUnderADifferentMountPoint(t *testing.T) {
	root := t.TempDir()
	bookDir := filepath.Join(root, "Book")
	if err := os.MkdirAll(bookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(bookDir, "001.strm")
	if err := os.WriteFile(pointer, []byte("http://10.0.0.31:19527/d/a.m4a"), 0o644); err != nil {
		t.Fatal(err)
	}

	mapper := New(nil, []string{Normalize(root)})
	resolved, found, err := mapper.Locate("/audiobooks/Set/Read/Book/001.strm")
	if err != nil {
		t.Fatalf("Locate returned error: %v", err)
	}
	if !found {
		t.Fatalf("Locate should have found the pointer under the configured root, got %q", resolved)
	}
	if resolved != Normalize(pointer) {
		t.Fatalf("resolved = %q, want %q", resolved, Normalize(pointer))
	}
}

func TestLocatePrefersAnExactMountMatch(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "Book")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	exact := filepath.Join(nested, "001.strm")
	if err := os.WriteFile(exact, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mapper := New([]Rule{{From: "/audiobooks", To: Normalize(root)}}, []string{Normalize(root)})
	resolved, found, err := mapper.Locate("/audiobooks/Book/001.strm")
	if err != nil || !found {
		t.Fatalf("Locate = %q, found = %v, err = %v", resolved, found, err)
	}
	if resolved != Normalize(exact) {
		t.Fatalf("resolved = %q, want the mapped path %q", resolved, Normalize(exact))
	}
}

func TestLocateReportsNotFoundWithoutError(t *testing.T) {
	root := t.TempDir()
	mapper := New(nil, []string{Normalize(root)})
	resolved, found, err := mapper.Locate(Normalize(filepath.Join(root, "missing", "001.strm")))
	if err != nil {
		t.Fatalf("a missing file is not a configuration error: %v", err)
	}
	if found {
		t.Fatal("found should be false for a missing pointer")
	}
	if resolved == "" {
		t.Fatal("the translated path should still be returned for error messages")
	}
}

func TestCheckWithoutRootsAllowsAnything(t *testing.T) {
	mapper := New(nil, nil)
	if _, err := mapper.Check("/anywhere/a.strm"); err != nil {
		t.Fatalf("Check with no roots should allow: %v", err)
	}
}
