package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestManifestUsesLeastPrivilegeWithoutWebhooks(t *testing.T) {
	for name, test := range map[string]struct {
		target     Target
		permission string
	}{
		"repository": {
			target:     Target{ConfigURL: "https://github.com/octo/repo", Owner: "octo", Repository: "repo", OwnerKind: OwnerOrganization},
			permission: "administration",
		},
		"organization": {
			target:     Target{ConfigURL: "https://github.com/octo", Owner: "octo", OwnerKind: OwnerOrganization},
			permission: "organization_self_hosted_runners",
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := manifestJSON(test.target, "http://127.0.0.1/callback", "https://runneryard.com", "abcd")
			if err != nil {
				t.Fatal(err)
			}
			var manifest map[string]any
			if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
				t.Fatal(err)
			}
			permissions := manifest["default_permissions"].(map[string]any)
			if len(permissions) != 1 || permissions[test.permission] != "write" {
				t.Fatalf("permissions = %#v", permissions)
			}
			if events := manifest["default_events"].([]any); len(events) != 0 {
				t.Fatalf("events = %#v", events)
			}
			hook := manifest["hook_attributes"].(map[string]any)
			if hook["active"] != false || manifest["public"] != false || manifest["request_oauth_on_install"] != false {
				t.Fatalf("unsafe manifest = %#v", manifest)
			}
		})
	}
}

func TestTargetRejectsMismatchedOrUnsafeIdentity(t *testing.T) {
	for _, target := range []Target{
		{ConfigURL: "https://github.com/octo/other", Owner: "octo", Repository: "repo", OwnerKind: OwnerOrganization},
		{ConfigURL: "https://github.com/octo/repo", Owner: "octo/escape", Repository: "repo", OwnerKind: OwnerOrganization},
		{ConfigURL: "https://github.example/octo/repo", Owner: "octo", Repository: "repo", OwnerKind: OwnerOrganization},
		{ConfigURL: "https://github.com/octo", Owner: "octo", OwnerKind: OwnerUser},
	} {
		if err := target.Validate(); err == nil {
			t.Fatalf("expected target rejection: %#v", target)
		}
	}
}

func TestCredentialsAndInstallationURLRejectInjection(t *testing.T) {
	privateKey := testPrivateKey(t)
	credentials := Credentials{ClientID: "Iv1.good\nINJECTED=value", InstallationID: 7, PrivateKey: privateKey}
	if err := credentials.Validate(); err == nil {
		t.Fatal("expected injected client ID rejection")
	}
	for _, credentials := range []Credentials{
		{Slug: "safe", HTMLURL: "https://evil.example/apps/safe"},
		{Slug: "safe", HTMLURL: "https://github.com/apps/other"},
		{Slug: "safe", HTMLURL: "https://github.com/apps/safe?next=evil"},
	} {
		if _, err := installationURL(credentials); err == nil {
			t.Fatalf("expected installation URL rejection: %#v", credentials)
		}
	}
}

