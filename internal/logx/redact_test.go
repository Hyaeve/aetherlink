package logx

import (
	"strings"
	"testing"
)

func TestCredentialRedaction(t *testing.T) {
	for _, input := range []string{
		"GET /Videos/1?api_key=secret&MediaSourceId=source",
		"GET /Videos/1?API_KEY=secret&MediaSourceId=source",
		"GET /Videos/1?%61pi_key=secret&MediaSourceId=source",
		"GET /Videos/1?aetherlink_ticket=secret&MediaSourceId=source",
		`MediaBrowser Token="secret", Client="Player"`,
		"Authorization: Bearer secret",
		"GET /Videos/1?access_token=secret",
	} {
		output := Redact(input)
		if strings.Contains(output, "secret") || !strings.Contains(output, "[REDACTED]") {
			t.Fatalf("credential not redacted: %s", output)
		}
	}
	input := "https://cdn.example/movie.mkv?t=1800000000&MediaSourceId=source"
	if Redact(input) != input {
		t.Fatal("non-credential query changed")
	}
	Infof("GET /Videos/1?api_key=secret")
	if strings.Contains(Recent(1)[0].Message, "secret") {
		t.Fatal("buffered log leaked credential")
	}
}
