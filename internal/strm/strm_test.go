package strm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aetherlink/aetherlink/internal/pathmap"
	"github.com/aetherlink/aetherlink/internal/urlx"
)

func TestParsePickCodePointer(t *testing.T) {
	target, err := Parse("http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a?/001.总序.m4a\n", "/NetDisk/115-Strm/book/001.strm", nil)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if target.Type != TargetRemote {
		t.Fatalf("Type = %q, want remote", target.Type)
	}
	if target.Kind != urlx.KindPickCode115 {
		t.Fatalf("Kind = %q, want pickcode115", target.Kind)
	}
	// The display filename lives in the query string, not the path.
	if target.Filename != "001.总序.m4a" {
		t.Fatalf("Filename = %q, want 001.总序.m4a", target.Filename)
	}
}

func TestParseOpenListPointer(t *testing.T) {
	pointer := "http://10.0.0.31:25244/d/移动云盘/电视剧/白色巨塔 (2003)/白色巨塔 (2003) S01E01.再读.mkv"
	target, err := Parse(pointer, "", nil)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if target.Kind != urlx.KindOpenList {
		t.Fatalf("Kind = %q, want openlist", target.Kind)
	}
	if strings.Contains(target.URL, " ") {
		t.Fatalf("URL still contains a raw space: %q", target.URL)
	}
	if target.Filename != "白色巨塔 (2003) S01E01.再读.mkv" {
		t.Fatalf("Filename = %q", target.Filename)
	}
}

func TestParseSkipsCommentLines(t *testing.T) {
	target, err := Parse("#EXTM3U\n#comment\nhttp://10.0.0.31:19527/d/abc.m4a\n", "", nil)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if target.URL != "http://10.0.0.31:19527/d/abc.m4a" {
		t.Fatalf("URL = %q", target.URL)
	}
}

func TestParseLocalTargetRespectsRoots(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "NetDisk")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mediaFile := filepath.Join(mediaDir, "chapter.m4a")
	if err := os.WriteFile(mediaFile, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	mapper := pathmap.New(nil, []string{pathmap.Normalize(mediaDir)})

	target, err := Parse(pathmap.Normalize(mediaFile), "", mapper)
	if err != nil {
		t.Fatalf("Parse local returned error: %v", err)
	}
	if target.Type != TargetLocal || target.Path != pathmap.Normalize(mediaFile) {
		t.Fatalf("unexpected local target %+v", target)
	}

	outside := filepath.Join(tempDir, "outside.m4a")
	if err := os.WriteFile(outside, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(pathmap.Normalize(outside), "", mapper); err == nil {
		t.Fatal("Parse should reject local targets outside the allowed roots")
	}
}

func TestReadRelativePointer(t *testing.T) {
	tempDir := t.TempDir()
	bookDir := filepath.Join(tempDir, "book")
	if err := os.MkdirAll(bookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mediaFile := filepath.Join(bookDir, "001.m4a")
	if err := os.WriteFile(mediaFile, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	strmFile := filepath.Join(bookDir, "001.strm")
	if err := os.WriteFile(strmFile, []byte("./001.m4a"), 0o644); err != nil {
		t.Fatal(err)
	}
	mapper := pathmap.New(nil, []string{pathmap.Normalize(tempDir)})

	target, err := Read(pathmap.Normalize(strmFile), mapper)
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if target.Type != TargetLocal || target.Path != pathmap.Normalize(mediaFile) {
		t.Fatalf("unexpected relative target %+v", target)
	}
}

func TestParseRejectsEmptyPointer(t *testing.T) {
	if _, err := Parse("\n \n", "", nil); err == nil {
		t.Fatal("Parse should reject an empty pointer")
	}
}

func TestIsStrmPath(t *testing.T) {
	if !IsStrmPath("/a/b/c.STRM") {
		t.Fatal("IsStrmPath should be case-insensitive")
	}
	if IsStrmPath("/a/b/c.m4a") {
		t.Fatal("IsStrmPath matched a non-strm file")
	}
}
