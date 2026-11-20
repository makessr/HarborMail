package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"harbormail/web"

	"golang.org/x/crypto/bcrypt"
)

const cookieName = "harbor_session"
const sessionAge = 7 * 24 * time.Hour

var credentialsFallback, _ = bcrypt.GenerateFromPassword([]byte("invalid credentials"), bcrypt.DefaultCost)
var loginSlots = make(chan struct{}, 4)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func Run() {
	if len(os.Args) > 1 && os.Args[1] == "dns-check" {
		if err := runDomainDNSCheck(os.Stdout, net.DefaultResolver, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "mysql-check" {
		if len(os.Args) != 2 {
			log.Fatal("usage: harbormail mysql-check")
		}
		if err := runMySQLCheck(os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "acme-check" {
		if len(os.Args) != 2 {
			log.Fatal("usage: harbormail acme-check")
		}
		if err := runACMECheck(os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "relay-check" {
		if err := runRelayCheck(os.Stdout, os.Args[2:]...); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := webHealthcheck(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "generate-admin-mfa" {
		if err := runAdminMFAEnrollment(os.Stdout, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate-sqlite" {
		if err := runSQLiteMigration(os.Args[2:], os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "hash-password" {
		password, err := bufio.NewReader(io.LimitReader(os.Stdin, 75)).ReadString('\n')
		if err != nil && err != io.EOF {
			log.Fatal(err)
		}
		password = strings.TrimRight(password, "\r\n")
		if len(password) < 12 || len(password) > 72 {
			log.Fatal("password must be 12–72 bytes")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(string(hash))
		return
	}
	if dir := os.Getenv("HARBOR_CHECK_DATA_DIR"); dir != "" {
		if err := checkDataDir(dir); err != nil {
			log.Fatal("HARBOR_CHECK_DATA_DIR is not writable: ", err)
		}
	}
	domain := strings.ToLower(env("HARBOR_DOMAIN", "harbor.test"))
	if !regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`).MatchString(domain) {
		log.Fatal("invalid HARBOR_DOMAIN")
	}
	var configured map[string]string
	accountJSON, err := fileSetting("HARBOR_ACCOUNTS")
	if err != nil {
		log.Fatal(err)
	}
	if err := json.Unmarshal([]byte(accountJSON), &configured); err != nil || len(configured) == 0 {
		log.Fatal("set HARBOR_ACCOUNTS or HARBOR_ACCOUNTS_FILE to account JSON mapping email addresses to bcrypt hashes")
	}
	a := &app{accounts: make(map[string][]byte), domain: domain}
	switch os.Getenv("HARBOR_RECOVERY_MODE") {
	case "true":
		a.recoveryMode = true
		if os.Getenv("HARBOR_ACME_HOSTS") != "" {
			log.Fatal("HARBOR_RECOVERY_MODE requires automatic ACME certificates to be disabled")
		}
	case "", "false":
	default:
		log.Fatal("HARBOR_RECOVERY_MODE must be true or false")
	}
	switch os.Getenv("HARBOR_PAUSE_OUTBOX") {
	case "true":
		a.pauseOutbox = true
	case "", "false":
	default:
		log.Fatal("HARBOR_PAUSE_OUTBOX must be true or false")
	}
	if err := a.loadRelays(); err != nil {
		log.Fatal(err)
	}
	a.webLogin.proxies, err = parseTrustedProxies(os.Getenv("HARBOR_TRUSTED_PROXIES"))
	if err != nil {
		log.Fatal(err)
	}
	a.spamScanner, err = newRspamdScanner(os.Getenv("HARBOR_RSPAMD_URL"))
	if err != nil {
		log.Fatal(err)
	}
	for key, hash := range configured {
		email, err := address(key)
		_, hashErr := bcrypt.Cost([]byte(hash))
		if err != nil || hashErr != nil || email != key || email != strings.ToLower(email) {
			log.Fatal("invalid account address or bcrypt hash")
		}
		a.accounts[email] = []byte(hash)
	}
	admins, err := fileSetting("HARBOR_ADMINS")
	if err != nil {
		log.Fatal(err)
	}
	a.admins, err = parseAdmins(admins, a.accounts)
	if err != nil {
		log.Fatal(err)
	}
	domainAdmins, err := fileSetting("HARBOR_DOMAIN_ADMINS")
	if err != nil {
		log.Fatal(err)
	}
	a.domainAdmins, err = parseDomainAdmins(domainAdmins, a.accounts)
	if err != nil {
		log.Fatal(err)
	}
	domainDefaults, err := fileSetting("HARBOR_DOMAIN_DEFAULT_QUOTAS")
	if err != nil {
		log.Fatal(err)
	}
	a.domainQuota, err = parseDomainQuotaDefaults(domainDefaults)
	if err != nil {
		log.Fatal(err)
	}
	mfaJSON, err := fileSetting("HARBOR_ADMIN_MFA")
	if err != nil {
		log.Fatal(err)
	}
	privileged := make(map[string]bool, len(a.admins)+len(a.domainAdmins))
	for owner := range a.admins {
		privileged[owner] = true
	}
	for owner := range a.domainAdmins {
		privileged[owner] = true
	}
	a.adminMFA, err = parseAdminMFA(mfaJSON, privileged)
	if err != nil {
		log.Fatal(err)
	}
	aliasJSON, err := fileSetting("HARBOR_ALIASES")
	if err != nil {
		log.Fatal(err)
	}
	a.aliases, err = parseAliases(aliasJSON, a.accounts)
	if err != nil {
		log.Fatal(err)
	}
	driver := env("HARBOR_DB_DRIVER", "sqlite")
	a.dbDriver = driver
	a.sharedMailboxEvents = driver == "mysql"
	dbSource := env("HARBOR_DB", "data/harbor.db")
	if driver == "mysql" {
		dbSource, err = fileSetting("HARBOR_MYSQL_DSN")
		if err != nil {
			log.Fatal(err)
		}
	}
	a.db, err = openDB(driver, dbSource)
	if err != nil {
		log.Fatal(err)
	}
	defer a.db.Close()
	if driver == "mysql" {
		limiter := &mysqlLoginLimiter{db: a.db}
		a.webLogin.shared, a.webLogin.scope = limiter, "web"
		a.clientLogin.shared, a.clientLogin.scope = limiter, "protocol"
	}
	redisURL, err := fileSetting("HARBOR_REDIS_URL")
	if err != nil {
		log.Fatal(err)
	}
	a.adminCache, err = openAdminRedisCache(redisURL, os.Getenv("HARBOR_REDIS_NAMESPACE"), driver)
	if err != nil {
		log.Fatal(err)
	}
	if a.adminCache != nil {
		defer a.adminCache.client.Close()
	}
	a.hardQuota, err = initHardQuota(a.db, driver, os.Getenv("HARBOR_HARD_QUOTA") == "true")
	if err != nil {
		log.Fatal("initialize mailbox quota: ", err)
	}
	if len(a.domainQuota) > 0 && !a.hardQuota {
		log.Fatal("HARBOR_DOMAIN_DEFAULT_QUOTAS requires mailbox hard quotas")
	}
	if os.Getenv("HARBOR_CONTENT_INDEX") == "true" {
		a.contentIndex, err = initContentIndex(a.db, driver)
		if err != nil {
			log.Fatal("initialize mail content index: ", err)
		}
	}
	if err := a.registerConfiguredAccounts(); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = a.removeStaleAdminSessions(ctx, privileged)
	cancel()
	if err != nil {
		log.Fatal("invalidate outdated administrator sessions: ", err)
	}
	if !a.recoveryMode && strings.TrimSpace(os.Getenv("HARBOR_RSPAMD_TRAIN_URL")) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		names, err := a.allAccountNames(ctx)
		cancel()
		if err != nil {
			log.Fatal(err)
		}
		a.spamTrainer, err = configureSpamTrainer(names)
		if err != nil {
			log.Fatal(err)
		}
	}
	transportTLS, err := configureTLS()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.serve(ctx, transportTLS); err != nil {
		log.Fatal(err)
	}
}

func (a *app) serve(ctx context.Context, transportTLS inboundTLS) error {
	tlsConfig, managed := transportTLS.config, transportTLS.acme
	runtime := newServiceRuntime()
	if transportTLS.dns != nil {
		defer transportTLS.dns.cache.Stop()
	}
	defer runtime.shutdown()
	a.stopping = runtime.ctx.Done()
	if err := a.listenMetrics(runtime, os.Getenv("HARBOR_METRICS")); err != nil {
		return err
	}
	if managed != nil {
		if err := managed.listenChallenge(runtime); err != nil {
			return err
		}
	}
	if transportTLS.dns != nil {
		if err := transportTLS.dns.start(runtime.ctx); err != nil {
			return err
		}
	}
	webTLS := tlsConfig != nil && os.Getenv("HARBOR_WEB_TLS") != "off"
	a.secure = webTLS || os.Getenv("HARBOR_SECURE_COOKIE") == "true"
	listen := env("HARBOR_WEB", "127.0.0.1:8080")
	server := &http.Server{
		Addr: listen, Handler: a.routes(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	webListener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	if webTLS {
		server.TLSConfig = tlsConfig.Clone()
		if managed != nil {
			server.TLSConfig.GetCertificate = managed.certificate
			server.TLSConfig.NextProtos = managed.manager.TLSConfig().NextProtos
		}
	}
	if a.recoveryMode {
		log.Print("Recovery mode: mail listeners and background mail jobs are disabled")
	} else if err := a.startMailServices(runtime, tlsConfig); err != nil {
		_ = webListener.Close()
		return err
	}
	log.Printf("Webmail listening on %s", webListener.Addr())
	runtime.serveHTTP("webmail", server, webListener)
	if transportTLS.pem != nil {
		runtime.worker(transportTLS.pem.watch)
	}
	if managed != nil {
		managed.warmCertificates(runtime.ctx)
	}
	select {
	case <-ctx.Done():
		log.Print("Stopping Harbor Mail")
		return nil
	case err := <-runtime.failures:
		return err
	}
}

func (a *app) startMailServices(runtime *serviceRuntime, tlsConfig *tls.Config) error {
	if err := a.listenSubmission(runtime, os.Getenv("HARBOR_SUBMISSION"), tlsConfig, false); err != nil {
		return err
	}
	if err := a.listenSubmission(runtime, os.Getenv("HARBOR_SUBMISSIONS"), tlsConfig, true); err != nil {
		return err
	}
	if err := a.listenPOP3S(runtime, os.Getenv("HARBOR_POP3S"), tlsConfig); err != nil {
		return err
	}
	if err := a.listenIMAPS(runtime, os.Getenv("HARBOR_IMAPS"), tlsConfig); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", env("HARBOR_SMTP", "127.0.0.1:2525"))
	if err != nil {
		return err
	}
	log.Printf("SMTP listening on %s", listener.Addr())
	runtime.serveSMTP("SMTP", a.smtpServer(a, tlsConfig), listener)
	if a.pauseOutbox {
		log.Print("Outbox is paused: queued mail will not be processed")
	} else {
		runtime.worker(func(ctx context.Context) { a.runOutbox(ctx, a.connectSenderRelay) })
	}
	if a.spamTrainer != nil {
		runtime.worker(a.runSpamFeedback)
	}
	runtime.worker(a.runSnoozes)
	runtime.worker(func(ctx context.Context) {
		if err := a.backfillThreads(ctx); err != nil && ctx.Err() == nil {
			log.Print("index existing mail conversations: ", err)
		}
	})
	return nil
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != "application/json" {
		fail(w, 415, "请求格式必须为 JSON")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	if err := json.NewDecoder(r.Body).Decode(value); err != nil {
		fail(w, 400, "请求内容无效或过大")
		return false
	}
	return true
}

func (a *app) cookie(w http.ResponseWriter, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true,
		Secure: a.secure, SameSite: http.SameSiteStrictMode, MaxAge: age})
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	source := loginSource(r, a.webLogin.proxies)
	if retry := a.webLogin.reserve(r.Context(), source, "", false); retry > 0 {
		a.rejectWebLoginRate(w, source, "", retry)
		return
	}
	var input struct{ Email, Password, Code string }
	if !decode(w, r, &input) {
		a.webLogin.record(source, "", "failed")
		return
	}
	email := strings.ToLower(strings.TrimSpace(input.Email))
	if len(email) > 254 {
		email = ""
	}
	if retry := a.webLogin.reserve(r.Context(), source, email, true); retry > 0 {
		a.rejectWebLoginRate(w, source, "", retry)
		return
	}
	owner, verifiedHash, err := a.authenticatePrimary(r.Context(), input.Email, input.Password)
	if errors.Is(err, errLoginBusy) {
		a.webLogin.record(source, "", "busy")
		w.Header().Set("Retry-After", "1")
		fail(w, 429, "登录请求过多，请稍后重试")
		return
	}
	if err != nil {
		a.webLogin.record(source, owner, "failed")
		fail(w, 401, "邮箱或密码不正确")
		return
	}
	value := token()
	digest := sha256.Sum256([]byte(value))
	if err := a.createBrowserSession(r, owner, digest[:], verifiedHash, input.Code); err != nil {
		if errors.Is(err, errAdminMFA) {
			a.webLogin.record(source, owner, "failed")
			fail(w, http.StatusPreconditionRequired, "请输入有效的管理员验证码或未使用的恢复码")
			return
		}
		if errors.Is(err, errAccountSuspended) || errors.Is(err, errPrimaryPasswordChanged) {
			a.webLogin.record(source, owner, "failed")
			fail(w, 401, "邮箱或密码不正确")
			return
		}
		a.webLogin.record(source, owner, "error")
		fail(w, 500, "无法创建登录会话")
		return
	}
	a.webLogin.record(source, owner, "success")
	a.cookie(w, value, int(sessionAge.Seconds()))
	a.replySession(w, owner)
}

var (
	errLoginBusy    = errors.New("too many concurrent login attempts")
	errLoginInvalid = errors.New("invalid credentials")
)

func (a *app) authenticate(email, password string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	owner, _, err := a.authenticatePrimary(ctx, email, password)
	return owner, err
}

func (a *app) authenticatePrimary(ctx context.Context, email, password string) (string, []byte, error) {
	select {
	case loginSlots <- struct{}{}:
		defer func() { <-loginSlots }()
	default:
		return "", nil, errLoginBusy
	}
	owner := strings.ToLower(strings.TrimSpace(email))
	hash, err := a.primaryPasswordHash(ctx, a.db, owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", nil, errLoginBusy
	}
	if errors.Is(err, sql.ErrNoRows) {
		hash = credentialsFallback
	}
	if len(password) > 72 || bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || err != nil {
		if err == nil {
			return owner, nil, errLoginInvalid
		}
		return "", nil, errLoginInvalid
	}
	return owner, hash, nil
}

type authorized func(http.ResponseWriter, *http.Request, string)

func (a *app) auth(handler authorized) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := a.browserSession(r)
		if err != nil {
			fail(w, 500, "无法读取登录会话")
			return
		}
		if session.owner == "" {
			fail(w, 401, "请登录邮箱")
			return
		}
		a.touchBrowserSession(r, session)
		handler(w, r, session.owner)
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.Handle("GET /", http.FileServer(http.FS(web.Assets)))
	mux.HandleFunc("GET /admin", a.adminPage)
	mux.HandleFunc("GET /admin/{$}", a.adminPage)
	mux.HandleFunc("GET /admin/domains", a.adminPage)
	mux.HandleFunc("GET /admin/accounts", a.adminPage)
	mux.HandleFunc("GET /admin/queue", a.adminPage)
	mux.HandleFunc("GET /admin/audit", a.adminPage)
	mux.HandleFunc("GET /admin/aliases", a.adminPage)
	mux.HandleFunc("GET /admin/runtime", a.adminPage)
	mux.HandleFunc("GET /admin/storage", a.adminPage)
	mux.HandleFunc("GET /admin/security", a.adminPage)
	mux.HandleFunc("GET /api/admin/overview", a.adminOnly(a.adminOverview))
	mux.HandleFunc("GET /api/admin/accounts", a.domainAdminOnly(a.adminAccountList))
	mux.HandleFunc("POST /api/admin/accounts", a.domainAdminOnly(a.adminCreateAccount))
	mux.HandleFunc("GET /api/admin/accounts/{email}/access", a.adminOnly(a.adminAccountAccess))
	mux.HandleFunc("GET /api/admin/accounts/{email}/storage", a.adminOnly(a.adminAccountStorage))
	mux.HandleFunc("PUT /api/admin/accounts/{email}/storage-alert", a.adminOnly(a.adminSetStorageAlert))
	mux.HandleFunc("PUT /api/admin/accounts/{email}/storage-quota", a.adminOnly(a.adminSetHardQuota))
	mux.HandleFunc("POST /api/admin/accounts/{email}/revoke", a.adminOnly(a.adminChangeAccountAccess))
	mux.HandleFunc("POST /api/admin/accounts/{email}/access", a.adminOnly(a.adminChangeAccountAccess))
	mux.HandleFunc("POST /api/admin/accounts/{email}/password", a.adminOnly(a.adminResetPassword))
	mux.HandleFunc("POST /api/admin/accounts/{email}/lifecycle", a.adminOnly(a.adminMailboxLifecycle))
	mux.HandleFunc("GET /api/admin/domains/{domain}/dns", a.domainAdminOnly(a.adminDomainDNS))
	mux.HandleFunc("POST /api/admin/domains", a.adminOnly(a.adminChangeDomain))
	mux.HandleFunc("GET /api/admin/queue", a.adminOnly(a.adminQueue))
	mux.HandleFunc("POST /api/admin/queue/{id}/retry", a.adminOnly(a.adminRetryDelivery))
	mux.HandleFunc("POST /api/admin/queue/{id}/resolve", a.adminOnly(a.adminResolveDelivery))
	mux.HandleFunc("GET /api/admin/audit", a.adminOnly(a.adminAudit))
	mux.HandleFunc("GET /api/admin/audit/export", a.adminOnly(a.adminAuditExport))
	mux.HandleFunc("GET /api/admin/aliases", a.domainAdminOnly(a.adminAliasList))
	mux.HandleFunc("POST /api/admin/aliases", a.domainAdminOnly(a.adminAliasChange))
	mux.HandleFunc("GET /api/admin/runtime", a.adminOnly(a.adminRuntime))
	mux.HandleFunc("GET /api/admin/security", a.adminOnly(a.adminSecurity))
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("GET /api/session", a.auth(func(w http.ResponseWriter, r *http.Request, owner string) {
		a.replySession(w, owner)
	}))
	mux.HandleFunc("POST /api/logout", a.auth(a.logout))
	mux.HandleFunc("GET /api/events", a.auth(a.streamMailboxEvents))
	mux.HandleFunc("GET /api/web-sessions", a.auth(a.listBrowserSessions))
	mux.HandleFunc("DELETE /api/web-sessions", a.auth(a.revokeOtherBrowserSessions))
	mux.HandleFunc("DELETE /api/web-sessions/{id}", a.auth(a.revokeBrowserSession))
	mux.HandleFunc("GET /api/app-passwords", a.auth(a.listAppPasswords))
	mux.HandleFunc("POST /api/app-passwords", a.auth(a.createAppPassword))
	mux.HandleFunc("DELETE /api/app-passwords/{id}", a.auth(a.revokeAppPassword))
	mux.HandleFunc("GET /api/addresses", a.auth(a.addressesHTTP))
	mux.HandleFunc("GET /api/contacts", a.auth(a.listContacts))
	mux.HandleFunc("POST /api/contacts", a.auth(a.saveContact))
	mux.HandleFunc("PUT /api/contacts/{id}", a.auth(a.saveContact))
	mux.HandleFunc("DELETE /api/contacts/{id}", a.auth(a.deleteContact))
	mux.HandleFunc("GET /api/signatures", a.auth(a.getSignatures))
	mux.HandleFunc("PUT /api/signatures", a.auth(a.putSignatures))
	mux.HandleFunc("GET /api/templates", a.auth(a.getMailTemplates))
	mux.HandleFunc("PUT /api/templates", a.auth(a.putMailTemplates))
	mux.HandleFunc("GET /api/preferences", a.auth(a.getMailboxPreferences))
	mux.HandleFunc("PUT /api/preferences", a.auth(a.putMailboxPreferences))
	mux.HandleFunc("GET /api/saved-searches", a.auth(a.listSavedSearches))
	mux.HandleFunc("POST /api/saved-searches", a.auth(a.saveSearch))
	mux.HandleFunc("PUT /api/saved-searches/{id}", a.auth(a.saveSearch))
	mux.HandleFunc("DELETE /api/saved-searches/{id}", a.auth(a.deleteSavedSearch))
	mux.HandleFunc("GET /api/labels", a.auth(a.listMailLabels))
	mux.HandleFunc("POST /api/labels", a.auth(a.saveMailLabel))
	mux.HandleFunc("PUT /api/labels/{label}", a.auth(a.saveMailLabel))
	mux.HandleFunc("DELETE /api/labels/{label}", a.auth(a.deleteMailLabel))
	mux.HandleFunc("GET /api/letters/{id}/labels", a.auth(a.listLetterLabels))
	mux.HandleFunc("PUT /api/letters/{id}/labels/{label}", a.auth(a.changeMailLabel))
	mux.HandleFunc("DELETE /api/letters/{id}/labels/{label}", a.auth(a.changeMailLabel))
	mux.HandleFunc("GET /api/letters", a.auth(a.list))
	mux.HandleFunc("POST /api/mail-import", a.auth(a.importMail))
	mux.HandleFunc("POST /api/mail-export", a.auth(a.exportMail))
	mux.HandleFunc("PATCH /api/letters", a.auth(a.updateBatch))
	mux.HandleFunc("GET /api/letters/{id}", a.auth(a.read))
	mux.HandleFunc("GET /api/letters/{id}/thread", a.auth(a.getThread))
	mux.HandleFunc("PATCH /api/letters/{id}", a.auth(a.update))
	mux.HandleFunc("DELETE /api/letters/{id}", a.auth(a.deleteTrashedMail))
	mux.HandleFunc("PUT /api/letters/{id}/snooze", a.auth(a.setSnooze))
	mux.HandleFunc("GET /api/letters/{id}/raw", a.auth(a.download))
	mux.HandleFunc("GET /api/letters/{id}/html", a.auth(a.htmlLetter))
	mux.HandleFunc("GET /api/letters/{id}/attachments/{part}", a.auth(a.downloadAttachment))
	mux.HandleFunc("POST /api/letters", a.auth(a.send))
	mux.HandleFunc("GET /api/outbox", a.auth(a.listOutbox))
	mux.HandleFunc("GET /api/outbox/{id}", a.auth(a.readOutbox))
	mux.HandleFunc("GET /api/outbox/{id}/raw", a.auth(a.downloadOutbox))
	mux.HandleFunc("GET /api/outbox/{id}/recipients", a.auth(a.outboxRecipientProgress))
	mux.HandleFunc("POST /api/outbox/{id}/recipient-result", a.auth(a.resolveOutboxRecipient))
	mux.HandleFunc("POST /api/outbox/{id}/cancel", a.auth(a.cancelScheduled))
	mux.HandleFunc("POST /api/drafts", a.auth(a.createDraft))
	mux.HandleFunc("PUT /api/drafts/{id}", a.auth(a.editDraft))
	mux.HandleFunc("POST /api/drafts/{id}/send", a.auth(a.sendDraft))
	mux.HandleFunc("GET /api/folders", a.auth(a.foldersHTTP))
	mux.HandleFunc("POST /api/folders", a.auth(a.saveFolderHTTP))
	mux.HandleFunc("PUT /api/folders/{folder}", a.auth(a.saveFolderHTTP))
	mux.HandleFunc("DELETE /api/folders/{folder}", a.auth(a.deleteFolderHTTP))
	mux.HandleFunc("GET /api/rules", a.auth(a.listRulesHTTP))
	mux.HandleFunc("POST /api/rules", a.auth(a.saveRuleHTTP))
	mux.HandleFunc("PUT /api/rules/{id}", a.auth(a.saveRuleHTTP))
	mux.HandleFunc("DELETE /api/rules/{id}", a.auth(a.deleteRuleHTTP))
	protected := http.NewCrossOriginProtection().Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if a.recoveryMode && r.Method != http.MethodGet && r.Method != http.MethodHead &&
			!(r.Method == http.MethodPost && (r.URL.Path == "/api/login" || r.URL.Path == "/api/logout")) {
			fail(w, http.StatusServiceUnavailable, "恢复校验模式下无法执行此操作")
			return
		}
		protected.ServeHTTP(w, r)
	})
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store")
	if err := a.db.PingContext(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (a *app) list(w http.ResponseWriter, r *http.Request, owner string) {
	folder := r.URL.Query().Get("folder")
	query := `SELECT id,sender,coalesce(e.to_list,recipient),subject,substr(body,1,180),stamp,folder,seen,star,
		coalesce(s.resume_at,0),coalesce(e.cc_list,''),coalesce(e.bcc_list,'')
		FROM letters LEFT JOIN mail_snoozes s ON s.letter_id=letters.id
		LEFT JOIN web_letter_recipients e ON e.letter_id=letters.id WHERE owner=?`
	args := []any{owner}
	switch folder {
	case "all":
		query += ` AND folder NOT IN ('drafts','trash','junk')`
	case "starred":
		query += ` AND star=1 AND folder NOT IN ('trash','junk')`
	case "sent", "archive", "trash", "drafts", "junk", "snoozed":
		query += ` AND folder=?`
		args = append(args, folder)
	default:
		if customFolderID.MatchString(folder) {
			if folderHTTPError(w, verifyFolder(r.Context(), a.db, owner, folder)) {
				return
			}
			query += ` AND folder=?`
			args = append(args, folder)
		} else {
			query += ` AND folder='inbox'`
		}
	}
	if raw := r.URL.Query().Get("label"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			fail(w, 400, "请选择有效的标签")
			return
		}
		query += ` AND EXISTS (SELECT 1 FROM mail_label_links ml JOIN mail_labels t ON t.id=ml.label_id
			WHERE ml.letter_id=letters.id AND t.owner=letters.owner AND t.id=?)`
		args = append(args, id)
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		if r.URL.Query().Get("content") == "1" {
			contentFilter, contentArgs := a.mailContentQuery(q)
			query += contentFilter
			args = append(args, contentArgs...)
		} else {
			query += ` AND instr(lower(concat(sender,' ',coalesce(e.to_list,recipient),' ',coalesce(e.cc_list,''),
				' ',coalesce(e.bcc_list,''),' ',subject,' ',body)),lower(?))>0`
			args = append(args, q)
		}
	}
	filters, values, valid := letterListFilters(r.URL.Query())
	if !valid {
		fail(w, 400, "请检查日期范围和已读状态")
		return
	}
	query += filters
	args = append(args, values...)
	if before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64); before > 0 {
		query += ` AND id<?`
		args = append(args, before)
	}
	rows, err := a.db.QueryContext(r.Context(), query+` ORDER BY id DESC LIMIT 51`, args...)
	if err != nil {
		fail(w, 500, "无法读取邮件")
		return
	}
	defer rows.Close()
	result := make([]letter, 0, 51)
	for rows.Next() {
		var m letter
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Subject, &m.Body, &m.Date, &m.Folder, &m.Read, &m.Star,
			&m.SnoozeUntil, &m.Cc, &m.Bcc); err != nil {
			fail(w, 500, "无法读取邮件")
			return
		}
		result = append(result, m)
	}
	if rows.Err() != nil {
		fail(w, 500, "无法读取邮件")
		return
	}
	var next int64
	if len(result) == 51 {
		result = result[:50]
		next = result[49].ID
	}
	reply(w, 200, map[string]any{"letters": result, "next": next})
}

func (a *app) read(w http.ResponseWriter, r *http.Request, owner string) {
	var m letter
	var raw []byte
	var to sql.NullString
	err := a.db.QueryRow(`SELECT l.id,l.sender,l.recipient,l.subject,l.body,l.stamp,l.folder,l.seen,l.star,l.raw,
		CASE WHEN l.folder='drafts' THEN coalesce(d.revision,0) ELSE 0 END,coalesce(s.resume_at,0),
		e.to_list,coalesce(e.cc_list,''),coalesce(e.bcc_list,'')
		FROM letters l LEFT JOIN web_drafts d ON d.letter_id=l.id
		LEFT JOIN mail_snoozes s ON s.letter_id=l.id LEFT JOIN web_letter_recipients e ON e.letter_id=l.id
		WHERE l.owner=? AND l.id=?`, owner, letterID(r)).Scan(&m.ID, &m.From, &m.To, &m.Subject, &m.Body, &m.Date,
		&m.Folder, &m.Read, &m.Star, &raw, &m.DraftVersion, &m.SnoozeUntil, &to, &m.Cc, &m.Bcc)
	if letterError(w, err) {
		return
	}
	recipients := visibleRecipients(raw)
	if to.Valid {
		m.To = to.String
	} else {
		m.Cc = recipients.Cc
	}
	m.Labels, err = a.mailLabels(r.Context(), owner, m.ID)
	if labelHTTPError(w, err) {
		return
	}
	visitAttachments(raw, func(info attachmentInfo, _ io.Reader) bool {
		m.Attachments = append(m.Attachments, info)
		return true
	})
	view := inspectHTMLMail(raw, false)
	m.HasHTML, m.HasText = view.hasHTML, view.hasText
	m.ReplyAddress = replyAddress(raw, m.From)
	m.SenderAddress, m.SenderName = senderContact(m.From)
	reply(w, 200, struct {
		letter
		HeaderRecipients messageHeaderRecipients `json:"headerRecipients"`
	}{m, recipients})
}

func letterID(r *http.Request) int64 {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0
	}
	return id
}

func letterError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "找不到这封邮件")
	} else if isQuotaExceeded(err) {
		fail(w, http.StatusInsufficientStorage, "邮箱存储已达到硬配额，请释放空间后重试")
	} else if err != nil {
		fail(w, 500, "邮件操作失败")
	}
	return err != nil
}

func (a *app) rawLetter(owner string, id int64) ([]byte, error) {
	var raw []byte
	err := a.db.QueryRow(`SELECT raw FROM letters WHERE owner=? AND id=?`, owner, id).Scan(&raw)
	return raw, err
}

func (a *app) download(w http.ResponseWriter, r *http.Request, owner string) {
	id := letterID(r)
	raw, err := a.rawLetter(owner, id)
	if letterError(w, err) {
		return
	}
	downloadEML(w, id, raw)
}

func downloadEML(w http.ResponseWriter, id int64, raw []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="letter-`+strconv.FormatInt(id, 10)+`.eml"`)
	_, _ = w.Write(raw)
}

func (a *app) send(w http.ResponseWriter, r *http.Request, owner string) {
	plan, valid := parseSendPlan(w, r)
	if !valid {
		return
	}
	var input outgoingLetter
	if !decodeOutgoing(w, r, &input) {
		return
	}
	if strings.ContainsAny(input.Subject, "\r\n") || len(input.Subject) > 300 ||
		strings.TrimSpace(input.Body) == "" || len(input.Body) > 256<<10 {
		fail(w, 400, "请检查主题和正文")
		return
	}
	envelope, valid := a.webDeliveryEnvelope(w, r.Context(), a.db, owner, input.recipientFields())
	if !valid {
		return
	}
	m := letter{From: owner, To: envelope.Recipients.indexedAddress(), Subject: strings.TrimSpace(input.Subject),
		Body: input.Body, Date: time.Now().Unix(), recipients: &envelope.Recipients}
	headers, err := prepareReply(r.Context(), a.db, owner, input.ReplyToID)
	if letterError(w, err) {
		return
	}
	m.reply = headers
	if plan.at != 0 {
		m.Date = plan.at
	}
	raw, err := encodeLetter(m, a.domain, input.files)
	if letterError(w, err) {
		return
	}
	if len(raw) > mailLimit {
		fail(w, 413, "编码后的邮件超出 10 MiB，请减少附件或正文")
		return
	}
	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) {
		return
	}
	a.submitWebMail(w, ctx, tx, m, raw, envelope, plan)
}

func (a *app) deliverLocal(m letter, owner string, raw []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	if err := a.checkDestination(ctx, tx, m.To, owner); err != nil {
		return err
	}
	if err := localDelivery(ctx, tx, m, owner, raw); err != nil {
		return err
	}
	return a.commitMailbox(tx, m.From, owner)
}

func localDelivery(ctx context.Context, tx *sql.Tx, m letter, owner string, raw []byte) error {
	if err := insert(tx, m.From, m, "sent", raw); err != nil {
		return err
	}
	return insertIncoming(ctx, tx, owner, m.From, "inbox", m, raw)
}
