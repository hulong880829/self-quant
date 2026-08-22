package polymarketauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testPrivateKey = "0000000000000000000000000000000000000000000000000000000000000001"

func TestCreateOrDeriveDoesNotCreateForArbitraryBadRequest(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			creates++
		}
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(writer, `{"error":"invalid signature"}`)
	}))
	defer server.Close()
	client := NewClient(server.URL, time.Second)
	_, _, err := client.CreateOrDerive(context.Background(), testPrivateKey, 0)
	if err == nil {
		t.Fatal("expected derive error")
	}
	if creates != 0 {
		t.Fatalf("unexpected create attempts=%d", creates)
	}
}

func TestCreateOrDeriveCreatesOnlyWhenDeriveExplicitlyMissing(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(writer, `{"error":"Could not derive api key"}`)
			return
		}
		creates++
		_, _ = fmt.Fprint(writer, `{"apiKey":"key","secret":"secret","passphrase":"pass"}`)
	}))
	defer server.Close()
	client := NewClient(server.URL, time.Second)
	credentials, _, err := client.CreateOrDerive(context.Background(), testPrivateKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if creates != 1 || credentials.APIKey != "key" {
		t.Fatalf("creates=%d credentials=%+v", creates, credentials)
	}
}

func TestDeriveNeverCreatesCredentials(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			creates++
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client := NewClient(server.URL, time.Second)
	_, _, err := client.Derive(context.Background(), testPrivateKey, 7)
	if err == nil {
		t.Fatal("expected derive error")
	}
	if creates != 0 {
		t.Fatalf("unexpected create attempts=%d", creates)
	}
}
