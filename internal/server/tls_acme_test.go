package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
	"golang.org/x/net/dns/dnsmessage"
)

func TestACMECertificateLifecycle(t *testing.T) {
	pebble := os.Getenv("HARBOR_TEST_PEBBLE")
	if pebble == "" {
		t.Skip("set HARBOR_TEST_PEBBLE to the Pebble executable")
	}
	t.Run("HTTP-01 renewal and cached restart", func(t *testing.T) {
		checkACMECertificates(t, pebble, false)
	})
	t.Run("TLS-ALPN-01 issuance", func(t *testing.T) {
		checkACMECertificates(t, pebble, true)
	})
}

func checkACMECertificates(t *testing.T, pebble string, useALPN bool) {
	t.Helper()
	const hostname = "mail.harbor.test"
	dir := t.TempDir()
	addresses := acmeTestAddresses(t, []string{
		"HARBOR_WEB", "HARBOR_SMTP", "HARBOR_SUBMISSION", "HARBOR_SUBMISSIONS", "HARBOR_IMAPS", "HARBOR_POP3S",
		"HARBOR_ACME_HTTP", "pebble", "pebble-admin", "unused",
	})
	for key, address := range addresses {
		if strings.HasPrefix(key, "HARBOR_") {
			t.Setenv(key, address)
		}
	}
	directory := "https://" + addresses["pebble"] + "/dir"
	for key, value := range map[string]string{
		"HARBOR_ACME_HOSTS": hostname, "HARBOR_ACME_ACCEPT_TOS": "true", "HARBOR_ACME_CACHE": filepath.Join(dir, "cache"),
		"HARBOR_ACME_DIRECTORY": directory, "HARBOR_WEB_TLS": "on", "HARBOR_CERT": "", "HARBOR_KEY": "", "HARBOR_METRICS": "",
	} {
		t.Setenv(key, value)
	}
	httpAddress, alpnAddress := addresses["HARBOR_ACME_HTTP"], addresses["unused"]
	if useALPN {
		httpAddress, alpnAddress = addresses["unused"], addresses["HARBOR_WEB"]
	}
	api, stopPebble := startTestPebble(t, pebble, dir, hostname, addresses["pebble"], addresses["pebble-admin"], httpAddress, alpnAddress)
	rootPEM := acmeTestGet(t, api, "https://"+addresses["pebble-admin"]+"/roots/0")
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		t.Fatal("Pebble did not return a root certificate")
	}

	db, err := openDB("sqlite", filepath.Join(dir, "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	start := func() func() {
		t.Helper()
		config, err := configureTLS()
		if err != nil {
			t.Fatal(err)
		}
		config.acme.manager.Client.HTTPClient = api
		ctx, cancel := context.WithCancel(t.Context())
		server := &app{domain: "harbor.test", db: db, pauseOutbox: true}
		done := make(chan struct{})
		var serveErr error
		go func() {
			defer close(done)
			serveErr = server.serve(ctx, config)
		}()
		stop := sync.OnceFunc(func() {
			cancel()
			select {
			case <-done:
				if serveErr != nil {
					t.Errorf("Harbor listeners: %v", serveErr)
				}
			case <-time.After(5 * time.Second):
				t.Error("Harbor listeners did not stop")
			}
		})
		t.Cleanup(stop)
		return stop
	}
	stop := start()
	clientTLS := &tls.Config{ServerName: hostname, RootCAs: roots, MinVersion: tls.VersionTLS12}
	popAddress := addresses["HARBOR_POP3S"]
	first, err := acmeTestDial(popAddress, clientTLS, 25*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	leaf := first.ConnectionState().PeerCertificates[0]
	reader := bufio.NewReader(first)
	if banner, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(banner, "+OK") {
		t.Fatalf("expected POP3 greeting: %q (%v)", banner, err)
	}
	for _, key := range []string{"HARBOR_WEB", "HARBOR_SUBMISSIONS", "HARBOR_IMAPS"} {
		conn, err := acmeTestDial(addresses[key], clientTLS, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		seen := conn.ConnectionState().PeerCertificates[0].SerialNumber
		conn.Close()
		if seen.Cmp(leaf.SerialNumber) != 0 {
			t.Fatalf("%s presented a different certificate", key)
		}
	}
	for _, key := range []string{"HARBOR_SMTP", "HARBOR_SUBMISSION"} {
		conn, err := net.DialTimeout("tcp", addresses[key], 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		client, err := smtp.NewClientStartTLS(conn, clientTLS)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		err = client.Noop()
		state, _ := client.TLSConnectionState()
		client.Close()
		if err != nil || len(state.PeerCertificates) == 0 || state.PeerCertificates[0].SerialNumber.Cmp(leaf.SerialNumber) != 0 {
			t.Fatalf("%s did not present the issued certificate: %v", key, err)
		}
	}
	if useALPN {
		return
	}

	// Wait for the automatic background renewal to replace the cache, without
	// opening new connections that could hide a broken renewal timer.
	var renewed *x509.Certificate
	cacheFile := filepath.Join(dir, "cache", hostname)
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if pem, err := os.ReadFile(cacheFile); err == nil {
			cached, err := tls.X509KeyPair(pem, pem)
			if err == nil && cached.Leaf != nil && cached.Leaf.SerialNumber.Cmp(leaf.SerialNumber) != 0 && cached.Leaf.NotAfter.After(leaf.NotAfter) {
				renewed = cached.Leaf
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if renewed == nil {
		t.Fatal("ACME did not renew the cached certificate")
	}
	verifyRenewed := func() {
		t.Helper()
		conn, err := acmeTestDial(popAddress, clientTLS, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if conn.ConnectionState().PeerCertificates[0].SerialNumber.Cmp(renewed.SerialNumber) != 0 {
			t.Fatal("new POP3S connection did not use the renewed certificate")
		}
	}
	verifyRenewed()
	if _, err := io.WriteString(first, "QUIT\r\n"); err != nil {
		t.Fatal(err)
	}
	if reply, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(reply, "+OK") {
		t.Fatalf("existing POP3S connection did not survive renewal: %q (%v)", reply, err)
	}
	first.Close()
	stop()
	stopPebble()
	start()
	verifyRenewed()
}

func acmeTestDial(address string, config *tls.Config, timeout time.Duration) (*tls.Conn, error) {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := tls.DialWithDialer(&net.Dialer{Deadline: deadline}, "tcp", address, config)
		if err == nil || time.Now().After(deadline) {
			return conn, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func acmeTestAddresses(t *testing.T, keys []string) map[string]string {
	t.Helper()
	addresses := make(map[string]string, len(keys))
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			listener.Close()
		}
	}()
	for _, key := range keys {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		addresses[key] = listener.Addr().String()
	}
	return addresses
}

func startTestPebble(t *testing.T, executable, dir, hostname, address, admin, httpAddress, alpnAddress string) (*http.Client, func()) {
	t.Helper()
	now := time.Now()
	cert, key := makeReloadTestPEM(t, 1, now.Add(-time.Hour), now.Add(time.Hour))
	certPath, keyPath := filepath.Join(dir, "api.pem"), filepath.Join(dir, "api-key.pem")
	configPath := filepath.Join(dir, "pebble.json")
	port := func(address string) int {
		_, raw, _ := net.SplitHostPort(address)
		value, _ := strconv.Atoi(raw)
		return value
	}
	config, err := json.Marshal(map[string]any{"pebble": map[string]any{
		"listenAddress": address, "managementListenAddress": admin, "certificate": certPath, "privateKey": keyPath,
		"httpPort": port(httpAddress), "tlsPort": port(alpnAddress), "keyAlgorithm": "ecdsa",
		"profiles": map[string]any{"default": map[string]int{"validityPeriod": 25}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{certPath: cert, keyPath: key, configPath: config} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	logFile, err := os.Create(filepath.Join(dir, "pebble.log"))
	if err != nil {
		t.Fatal(err)
	}
	dns := acmeTestDNS(t, hostname)
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, executable, "-strict=false", "-config", configPath, "-dnsserver", dns)
	cmd.Env = append(os.Environ(), "PEBBLE_VA_NOSLEEP=1", "PEBBLE_VA_ALWAYS_VALID=0", "PEBBLE_WFE_NONCEREJECT=0", "PEBBLE_AUTHZREUSE=0")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		cancel()
		logFile.Close()
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() {
		cancel()
		_ = cmd.Wait()
		_ = logFile.Close()
	})
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			logs, _ := os.ReadFile(logFile.Name())
			t.Logf("Pebble logs:\n%s", logs)
		}
	})
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	acmeTestGet(t, client, "https://"+address+"/dir")
	return client, stop
}

func acmeTestGet(t *testing.T, client *http.Client, endpoint string) []byte {
	t.Helper()
	var err error
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var response *http.Response
		response, err = client.Get(endpoint)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK || readErr != nil {
				t.Fatalf("Pebble GET %s: HTTP %d (%v)", endpoint, response.StatusCode, readErr)
			}
			return body
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Pebble did not open %s: %v", endpoint, err)
	return nil
}

func acmeTestDNS(t *testing.T, hostname string) string {
	t.Helper()
	server, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	go func() {
		for {
			conn, err := server.Accept()
			if err != nil {
				return
			}
			go replyACMETestDNS(conn, hostname)
		}
	}()
	return server.Addr().String()
}

func replyACMETestDNS(conn net.Conn, hostname string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	var length [2]byte
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		return
	}
	buffer := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(conn, buffer); err != nil {
		return
	}
	var request dnsmessage.Message
	if request.Unpack(buffer) != nil {
		return
	}
	response := dnsmessage.Message{Header: dnsmessage.Header{ID: request.ID, Response: true, Authoritative: true}, Questions: request.Questions}
	for _, question := range request.Questions {
		if question.Name.String() == hostname+"." && question.Type == dnsmessage.TypeA {
			response.Answers = append(response.Answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 1},
				Body:   &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}},
			})
		}
	}
	if payload, err := response.Pack(); err == nil {
		frame := binary.BigEndian.AppendUint16(nil, uint16(len(payload)))
		_, _ = conn.Write(append(frame, payload...))
	}
}