func TestLoopbackRejectsMismatchedStateAndDuplicateCallbacks(t *testing.T) {
	codes := make(chan string, 2)
	handler := newLoopbackHandler("https://github.com/settings/apps/new?state=right", `{}`, "right", codes)
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := http.Get(server.URL + "/callback?state=wrong&code=abcdefghijklmnopqrstuvwxyz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || len(codes) != 0 {
		t.Fatalf("mismatched callback status=%d codes=%d", response.StatusCode, len(codes))
	}
	for _, code := range []string{"abcdefghijklmnopqrstuvwxyz", "zyxwvutsrqponmlkjihgfedcba"} {
		response, err = http.Get(server.URL + "/callback?state=right&code=" + code)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	if len(codes) != 1 {
		t.Fatalf("callback count = %d, want 1", len(codes))
	}
}

func TestBootstrapCreatesVerifiesAndStoresWithoutPrintingASecret(t *testing.T) {
	privateKey := testPrivateKey(t)
	apiServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/app-manifests/"):
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id": 42, "client_id": "Iv1.runner", "pem": privateKey,
				"slug": "runneryard-test", "html_url": "https://github.com/apps/runneryard-test",
			})
		case request.Method == http.MethodGet && request.URL.Path == "/app":
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": 42, "client_id": "Iv1.runner"})
		case request.Method == http.MethodGet && request.URL.Path == "/repos/octo/repo/installation":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id": 77, "app_id": 42, "account": map[string]any{"login": "octo"},
				"permissions": map[string]string{"administration": "write"},
			})
		default:
			http.Error(writer, "unexpected request", http.StatusNotFound)
		}
	}))
	defer apiServer.Close()

	var output strings.Builder
	var openedURLs []string
	browser := browserFunc(func(openURL string) error {
		if !strings.Contains(output.String(), openURL) {
			t.Fatalf("browser opened %q before the fallback URL was printed", openURL)
		}
		openedURLs = append(openedURLs, openURL)
		if strings.Contains(openURL, "/start") {
			response, err := http.Get(openURL)
			if err != nil {
				return err
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			manifestMatch := regexp.MustCompile(`name="manifest" value="([^"]+)"`).FindSubmatch(body)
			actionMatch := regexp.MustCompile(`form action="([^"]+)"`).FindSubmatch(body)
			if len(manifestMatch) != 2 || len(actionMatch) != 2 {
				return errors.New("setup page did not contain manifest form")
			}
			var manifest map[string]any
			if err := json.Unmarshal([]byte(html.UnescapeString(string(manifestMatch[1]))), &manifest); err != nil {
				return err
			}
			action, _ := url.Parse(html.UnescapeString(string(actionMatch[1])))
			callback := manifest["redirect_url"].(string) + "?code=abcdefghijklmnopqrstuvwxyz&state=" + url.QueryEscape(action.Query().Get("state"))
			response, err = http.Get(callback)
			if err == nil {
				response.Body.Close()
			}
			return err
		}
		if strings.Contains(openURL, "/installations/new") {
			return nil
		}
		return errors.New("unexpected browser URL")
	})
	sink := &captureSink{}
	result, err := Bootstrap(context.Background(), Options{
		Target: Target{ConfigURL: "https://github.com/octo/repo", Owner: "octo", Repository: "repo", OwnerKind: OwnerOrganization},
		Sink:   sink, Browser: browser, Timeout: time.Second, APIBaseURL: apiServer.URL,
		SiteBaseURL: "https://github.com", HTTPClient: apiServer.Client(), Output: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.InstallationID != 77 || sink.credentials.InstallationID != 77 {
		t.Fatalf("result=%#v credentials=%#v", result, sink.credentials)
	}
	if len(openedURLs) != 2 {
		t.Fatalf("opened URLs = %#v", openedURLs)
	}
	for _, openedURL := range openedURLs {
		if !strings.Contains(output.String(), openedURL) {
			t.Fatalf("browser fallback URL %q missing from output", openedURL)
		}
	}
	if strings.Contains(output.String(), "BEGIN RSA PRIVATE KEY") || strings.Contains(output.String(), privateKey) {
		t.Fatal("private key appeared in user output")
	}
}

func TestExposeAndOpenNoBrowserPrintsWithoutOpening(t *testing.T) {
	var output strings.Builder
	browser := browserFunc(func(openURL string) error {
		t.Fatalf("browser unexpectedly opened %q", openURL)
		return nil
	})
	const fallbackURL = "http://127.0.0.1:54321/start"
	if err := exposeAndOpen(&output, browser, true, "Local setup URL", fallbackURL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), fallbackURL) {
		t.Fatalf("fallback URL missing from output: %q", output.String())
	}
}

func TestManifestExchangeDoesNotReflectErrorBodies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "TOP-SECRET-PEM", http.StatusInternalServerError)
	}))
	defer server.Close()
	_, err := (API{BaseURL: server.URL, Client: server.Client()}).ConvertManifest(context.Background(), "abcdefghijklmnopqrstuvwxyz")
	if err == nil || strings.Contains(err.Error(), "TOP-SECRET-PEM") {
		t.Fatalf("error = %v", err)
	}
}

