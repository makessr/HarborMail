package server

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebHealthcheck(t *testing.T) {
	t.Setenv("HARBOR_ACME_HOSTS", "")
	t.Setenv("HARBOR_CERT", "")
	t.Setenv("HARBOR_WEB_TLS", "off")

	handler := http.NewServeMux()
	handler.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	t.Setenv("HARBOR_WEB", httpServer.Listener.Addr().String())
	if err := webHealthcheck(); err != nil {
		t.Fatalf("plain HTTP probe: %v", err)
	}

	tlsServer := httptest.NewTLSServer(handler)
	defer tlsServer.Close()
	t.Setenv("HARBOR_WEB", tlsServer.Listener.Addr().String())
	t.Setenv("HARBOR_WEB_TLS", "on")
	certFile := filepath.Join(t.TempDir(), "leaf.pem")
	writeCert := func(cert []byte) {
		t.Helper()
		if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HARBOR_CERT", certFile)
	writeCert(tlsServer.Certificate().Raw)
	if err := webHealthcheck(); err != nil {
		t.Fatalf("pinned HTTPS probe: %v", err)
	}
	writeCert([]byte("different certificate"))
	if err := webHealthcheck(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched HTTPS probe: %v", err)
	}

	t.Setenv("HARBOR_WEB_TLS", "off")
	t.Setenv("HARBOR_CERT", "")
	redirect := httptest.NewServer(http.RedirectHandler(httpServer.URL+"/healthz", http.StatusFound))
	defer redirect.Close()
	t.Setenv("HARBOR_WEB", redirect.Listener.Addr().String())
	if err := webHealthcheck(); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirect probe: %v", err)
	}
}
