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

func TestSignAuthGolden(t *testing.T) {
	const privateKey = "0000000000000000000000000000000000000000000000000000000000000001"
	const timestamp = "1786186200"
	const wantAddress = "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"
	const wantSignature = "0x25c2c4ae6e0cf49450d82e78426929f4ddc2ef8b159b43a3c0229ba7d02121e57503ff9e543acbe45d6ae20fd7d607c1f76660e49f09d2b5c38c857439b1cef51c"
	key, address, err := ParsePrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if address != wantAddress {
		t.Fatalf("address=%s want=%s", address, wantAddress)
	}
	signature, err := signAuth(key, address, timestamp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if signature != wantSignature {
		t.Fatalf("signature=%s want=%s", signature, wantSignature)
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
