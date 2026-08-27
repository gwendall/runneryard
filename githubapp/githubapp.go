// Package githubapp bootstraps and verifies the dedicated GitHub App used by
// a RunnerYard controller. The package never logs or formats private keys.
package githubapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	jwt "github.com/golang-jwt/jwt/v4"
)

const (
	defaultAPIURL  = "https://api.github.com"
	defaultSiteURL = "https://github.com"
	maxResponse    = 1 << 20
)

var (
	safeOwner = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	safeRepo  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	safeID    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	safeSlug  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,99})$`)
)

type OwnerKind string

const (
	OwnerUser         OwnerKind = "user"
	OwnerOrganization OwnerKind = "organization"
)

type Target struct {
	ConfigURL  string
	Owner      string
	Repository string
	OwnerKind  OwnerKind
}

func (t Target) Validate() error {
	if t.ConfigURL == "" || t.Owner == "" {
		return errors.New("GitHub target URL and owner are required")
	}
	if !safeOwner.MatchString(t.Owner) || (t.Repository != "" && !safeRepo.MatchString(t.Repository)) {
		return errors.New("GitHub target contains an invalid owner or repository name")
	}
	expectedURL := "https://github.com/" + t.Owner
	if t.Repository != "" {
		expectedURL += "/" + t.Repository
	}
	if t.ConfigURL != expectedURL {
		return errors.New("GitHub target URL must be canonical and match its owner and repository")
	}
	if t.OwnerKind != OwnerUser && t.OwnerKind != OwnerOrganization {
		return errors.New("GitHub owner kind must be user or organization")
	}
	if t.Repository == "" && t.OwnerKind != OwnerOrganization {
		return errors.New("organization runner scale sets require an organization owner")
	}
	return nil
}

type Credentials struct {
	AppID          int64
	ClientID       string
	InstallationID int64
	PrivateKey     string
	Slug           string
	HTMLURL        string
}

func (c Credentials) Validate() error {
	if c.ClientID == "" || c.InstallationID < 1 || c.PrivateKey == "" {
		return errors.New("incomplete GitHub App credentials")
	}
	if !safeID.MatchString(c.ClientID) {
		return errors.New("GitHub App client ID contains invalid characters")
	}
	if _, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(c.PrivateKey)); err != nil {
		return errors.New("GitHub App private key is not a valid RSA PEM key")
	}
	return nil
}

// SecretSink receives verified credentials. Implementations must not include
// credential values in errors or command arguments.
type SecretSink interface {
	Store(context.Context, Credentials) error
	Description() string
}

type Browser interface {
	Open(string) error
}

type Options struct {
	Target      Target
	Sink        SecretSink
	Browser     Browser
	NoBrowser   bool
	Timeout     time.Duration
	APIBaseURL  string
	SiteBaseURL string
	HomepageURL string
	HTTPClient  *http.Client
	Output      io.Writer
	Now         func() time.Time
	Listen      func(network, address string) (net.Listener, error)
}

type Result struct {
	AppURL          string
	InstallationID  int64
	SinkDescription string
}

// Bootstrap runs GitHub's App Manifest Flow on a loopback listener, verifies
// that the resulting installation can reach the requested target, and only
// then hands credentials to the configured sink.
func Bootstrap(ctx context.Context, options Options) (Result, error) {
	if err := options.Target.Validate(); err != nil {
		return Result{}, err
	}
	if options.Sink == nil {
		return Result{}, errors.New("credential sink is required")
	}
	if options.Browser == nil && !options.NoBrowser {
		return Result{}, errors.New("browser opener is required unless --no-browser is used")
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Minute
	}
	if options.Timeout > time.Hour {
		return Result{}, errors.New("GitHub App setup timeout cannot exceed one hour")
	}
	if options.APIBaseURL == "" {
		options.APIBaseURL = defaultAPIURL
	}
	if options.SiteBaseURL == "" {
		options.SiteBaseURL = defaultSiteURL
	}
	if options.HomepageURL == "" {
		options.HomepageURL = "https://runneryard.com"
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if options.Output == nil {
		options.Output = io.Discard
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Listen == nil {
		options.Listen = net.Listen
	}

	setupCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	listener, err := options.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return Result{}, fmt.Errorf("start secure loopback callback: %w", err)
	}
	defer listener.Close()

	state, err := randomHex(32)
	if err != nil {
		return Result{}, errors.New("generate setup state")
	}
	nameSuffix, err := randomHex(4)
	if err != nil {
		return Result{}, errors.New("generate app name")
	}
	callbackBase := "http://" + listener.Addr().String()
	callbackURL := callbackBase + "/callback"
	manifest, err := manifestJSON(options.Target, callbackURL, options.HomepageURL, nameSuffix)
	if err != nil {
		return Result{}, err
	}
	registerURL, err := registrationURL(options.SiteBaseURL, options.Target, state)
	if err != nil {
		return Result{}, err
	}

	callbackCodes := make(chan string, 1)
	handler := newLoopbackHandler(registerURL, manifest, state, callbackCodes)
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	serveDone := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
	}()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	startURL := callbackBase + "/start"
	fmt.Fprintf(options.Output, "RunnerYard will create a private GitHub App owned by %s.\n", options.Target.Owner)
	fmt.Fprintln(options.Output, permissionExplanation(options.Target))
	fmt.Fprintln(options.Output, "The app subscribes to no webhooks and RunnerYard receives no credential.")
	if err := exposeAndOpen(
		options.Output,
		options.Browser,
		options.NoBrowser,
		"Local setup URL (safe fallback if the browser opens elsewhere)",
		startURL,
	); err != nil {
		return Result{}, fmt.Errorf("open browser: %w; rerun with --no-browser", err)
	}

	code, err := waitValue(setupCtx, callbackCodes, serveDone, "GitHub App creation")
	if err != nil {
		return Result{}, err
	}
	api := API{BaseURL: options.APIBaseURL, Client: options.HTTPClient, Now: options.Now}
	credentials, err := api.ConvertManifest(setupCtx, code)
	if err != nil {
		return Result{}, err
	}
	installURL, err := installationURL(credentials)
	if err != nil {
		return Result{}, err
	}
	fmt.Fprintln(options.Output, "GitHub App created. Approve its installation only for the requested target.")
	if err := exposeAndOpen(
		options.Output,
		options.Browser,
		options.NoBrowser,
		"GitHub installation URL (safe fallback if the browser opens elsewhere)",
		installURL,
	); err != nil {
		return Result{}, fmt.Errorf("open installation page: %w; rerun with --no-browser", err)
	}

	installationID, err := api.WaitForInstallation(setupCtx, credentials, options.Target, 2*time.Second)
	if err != nil {
		return Result{}, err
	}
	credentials.InstallationID = installationID
	if err := api.Verify(setupCtx, credentials, options.Target); err != nil {
		return Result{}, err
	}
	if err := options.Sink.Store(setupCtx, credentials); err != nil {
		return Result{}, fmt.Errorf("store verified GitHub App credentials: %w", err)
	}
	return Result{
		AppURL:          credentials.HTMLURL,
		InstallationID:  credentials.InstallationID,
		SinkDescription: options.Sink.Description(),
	}, nil
}

func exposeAndOpen(output io.Writer, browser Browser, noBrowser bool, label, target string) error {
	fmt.Fprintf(output, "%s: %s\n", label, target)
	if noBrowser {
		return nil
	}
	return browser.Open(target)
}

func permissionExplanation(target Target) string {
	if target.Repository != "" {
		return "It requests Repository administration: write because GitHub places repository runner scale-set management behind that permission."
	}
	return "It requests Organization self-hosted runners: write because GitHub places organization runner scale-set management behind that permission."
}

func manifestJSON(target Target, redirectURL, homepageURL, suffix string) (string, error) {
	permissions := map[string]string{}
	if target.Repository != "" {
		permissions["administration"] = "write"
	} else {
		permissions["organization_self_hosted_runners"] = "write"
	}
	nameParts := []string{"RunnerYard", target.Owner}
	if target.Repository != "" {
		nameParts = append(nameParts, target.Repository)
	}
	nameParts = append(nameParts, suffix)
	manifest := map[string]any{
		"name":                     strings.Join(nameParts, " "),
		"url":                      homepageURL,
		"description":              "Dedicated credentials for one self-hosted RunnerYard fleet.",
		"redirect_url":             redirectURL,
		"public":                   false,
		"request_oauth_on_install": false,
		"hook_attributes": map[string]any{
			"url":    homepageURL + "/github/no-webhook",
			"active": false,
		},
		"default_permissions": permissions,
		"default_events":      []string{},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", errors.New("encode GitHub App manifest")
	}
	return string(encoded), nil
}

func registrationURL(siteBase string, target Target, state string) (string, error) {
	base, err := url.Parse(siteBase)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return "", errors.New("invalid GitHub site base URL")
	}
	if target.OwnerKind == OwnerOrganization {
		base.Path = "/organizations/" + url.PathEscape(target.Owner) + "/settings/apps/new"
	} else {
		base.Path = "/settings/apps/new"
	}
	query := base.Query()
	query.Set("state", state)
	base.RawQuery = query.Encode()
	return base.String(), nil
}

func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func waitValue[T any](ctx context.Context, values <-chan T, server <-chan error, label string) (T, error) {
	var zero T
	select {
	case value := <-values:
		return value, nil
	case err := <-server:
		if err == nil {
			err = errors.New("callback server stopped")
		}
		return zero, fmt.Errorf("%s failed: %w", label, err)
	case <-ctx.Done():
		return zero, fmt.Errorf("%s timed out or was canceled: %w", label, ctx.Err())
	}
}

type loopbackHandler struct {
	registerURL   string
	manifest      string
	state         string
	callbackCodes chan<- string
	callbackOnce  sync.Once
}

func newLoopbackHandler(registerURL, manifest, state string, callbackCodes chan<- string) http.Handler {
	return &loopbackHandler{
		registerURL: registerURL, manifest: manifest, state: state,
		callbackCodes: callbackCodes,
	}
}

var startTemplate = template.Must(template.New("start").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Connect GitHub - RunnerYard</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:42rem;margin:10vh auto;padding:1.5rem;color:#171717;background:#fafafa}h1{font-size:1.5rem}p{max-width:60ch;color:#525252}button{font:inherit;padding:.7rem 1rem;border:0;border-radius:.5rem;background:#171717;color:#fafafa;cursor:pointer}button:focus-visible{outline:3px solid #2563eb;outline-offset:3px}@media(prefers-color-scheme:dark){body{color:#e5e5e5;background:#171717}p{color:#a3a3a3}button{color:#171717;background:#e5e5e5}}</style>
<main><h1>Create a dedicated GitHub App</h1><p>GitHub will show the exact owner and permission before creating anything. RunnerYard does not receive your credential.</p><form action="{{.Action}}" method="post"><input type="hidden" name="manifest" value="{{.Manifest}}"><button type="submit">Review on GitHub</button></form></main></html>`))

