package pathmap

import "testing"

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

func TestCheckWithoutRootsAllowsAnything(t *testing.T) {
	mapper := New(nil, nil)
	if _, err := mapper.Check("/anywhere/a.strm"); err != nil {
		t.Fatalf("Check with no roots should allow: %v", err)
	}
}
