package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestLoginAudit(t *testing.T) {
	for _, driver := range []string{"sqlite", "mysql"} {
		t.Run(driver, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "audit.db")
			if driver == "mysql" {
				source = os.Getenv("HARBOR_TEST_MYSQL_DSN")
				if source == "" {
					t.Skip("set HARBOR_TEST_MYSQL_DSN to a disposable MySQL database")
				}
			}
			writer, err := openDB(driver, source)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			owner := "audit-" + token() + "@harbor.test"
			ip := netip.MustParsePrefix("2001:db8:1::/64")
			now := time.Now().Unix()
			_, err = writer.Exec(`INSERT INTO login_audit_events(stamp,source,account,protocol,result)
				VALUES(?,?,?,?,?)`, now-48*3600, ip.String(), owner, "imap", "failed")
			if err != nil {
				t.Fatal(err)
			}
			producer := &app{db: writer}
			for range loginAuditPage + 1 {
				producer.recordLogin(ip, owner, "failed", "imap")
			}
			producer.recordLogin(ip, owner, "success", "smtp")

			reader, err := openDB(driver, source)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			hash, err := bcrypt.GenerateFromPassword([]byte("audit-password"), bcrypt.MinCost)
			if err != nil {
				t.Fatal(err)
			}
			admin := "audit-admin-" + token() + "@harbor.test"
			a := &app{db: reader, dbDriver: driver, auditDays: 1, accounts: map[string][]byte{admin: hash, owner: hash},
				admins: map[string]bool{admin: true}}
			router := a.routes()
			login := func(account string) *http.Cookie {
				t.Helper()
				response := httptest.NewRecorder()
				body := `{"email":"` + account + `","password":"audit-password"}`
				request := httptest.NewRequest("POST", "/api/login", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("login returned %d: %s", response.Code, response.Body)
				}
				return response.Result().Cookies()[0]
			}
			adminCookie, ownerCookie := login(admin), login(owner)
			get := func(query string, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
				t.Helper()
				request := httptest.NewRequest("GET", "/api/admin/security?"+query, nil)
				if cookie != nil {
					request.AddCookie(cookie)
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != want {
					t.Fatalf("audit returned %d, want %d: %s", response.Code, want, response.Body)
				}
				return response
			}
			get("", nil, http.StatusUnauthorized)
			get("", ownerCookie, http.StatusForbidden)
			get("protocol=invalid", adminCookie, http.StatusBadRequest)
			query := "account=" + owner + "&protocol=imap&result=failed&from=" + strconv.FormatInt(now-60, 10)
			type report struct {
				Events  []loginEvent
				HasMore bool
			}
			var first, second report
			response := get(query, adminCookie, http.StatusOK)
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("audit response may be cached")
			}
			if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
				t.Fatal(err)
			}
			if len(first.Events) != loginAuditPage || !first.HasMore || first.Events[0].Account != owner ||
				first.Events[0].Source != ip.String() || first.Events[0].Protocol != "imap" {
				t.Fatalf("wrong first page: %d rows, more=%t", len(first.Events), first.HasMore)
			}
			cursor := first.Events[len(first.Events)-1].ID
			response = get(query+"&before="+strconv.FormatInt(cursor, 10), adminCookie, http.StatusOK)
			if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
				t.Fatal(err)
			}
			if len(second.Events) != 1 || second.Events[0].ID >= cursor || second.HasMore {
				t.Fatalf("wrong second page: %+v", second)
			}
			var expired report
			response = get("account="+owner+"&until="+strconv.FormatInt(now-3600, 10), adminCookie, http.StatusOK)
			if err := json.Unmarshal(response.Body.Bytes(), &expired); err != nil || len(expired.Events) != 0 {
				t.Fatalf("expired audit events served: %+v, %v", expired, err)
			}
			if err := a.pruneLoginAudit(context.Background(), now-86400); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := reader.QueryRow(`SELECT COUNT(*) FROM login_audit_events WHERE account=? AND stamp<?`, owner, now-86400).
				Scan(&count); err != nil || count != 0 {
				t.Fatalf("expired rows after sweep: %d, %v", count, err)
			}
			response = get("account="+owner+"&protocol=web&result=success", adminCookie, http.StatusOK)
			if !strings.Contains(response.Body.String(), `"protocol":"web"`) ||
				strings.Contains(response.Body.String(), "audit-password") {
				t.Fatal("unexpected fields in Web login audit")
			}
		})
	}
}
