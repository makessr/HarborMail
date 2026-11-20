package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

type translationTransport func(*http.Request) (*http.Response, error)

func (f translationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestTranslateLetterRequiresOwnershipAndSendsOnlyText(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const owner, other, password = "owner@harbor.test", "other@harbor.test", "synthetic-test-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, domain: "harbor.test", accounts: map[string][]byte{owner: hash, other: hash}}
	const raw = "From: other@harbor.test\r\nTo: owner@harbor.test\r\nMessage-ID: <demo@harbor.test>\r\nReturn-Path: <bounce@harbor.test>\r\nDKIM-Signature: v=1; d=sender.example; s=demo\r\nList-Id: Example list <news.example.test>\r\nList-Unsubscribe: <javascript:unsafe>, <https://example.test/unsubscribe>\r\nContent-Type: multipart/mixed; boundary=letter\r\n\r\n" +
		"--letter\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>First &amp; second</p><script>ignored</script><img src='https://example.test/pixel'><p>Third</p>\r\n" +
		"--letter\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=hidden.txt\r\n\r\nDo not send attachment\r\n--letter--\r\n"
	if err := a.deliverLocal(letter{From: other, To: owner, Subject: "Hello"}, owner, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	var id int
	if err := db.QueryRow("SELECT id FROM letters WHERE owner=?", owner).Scan(&id); err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 1)
	client := &http.Client{
		Transport: translationTransport(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "api.cognitive.microsofttranslator.com" || req.URL.Query().Get("to") != "zh-Hans" ||
				req.Header.Get("Ocp-Apim-Subscription-Key") != "test-key" || req.Header.Get("Ocp-Apim-Subscription-Region") != "test-region" {
				t.Error("unexpected translation endpoint, language or credentials")
			}
			var got []struct{ Text string }
			if err := json.NewDecoder(req.Body).Decode(&got); err != nil || len(got) != 2 ||
				got[0].Text != "Hello" || got[1].Text != "First & second\nThird" {
				t.Errorf("unexpected upstream body: %+v (%v)", got, err)
			}
			select {
			case called <- struct{}{}:
			default:
				t.Error("duplicate translation request")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[{"translations":[{"text":"你好"}]},{"translations":[{"text":"译文"}]}]`))}, nil
		}),
	}
	a.translator = &emailTranslation{providers: map[string]emailTranslator{"microsoft": microsoftEmailTranslator(client, "test-key", "test-region")}, slots: make(chan struct{}, 1)}
	web := httptest.NewServer(a.routes())
	defer web.Close()
	login := func(email string) *http.Client {
		t.Helper()
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar}
		postMailOperation(t, client, web.URL+"/api/login", map[string]string{"email": email, "password": password}, 200)
		return client
	}
	endpoint := web.URL + "/api/letters/" + strconv.Itoa(id) + "/translate"
	otherClient := login(other)
	postMailOperation(t, otherClient, endpoint, map[string]string{"target": "zh-Hans"}, 404)
	ownerClient := login(owner)
	postMailOperation(t, ownerClient, endpoint, map[string]string{"target": "invalid"}, 400)
	if len(called) != 0 {
		t.Fatal("invalid requests reached Microsoft")
	}
	res, err := ownerClient.Post(endpoint, "application/json", strings.NewReader(`{"target":"zh-Hans"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got translatedEmail
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil || res.StatusCode != 200 ||
		res.Header.Get("Cache-Control") != "no-store" || got != (translatedEmail{Subject: "你好", Body: "译文"}) || len(called) != 1 {
		t.Fatalf("unexpected translation response: %d %+v %v", res.StatusCode, got, err)
	}
	postMailOperation(t, ownerClient, endpoint, map[string]string{"target": "zh-Hans", "provider": "microsoft"}, 200)
	postMailOperation(t, otherClient, endpoint, map[string]string{"target": "zh-Hans"}, 404)
	if len(called) != 1 {
		t.Fatal("repeated request called the provider")
	}
	senderEndpoint := web.URL + "/api/letters/" + strconv.Itoa(id) + "/sender-details"
	for _, fixture := range []struct {
		client *http.Client
		status int
	}{{otherClient, 404}, {ownerClient, 200}} {
		res, err := fixture.client.Get(senderEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode == 200 {
			var details letterSenderDetails
			err = json.NewDecoder(res.Body).Decode(&details)
			if err != nil || details != (letterSenderDetails{ReturnPath: "bounce@harbor.test", SigningDomain: "sender.example", MessageID: "<demo@harbor.test>", ListID: "news.example.test", Unsubscribe: "https://example.test/unsubscribe"}) {
				t.Errorf("unexpected sender details: %+v %v", details, err)
			}
		}
		res.Body.Close()
		if res.StatusCode != fixture.status {
			t.Errorf("sender details status: %d, want %d", res.StatusCode, fixture.status)
		}
	}
}

func TestAdditionalEmailTranslationProviders(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, authHeader, authValue, targetField, target, response string
		newProvider                                                          func(*http.Client) emailTranslator
	}{
		{"deepl", deepLFreeURL, "Authorization", "DeepL-Auth-Key fixture:fx", "target_lang", "ZH-HANT",
			`{"translations":[{"text":"譯文"}]}`, func(c *http.Client) emailTranslator { return deepLEmailTranslator(c, "fixture:fx") }},
		{"google", googleTranslationURL, "X-Goog-Api-Key", "fixture", "target", "zh-TW",
			`{"data":{"translations":[{"translatedText":"譯文"}]}}`, func(c *http.Client) emailTranslator { return googleEmailTranslator(c, "fixture") }},
		{"libretranslate", "http://fixture.test/translate", "", "", "target", "zt",
			`{"translatedText":["譯文"]}`, func(c *http.Client) emailTranslator {
				return libreEmailTranslator(c, "http://fixture.test/translate", "")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: translationTransport(func(req *http.Request) (*http.Response, error) {
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil || req.URL.String() != tc.endpoint ||
					req.Header.Get(tc.authHeader) != tc.authValue || body[tc.targetField] != tc.target {
					t.Errorf("unexpected request: %s %+v %v", req.URL, body, err)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.response))}, nil
			})}
			got, status := tc.newProvider(client)(context.Background(), "zh-Hant", []string{"Hello"})
			if status != 200 || len(got) != 1 || got[0] != "譯文" {
				t.Fatalf("unexpected provider response: %d %v", status, got)
			}
		})
	}
}