func (h *loopbackHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action https://github.com; frame-ancestors 'none'; base-uri 'none'")
	writer.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if request.Method != http.MethodGet {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch request.URL.Path {
	case "/start":
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = startTemplate.Execute(writer, map[string]string{"Action": h.registerURL, "Manifest": h.manifest})
	case "/callback":
		if request.URL.Query().Get("state") != h.state {
			http.Error(writer, "setup state did not match", http.StatusBadRequest)
			return
		}
		code := strings.TrimSpace(request.URL.Query().Get("code"))
		if len(code) < 20 || len(code) > 256 || strings.ContainsAny(code, "\r\n") {
			http.Error(writer, "missing or invalid setup code", http.StatusBadRequest)
			return
		}
		h.callbackOnce.Do(func() { h.callbackCodes <- code })
		writeCompletion(writer, "GitHub App created", "RunnerYard will open the installation page next.")
	default:
		http.NotFound(writer, request)
	}
}

func writeCompletion(writer http.ResponseWriter, title, message string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(writer, "<!doctype html><html lang=en><meta charset=utf-8><meta name=viewport content='width=device-width,initial-scale=1'><title>%s</title><body><main><h1>%s</h1><p>%s</p></main></body></html>", template.HTMLEscapeString(title), template.HTMLEscapeString(title), template.HTMLEscapeString(message))
}

type API struct {
	BaseURL string
	Client  *http.Client
	Now     func() time.Time
}

func (api API) ResolveOwnerKind(ctx context.Context, owner string) (OwnerKind, error) {
	api = api.defaults()
	var account struct {
		Type string `json:"type"`
	}
	if err := api.doJSON(ctx, http.MethodGet, "/users/"+url.PathEscape(owner), "", nil, &account, http.StatusOK); err != nil {
		return "", fmt.Errorf("resolve GitHub owner %s: %w", owner, err)
	}
	switch account.Type {
	case "User":
		return OwnerUser, nil
	case "Organization":
		return OwnerOrganization, nil
	default:
		return "", fmt.Errorf("GitHub owner %s has unsupported account type %q", owner, account.Type)
	}
}

func (api API) defaults() API {
	if api.BaseURL == "" {
		api.BaseURL = defaultAPIURL
	}
	if api.Client == nil {
		api.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if api.Now == nil {
		api.Now = time.Now
	}
	return api
}

func (api API) ConvertManifest(ctx context.Context, code string) (Credentials, error) {
	api = api.defaults()
	endpoint := strings.TrimRight(api.BaseURL, "/") + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return Credentials{}, errors.New("create GitHub manifest exchange request")
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	response, err := api.Client.Do(request)
	if err != nil {
		return Credentials{}, fmt.Errorf("exchange GitHub App manifest code: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponse))
		return Credentials{}, fmt.Errorf("exchange GitHub App manifest code: GitHub returned %s", response.Status)
	}
	var payload struct {
		ID       int64  `json:"id"`
		ClientID string `json:"client_id"`
		PEM      string `json:"pem"`
		Slug     string `json:"slug"`
		HTMLURL  string `json:"html_url"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponse))
	if err := decoder.Decode(&payload); err != nil {
		return Credentials{}, errors.New("decode GitHub App manifest response")
	}
	credentials := Credentials{AppID: payload.ID, ClientID: payload.ClientID, PrivateKey: payload.PEM, Slug: payload.Slug, HTMLURL: payload.HTMLURL}
	if payload.ID < 1 || payload.ClientID == "" || payload.PEM == "" || payload.HTMLURL == "" {
		return Credentials{}, errors.New("GitHub App manifest response was incomplete")
	}
	if _, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(payload.PEM)); err != nil {
		return Credentials{}, errors.New("GitHub App manifest returned an invalid private key")
	}
	return credentials, nil
}

func installationURL(credentials Credentials) (string, error) {
	parsed, err := url.Parse(credentials.HTMLURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("GitHub App response contained an unsafe installation URL")
	}
	if !safeSlug.MatchString(credentials.Slug) || parsed.EscapedPath() != "/apps/"+url.PathEscape(credentials.Slug) {
		return "", errors.New("GitHub App response contained a mismatched installation URL")
	}
	return strings.TrimRight(credentials.HTMLURL, "/") + "/installations/new", nil
}

func (api API) Verify(ctx context.Context, credentials Credentials, target Target) error {
	api = api.defaults()
	if err := credentials.Validate(); err != nil {
		return err
	}
	appToken, err := api.appJWT(credentials)
	if err != nil {
		return err
	}
	var app struct {
		ID          int64             `json:"id"`
		ClientID    string            `json:"client_id"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := api.doJSON(ctx, http.MethodGet, "/app", appToken, nil, &app, http.StatusOK); err != nil {
		return fmt.Errorf("verify GitHub App identity: %w", err)
	}
	clientMatches := app.ClientID == credentials.ClientID || strconv.FormatInt(app.ID, 10) == credentials.ClientID
	if app.ID < 1 || !clientMatches {
		return errors.New("GitHub App identity does not match the supplied client ID")
	}
	if credentials.AppID > 0 && credentials.AppID != app.ID {
		return errors.New("GitHub App identity does not match the created app")
	}
	installation, err := api.targetInstallation(ctx, appToken, target)
	if err != nil {
		return fmt.Errorf("verify GitHub App installation for %s: %w", target.ConfigURL, err)
	}
	if installation.ID != credentials.InstallationID || installation.AppID != app.ID {
		return errors.New("GitHub App installation does not match the created app or selected target")
	}
	if !strings.EqualFold(installation.Account.Login, target.Owner) {
		return errors.New("GitHub App installation belongs to a different account")
	}
	if installation.Permissions[requiredPermission(target)] != "write" {
		return fmt.Errorf("GitHub App installation lacks required %s: write permission", requiredPermission(target))
	}
	return nil
}

func (api API) WaitForInstallation(ctx context.Context, credentials Credentials, target Target, interval time.Duration) (int64, error) {
	api = api.defaults()
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		appToken, err := api.appJWT(credentials)
		if err != nil {
			return 0, err
		}
		installation, err := api.targetInstallation(ctx, appToken, target)
		if err == nil {
			if installation.Permissions[requiredPermission(target)] != "write" {
				return 0, fmt.Errorf("GitHub App installation lacks required %s: write permission", requiredPermission(target))
			}
			return installation.ID, nil
		}
		var statusError *githubStatusError
		if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusNotFound {
			return 0, fmt.Errorf("discover GitHub App installation for %s: %w", target.ConfigURL, err)
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("GitHub App installation was not visible for %s before setup ended: %w", target.ConfigURL, ctx.Err())
		case <-ticker.C:
		}
	}
}

type installationRecord struct {
	ID      int64 `json:"id"`
	AppID   int64 `json:"app_id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
	Permissions map[string]string `json:"permissions"`
}

func (api API) targetInstallation(ctx context.Context, appToken string, target Target) (installationRecord, error) {
	var installation installationRecord
	path := "/repos/" + url.PathEscape(target.Owner) + "/" + url.PathEscape(target.Repository) + "/installation"
	if target.Repository == "" {
		path = "/orgs/" + url.PathEscape(target.Owner) + "/installation"
	}
	if err := api.doJSON(ctx, http.MethodGet, path, appToken, nil, &installation, http.StatusOK); err != nil {
		return installationRecord{}, err
	}
	return installation, nil
}

func requiredPermission(target Target) string {
	if target.Repository != "" {
		return "administration"
	}
	return "organization_self_hosted_runners"
}

func (api API) appJWT(credentials Credentials) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(credentials.PrivateKey))
	if err != nil {
		return "", errors.New("parse GitHub App private key")
	}
	now := api.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    credentials.ClientID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-30 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		return "", errors.New("sign GitHub App verification token")
	}
	return token, nil
}

func (api API) doJSON(ctx context.Context, method, path, token string, input, output any, expectedStatus int) error {
	var body io.Reader = http.NoBody
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return errors.New("encode GitHub request")
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(api.BaseURL, "/")+path, body)
	if err != nil {
		return errors.New("create GitHub request")
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := api.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponse))
		return &githubStatusError{StatusCode: response.StatusCode, Status: response.Status}
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponse))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponse)).Decode(output); err != nil {
		return errors.New("decode GitHub response")
	}
	return nil
}

type githubStatusError struct {
	StatusCode int
	Status     string
}

func (err *githubStatusError) Error() string { return "GitHub returned " + err.Status }
