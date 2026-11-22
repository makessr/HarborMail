package server

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
	"golang.org/x/crypto/bcrypt"
)

type webPushTestTransport func(*http.Request) (*http.Response, error)

func (f webPushTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWebPushMailboxDelivery(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testWebPushMailboxDelivery(t, "sqlite", filepath.Join(t.TempDir(), "web-push.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testWebPushMailboxDelivery(t, "mysql", dsn) })
	}
}

func testWebPushMailboxDelivery(t *testing.T, driver, source string) {
	ctx := context.Background()
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const password = "synthetic-mailbox-password"
	owner := token()[:16] + "@harbor.test"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, dbDriver: driver, domain: "harbor.test", accounts: map[string][]byte{owner: hash}}
	a.webPush, err = a.loadWebPushKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := a.loadWebPushKeys(ctx)
	if err != nil || keys.publicKey != a.webPush.publicKey {
		t.Fatalf("VAPID key did not persist: %v", err)
	}
	handler := a.routes()
	var cookie *http.Cookie
	request := func(method, path string, body any, want int) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w
	}
	request("GET", "/api/web-push", nil, 401)
	login := func() {
		cookie = request("POST", "/api/login", map[string]string{"email": owner, "password": password}, 200).Result().Cookies()[0]
	}
	login()
	device, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sub := webpush.Subscription{
		Endpoint: "https://fcm.googleapis.com/fcm/send/" + token(),
		Keys: webpush.Keys{
			P256dh: base64.RawURLEncoding.EncodeToString(device.PublicKey().Bytes()),
			Auth:   base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
		},
	}
	invalid := sub
	invalid.Endpoint = "https://fcm.googleapis.com.example.test/fcm/send/test"
	request("PUT", "/api/web-push", invalid, 400)
	request("PUT", "/api/web-push", sub, 204)
	status := request("GET", "/api/web-push", nil, 200)
	if !strings.Contains(status.Body.String(), `"enabled":true`) || strings.Contains(status.Body.String(), a.webPush.privateKey) {
		t.Fatal("unexpected subscription status")
	}
	var sent int
	responseCode := 429
	a.webPush.client.Transport = webPushTestTransport(func(r *http.Request) (*http.Response, error) {
		sent++
		payload, err := io.ReadAll(r.Body)
		if err != nil || len(payload) == 0 || r.URL.String() != sub.Endpoint ||
			r.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid ") {
			t.Fatal("invalid outgoing push request")
		}
		return &http.Response{StatusCode: responseCode, Body: http.NoBody}, nil
	})
	receive := func(subject string) {
		t.Helper()
		delivery := &delivery{app: a, sender: "sender@harbor.test", recipients: []mailDestination{{address: owner, owner: owner}}}
		if err := delivery.Data(strings.NewReader("Subject: " + subject + "\r\n\r\nSynthetic body\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	receive("First")
	receive("Second")
	if !a.deliverWebPush(ctx) || a.deliverWebPush(ctx) || sent != 1 {
		t.Fatal("expected one claim for the two arrivals")
	}
	if _, err := db.Exec(`UPDATE web_push_subscriptions SET due_at=1 WHERE owner=?`, owner); err != nil {
		t.Fatal(err)
	}
	responseCode = 201
	if !a.deliverWebPush(ctx) || a.deliverWebPush(ctx) || sent != 2 {
		t.Fatal("expected one successful retry")
	}
	request("DELETE", "/api/web-push", nil, 204)
	receive("Disabled")
	if a.deliverWebPush(ctx) {
		t.Fatal("disabled device received a push")
	}
	request("PUT", "/api/web-push", sub, 204)
	receive("Expired")
	responseCode = 410
	if !a.deliverWebPush(ctx) || !strings.Contains(request("GET", "/api/web-push", nil, 200).Body.String(), `"enabled":false`) {
		t.Fatal("expired endpoint was not removed")
	}
	request("PUT", "/api/web-push", sub, 204)
	receive("Signed out")
	request("POST", "/api/logout", nil, 204)
	if a.deliverWebPush(ctx) || sent != 3 {
		t.Fatal("signed-out session received a push")
	}
}