func TestWaitForInstallationTimesOutWithoutReflectingResponseBody(t *testing.T) {
	privateKey := testPrivateKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/repos/octo/repo/installation" {
			http.Error(writer, "SENSITIVE-PROVIDER-BODY", http.StatusNotFound)
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := (API{BaseURL: server.URL, Client: server.Client()}).WaitForInstallation(ctx, Credentials{
		ClientID: "Iv1.runner", PrivateKey: privateKey,
	}, Target{ConfigURL: "https://github.com/octo/repo", Owner: "octo", Repository: "repo", OwnerKind: OwnerOrganization}, time.Millisecond)
	if err == nil || strings.Contains(err.Error(), "SENSITIVE-PROVIDER-BODY") {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyRejectsWrongInstallationAndPermission(t *testing.T) {
	privateKey := testPrivateKey(t)
	for name, installation := range map[string]map[string]any{
		"wrong installation": {"id": 99, "app_id": 42, "account": map[string]any{"login": "octo"}, "permissions": map[string]string{"administration": "write"}},
		"missing permission": {"id": 77, "app_id": 42, "account": map[string]any{"login": "octo"}, "permissions": map[string]string{"administration": "read"}},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/app" {
					_ = json.NewEncoder(writer).Encode(map[string]any{"id": 42, "client_id": "Iv1.runner"})
					return
				}
				_ = json.NewEncoder(writer).Encode(installation)
			}))
			defer server.Close()
			err := (API{BaseURL: server.URL, Client: server.Client()}).Verify(context.Background(), Credentials{
				AppID: 42, ClientID: "Iv1.runner", InstallationID: 77, PrivateKey: privateKey,
			}, Target{ConfigURL: "https://github.com/octo/repo", Owner: "octo", Repository: "repo", OwnerKind: OwnerOrganization})
			if err == nil {
				t.Fatal("expected verification failure")
			}
		})
	}
}

func TestFileSinkUsesPrivateModesAndRejectsSymlink(t *testing.T) {
	project := t.TempDir()
	credentials := Credentials{ClientID: "Iv1.runner", InstallationID: 77, PrivateKey: testPrivateKey(t)}
	if err := (FileSink{ProjectDirectory: project}).Store(context.Background(), credentials); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"github-app.pem", "github-app.env"} {
		info, err := os.Stat(filepath.Join(project, ".runneryard", name))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", name, info.Mode().Perm())
		}
	}
	contents, _ := os.ReadFile(filepath.Join(project, ".runneryard", ".gitignore"))
	if !strings.Contains(string(contents), "github-app.pem") || !strings.Contains(string(contents), "github-app.env") {
		t.Fatalf("gitignore = %q", contents)
	}

	outside := t.TempDir()
	projectWithLink := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(projectWithLink, ".runneryard")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := (FileSink{ProjectDirectory: projectWithLink}).Store(context.Background(), credentials); err == nil {
		t.Fatal("expected symlinked directory rejection")
	}
}

func TestFlySinkKeepsPrivateKeyOutOfArguments(t *testing.T) {
	credentials := Credentials{ClientID: "Iv1.runner", InstallationID: 77, PrivateKey: testPrivateKey(t)}
	var arguments []string
	var stdin []byte
	run := func(_ context.Context, args []string, input []byte) error {
		arguments = append([]string(nil), args...)
		stdin = append([]byte(nil), input...)
		return nil
	}
	if err := (FlySink{App: "octo-ci-controller", Run: run}).Store(context.Background(), credentials); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(arguments, " "), "PRIVATE KEY") || !strings.Contains(string(stdin), "GITHUB_APP_PRIVATE_KEY=\"\"\"") {
		t.Fatalf("arguments=%#v stdin format valid=%t", arguments, strings.Contains(string(stdin), "GITHUB_APP_PRIVATE_KEY"))
	}
}

type browserFunc func(string) error

func (function browserFunc) Open(target string) error { return function(target) }

type captureSink struct{ credentials Credentials }

func (sink *captureSink) Store(_ context.Context, credentials Credentials) error {
	sink.credentials = credentials
	return nil
}

func (*captureSink) Description() string { return "test sink" }

func testPrivateKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
