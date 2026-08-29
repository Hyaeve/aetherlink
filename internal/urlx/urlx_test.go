package urlx

import "testing"

func TestNormalizePickCodeStyle(t *testing.T) {
	// 115 pick-code STRM pointers keep the display filename in the query.
	raw := "http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a?/001.总序.m4a"
	got, err := Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize returned error: %v", err)
	}
	want := "http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a?/001.%E6%80%BB%E5%BA%8F.m4a"
	if got != want {
		t.Fatalf("Normalize = %q, want %q", got, want)
	}
	if kind := Classify(got); kind != KindPickCode115 {
		t.Fatalf("Classify = %q, want %q", kind, KindPickCode115)
	}
}

func TestNormalizeOpenListCJKWithSpaces(t *testing.T) {
	raw := "http://10.0.0.31:25244/d/移动云盘/移动云资源/电视剧/电视剧集/白色巨塔 (2003)/白色巨塔 (2003) S01E01.再读.mkv"
	got, err := Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize returned error: %v", err)
	}
	if want := "http://10.0.0.31:25244/d/%E7%A7%BB%E5%8A%A8%E4%BA%91%E7%9B%98"; got[:len(want)] != want {
		t.Fatalf("Normalize prefix = %q, want %q", got[:len(want)], want)
	}
	if contains(got, " ") {
		t.Fatalf("Normalize left a raw space in %q", got)
	}
	if kind := Classify(got); kind != KindOpenList {
		t.Fatalf("Classify = %q, want %q", kind, KindOpenList)
	}
}

func TestNormalizePreservesExistingEscapes(t *testing.T) {
	raw := "http://10.0.0.31:25244/d/139-0211/%E6%9C%89%E5%A3%B0%E8%AF%BB%E7%89%A9/%E3%80%8A%E7%A9%BF%E8%B6%8A%E3%80%8B%26%E8%80%81%E8%B4%A2%20971/0001-0500/001.m4a"
	got, err := Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize returned error: %v", err)
	}
	if got != raw {
		t.Fatalf("Normalize modified an already encoded URL:\n got %q\nwant %q", got, raw)
	}
}

func TestNormalizeRejectsNonHTTP(t *testing.T) {
	for _, raw := range []string{"", "ftp://example.com/a.mp3", "file:///media/a.mp3"} {
		if _, err := Normalize(raw); err == nil {
			t.Fatalf("Normalize(%q) should have failed", raw)
		}
	}
}

func TestIsPrivateHost(t *testing.T) {
	cases := map[string]bool{
		"http://10.0.0.31:19527/d/a.m4a": true,
		"http://192.168.1.9/media.mkv":   true,
		"http://172.16.4.1/media.mkv":    true,
		"http://127.0.0.1:8080/x.mp3":    true,
		"http://localhost:8080/x.mp3":    true,
		"https://cdn.example.com/x.mp3":  false,
		"http://8.8.8.8/x.mp3":           false,
	}
	for target, want := range cases {
		if got := IsPrivateHost(target); got != want {
			t.Errorf("IsPrivateHost(%q) = %v, want %v", target, got, want)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
