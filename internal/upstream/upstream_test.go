package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestAPIClientUsesContextUserAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("User-Agent"); got != "Player/1.2" {
			t.Fatalf("User-Agent = %q, want Player/1.2", got)
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &apiClient{base: base, apiKey: "test-key", http: server.Client()}
	if err := client.getJSON(WithUserAgent(context.Background(), "Player/1.2"), "/probe", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWithUserAgentNeverReturnsEmpty(t *testing.T) {
	if got := contextUserAgent(WithUserAgent(context.Background(), "")); got == "" {
		t.Fatal("context User-Agent is empty")
	}
}
