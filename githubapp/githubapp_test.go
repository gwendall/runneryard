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
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
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
			raw, err := manifestJSON(RunnerProfile(test.target), "http://127.0.0.1/callback", "https://runneryard.com", "abcd")
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

func TestManifestNameFitsGitHubsLimitWithTheWholeSuffix(t *testing.T) {
	long := strings.Repeat("a", 39)
	for _, test := range []struct {
		profile Profile
		want    string
	}{
		{RunnerProfile(Target{Owner: "octo", Repository: "repo"}), "RunnerYard octo repo 1a2b3c4d"},
		{RunnerProfile(Target{Owner: "octo"}), "RunnerYard octo 1a2b3c4d"},
		// The case GitHub refused: the repository goes first.
		{RunnerProfile(Target{Owner: "KamiRobotics", Repository: "kira-monorepo"}), "RunnerYard KamiRobotics 1a2b3c4d"},
		{RunnerProfile(Target{Owner: "octo", Repository: "kira-monorepo"}), "RunnerYard octo kira-mono 1a2b3c4d"},
		{RunnerProfile(Target{Owner: "octo", Repository: "abcdefgh-ijkl"}), "RunnerYard octo abcdefgh 1a2b3c4d"},
		{RunnerProfile(Target{Owner: long, Repository: long}), "RunnerYard aaaaaaaaaaaaaa 1a2b3c4d"},
		{RunnerProfile(Target{Owner: long}), "RunnerYard aaaaaaaaaaaaaa 1a2b3c4d"},
		{queueProfile(), "RunnerYard merge queue 1a2b3c4d"},
	} {
		raw, err := manifestJSON(test.profile, "http://127.0.0.1/callback", "https://runneryard.com", "1a2b3c4d")
		if err != nil {
			t.Fatal(err)
		}
		var manifest map[string]any
		if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
			t.Fatal(err)
		}
		name := manifest["name"].(string)
		if name != test.want || len(name) > maxAppNameLength {
			t.Errorf("name = %q (%d characters), want %q", name, len(name), test.want)
		}
	}

	tooLong := queueProfile()
	tooLong.Name = strings.Repeat("n", maxAppNameLength-2*nameSuffixBytes)
	if err := tooLong.validate(); err == nil {
		t.Fatal("a profile name without room for the suffix was accepted")
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
	browser := manifestBrowser(t, &output, &openedURLs, nil)
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
	if strings.Join(arguments, " ") != "secrets import --stage --app octo-ci-controller" {
		t.Fatalf("the Fly sink must stage secrets so a token-authenticated controller is not restarted into a refusal: %#v", arguments)
	}
	next := (FlySink{App: "octo-ci-controller"}).NextStep()
	if !strings.Contains(next, "fly secrets unset GITHUB_TOKEN --app octo-ci-controller") || !strings.Contains(next, "fly deploy --app octo-ci-controller") {
		t.Fatalf("next step must name both ways to apply the staged secrets: %q", next)
	}
}

func queueProfile() Profile {
	return Profile{
		Name:        "RunnerYard merge queue",
		Description: "test queue",
		Permissions: map[string]string{"actions": "write", "checks": "read", "contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write"},
		Explanation: "It requests what a merge queue needs.",
	}
}

