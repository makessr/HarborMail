package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPEMTLSReload(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	now := time.Now()
	firstCert, firstKey := makeReloadTestPEM(t, 1, now.Add(-time.Hour), now.Add(time.Hour))
	secondCert, secondKey := makeReloadTestPEM(t, 2, now.Add(-time.Hour), now.Add(time.Hour))
	expiredCert, expiredKey := makeReloadTestPEM(t, 3, now.Add(-2*time.Hour), now.Add(-time.Hour))
	writePEM := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writePEM(certPath, firstCert)
	writePEM(keyPath, firstKey)
	t.Setenv("HARBOR_CERT", certPath)
	t.Setenv("HARBOR_KEY", keyPath)
	t.Setenv("HARBOR_ACME_HOSTS", "")
	inbound, err := configureTLS()
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(firstCert)
	roots.AppendCertsFromPEM(secondCert)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	runtime := newServiceRuntime()
	defer runtime.shutdown()
	runtime.serveHTTP("test HTTPS", &http.Server{TLSConfig: inbound.config.Clone()}, listener)
	verifySerial := func(expected int64) {
		t.Helper()
		// An IP address exercises the callback without client SNI.
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", listener.Addr().String(),
			&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if actual := conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64(); actual != expected {
			t.Fatalf("TLS served certificate %d, want %d", actual, expected)
		}
	}
	verifySerial(1)
	if changed, err := inbound.pem.reload(); changed || err != nil {
		t.Fatalf("unchanged certificate pair: changed=%v, err=%v", changed, err)
	}
	writePEM(certPath, secondCert)
	if changed, err := inbound.pem.reload(); changed || err == nil {
		t.Fatalf("mismatched replacement: changed=%v, err=%v", changed, err)
	}
	verifySerial(1)
	writePEM(keyPath, secondKey)
	if changed, err := inbound.pem.reload(); !changed || err != nil {
		t.Fatalf("valid replacement: changed=%v, err=%v", changed, err)
	}
	verifySerial(2)
	writePEM(certPath, expiredCert)
	writePEM(keyPath, expiredKey)
	if changed, err := inbound.pem.reload(); changed || err == nil {
		t.Fatalf("expired replacement: changed=%v, err=%v", changed, err)
	}
	verifySerial(2)
}

func makeReloadTestPEM(t *testing.T, serial int64, start, end time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial), NotBefore: start, NotAfter: end,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true, IsCA: true, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	cert, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKey})
}
