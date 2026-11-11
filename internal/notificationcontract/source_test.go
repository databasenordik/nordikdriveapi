package notificationcontract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestSourceClientPreservesSignedHeadersAndLargeSealedCommands(t *testing.T) {
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer signed-token" || r.Header.Get("X-Serverless-Authorization") != "Bearer signed-token" {
			t.Error("original signed workload token was not preserved")
		}
		if strings.HasSuffix(r.URL.Path, "/receipt") {
			w.WriteHeader(204)
			return
		}
		_ = json.NewEncoder(w).Encode(SourceRecord{State: "pending", Command: Command{ID: "opaque-id", Ciphertext: strings.Repeat("x", 100000)}})
	}))
	defer server.Close()
	client, err := NewSourceClient(server.URL, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "signed-token"}))
	if err != nil {
		t.Fatal(err)
	}
	client.client.HTTP = server.Client()
	record, err := client.Read(context.Background(), "opaque-id")
	if err != nil || len(record.Command.Ciphertext) != 100000 {
		t.Fatal("sealed producer command was truncated", err)
	}
	if err := client.Receipt(context.Background(), SourceReceipt{OutboxID: "opaque-id", DeliveryID: "opaque-id", State: "provider_accepted"}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatal("unexpected source calls")
	}
	if _, err := NewSourceClient("http://untrusted.example", nil); err == nil {
		t.Fatal("unsafe source client configuration accepted")
	}
}