// queueAPI fakes GitHub for an App with the queue's permissions. installed
// says whether /repos/octo/repo/installation answers yet.
func queueAPI(t *testing.T, privateKey string, permissions map[string]string, installed func() bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/app-manifests/"):
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id": 42, "client_id": "Iv1.queue", "pem": privateKey,
				"slug": "runneryard-merge-queue-abcd", "html_url": "https://github.com/apps/runneryard-merge-queue-abcd",
			})
		case request.Method == http.MethodGet && request.URL.Path == "/app":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id": 42, "client_id": "Iv1.queue", "slug": "runneryard-merge-queue-abcd",
				"html_url": "https://github.com/apps/runneryard-merge-queue-abcd",
			})
		case request.Method == http.MethodGet && request.URL.Path == "/repos/octo/repo/installation" && installed():
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id": 77, "app_id": 42, "account": map[string]any{"login": "octo"},
				"permissions": permissions, "repository_selection": "selected",
			})
		default:
			http.Error(writer, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestBootstrapCreatesTheProfilesAppAndRequiresEveryPermission(t *testing.T) {
	privateKey := testPrivateKey(t)
	profile := queueProfile()
	target := Target{ConfigURL: "https://github.com/octo/repo", Owner: "octo", Repository: "repo", OwnerKind: OwnerUser}
	granted := map[string]string{"actions": "write", "checks": "read", "contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write"}

	server := queueAPI(t, privateKey, granted, func() bool { return true })
	var output strings.Builder
	var opened []string
	var manifests []map[string]any
	sink := &captureSink{}
	result, err := Bootstrap(context.Background(), Options{
		Target: target, Profile: &profile, Sink: sink, Browser: manifestBrowser(t, &output, &opened, &manifests),
		Timeout: time.Second, APIBaseURL: server.URL, HTTPClient: server.Client(), Output: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 {
		t.Fatalf("manifests = %d", len(manifests))
	}
	manifest := manifests[0]
	permissions := map[string]string{}
	for name, level := range manifest["default_permissions"].(map[string]any) {
		permissions[name] = level.(string)
	}
	if !reflect.DeepEqual(permissions, profile.Permissions) {
		t.Fatalf("manifest permissions = %v, want the profile's %v", permissions, profile.Permissions)
	}
	if name := manifest["name"].(string); !strings.HasPrefix(name, profile.Name+" ") || manifest["description"] != profile.Description {
		t.Fatalf("manifest name %q, description %q", name, manifest["description"])
	}
	if hook := manifest["hook_attributes"].(map[string]any); hook["active"] != false || manifest["public"] != false || len(manifest["default_events"].([]any)) != 0 {
		t.Fatalf("unsafe manifest = %#v", manifest)
	}
	if result.Slug != "runneryard-merge-queue-abcd" || sink.credentials.InstallationID != 77 || !strings.Contains(output.String(), profile.Explanation) {
		t.Fatalf("result = %#v, output = %q", result, output.String())
	}
	if strings.Contains(output.String(), "PRIVATE KEY") {
		t.Fatal("private key appeared in user output")
	}

	// An installation that lost one permission is refused before the sink.
	partial := map[string]string{}
	for name, level := range granted {
		partial[name] = level
	}
	partial["checks"] = ""
	server = queueAPI(t, privateKey, partial, func() bool { return true })
	output.Reset()
	sink = &captureSink{}
	_, err = Bootstrap(context.Background(), Options{
		Target: target, Profile: &profile, Sink: sink, Browser: manifestBrowser(t, &output, &opened, nil),
		Timeout: time.Second, APIBaseURL: server.URL, HTTPClient: server.Client(), Output: &output,
	})
	if err == nil || !strings.Contains(err.Error(), "checks: read") || sink.credentials.PrivateKey != "" {
		t.Fatalf("err = %v, stored = %t; want a refusal naming checks: read and nothing stored", err, sink.credentials.PrivateKey != "")
	}
}

func TestReuseOpensTheInstallationPageOnlyWhenTheRepositoryIsMissing(t *testing.T) {
	privateKey := testPrivateKey(t)
	profile := queueProfile()
	target := Target{ConfigURL: "https://github.com/octo/repo", Owner: "octo", Repository: "repo", OwnerKind: OwnerUser}
	for name, test := range map[string]struct {
		installedAfter int
		wantOpened     int
	}{
		"already installed":     {installedAfter: 0, wantOpened: 0},
		"added by the operator": {installedAfter: 2, wantOpened: 1},
	} {
		t.Run(name, func(t *testing.T) {
			var lookups atomic.Int32
			server := queueAPI(t, privateKey, profile.Permissions, func() bool { return int(lookups.Add(1)) > test.installedAfter })
			var output strings.Builder
			var opened []string
			sink := &captureSink{}
			result, err := Reuse(context.Background(), Options{
				Target: target, Profile: &profile, Sink: sink, Browser: manifestBrowser(t, &output, &opened, nil),
				Timeout: 5 * time.Second, APIBaseURL: server.URL, HTTPClient: server.Client(), Output: &output,
			}, Credentials{ClientID: "Iv1.queue", PrivateKey: privateKey})
			if err != nil {
				t.Fatal(err)
			}
			if len(opened) != test.wantOpened {
				t.Fatalf("opened %v, want %d page(s)", opened, test.wantOpened)
			}
			if test.wantOpened == 1 && opened[0] != "https://github.com/apps/runneryard-merge-queue-abcd/installations/new" {
				t.Fatalf("opened %q", opened[0])
			}
			if result.Slug != "runneryard-merge-queue-abcd" || sink.credentials.InstallationID != 77 || sink.credentials.PrivateKey != privateKey {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestRepositorySinkSealsThroughGHWithTheKeyOnStdinOnly(t *testing.T) {
	credentials := Credentials{ClientID: "Iv1.queue", InstallationID: 77, PrivateKey: testPrivateKey(t)}
	type call struct {
		args  string
		stdin string
	}
	var calls []call
	run := func(_ context.Context, args []string, stdin []byte) error {
		calls = append(calls, call{strings.Join(args, " "), string(stdin)})
		return nil
	}
	sink := RepositorySink{Owner: "octo", Repository: "repo", ClientIDVariable: "MERGE_QUEUE_APP_CLIENT_ID", PrivateKeySecret: "MERGE_QUEUE_APP_PRIVATE_KEY", Run: run}
	if err := sink.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sink.Store(context.Background(), credentials); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{"api --silent repos/octo/repo/actions/secrets/public-key", ""},
		{"secret set MERGE_QUEUE_APP_PRIVATE_KEY --app actions --repo octo/repo", credentials.PrivateKey},
		{"variable set MERGE_QUEUE_APP_CLIENT_ID --repo octo/repo --body Iv1.queue", ""},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("gh calls = %q, want %q", calls, want)
	}
	if strings.Contains(sink.Description()+sink.NextStep(), "PRIVATE KEY") {
		t.Fatal("sink text contains the key")
	}

	for _, bad := range []RepositorySink{
		{Owner: "octo", Repository: "repo --body x", ClientIDVariable: "A", PrivateKeySecret: "B", Run: run},
		{Owner: "octo", Repository: "repo", ClientIDVariable: "lower", PrivateKeySecret: "B", Run: run},
		{Owner: "octo", Repository: "repo", ClientIDVariable: "A", PrivateKeySecret: "B --repo other/repo", Run: run},
	} {
		calls = nil
		if err := bad.Store(context.Background(), credentials); err == nil || len(calls) != 0 {
			t.Fatalf("sink %+v ran %q", bad, calls)
		}
	}
}

// manifestBrowser stands in for the operator's browser: it submits the setup
// page's manifest to GitHub (the callback, here), records each manifest when
// manifests is not nil, and accepts the installation page.
func manifestBrowser(t *testing.T, output *strings.Builder, openedURLs *[]string, manifests *[]map[string]any) browserFunc {
	return browserFunc(func(openURL string) error {
		if !strings.Contains(output.String(), openURL) {
			t.Fatalf("browser opened %q before the fallback URL was printed", openURL)
		}
		*openedURLs = append(*openedURLs, openURL)
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
			if manifests != nil {
				*manifests = append(*manifests, manifest)
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
}

type browserFunc func(string) error

func (function browserFunc) Open(target string) error { return function(target) }

type captureSink struct{ credentials Credentials }

func (sink *captureSink) Store(_ context.Context, credentials Credentials) error {
	sink.credentials = credentials
	return nil
}

func (*captureSink) Description() string { return "test sink" }

func (*captureSink) NextStep() string { return "" }

func testPrivateKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
