package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMergeQueueTickWithGitHubTokenOnlyPlans(t *testing.T) {
	base := strings.Repeat("b", 40)
	responses := map[string]string{
		"/repos/acme/widgets/contents/.github/merge-queue.yml": "required_checks:\n  - name: ci\n",
		"/repos/acme/widgets/git/ref/heads/main":               `{"object":{"sha":"` + base + `"}}`,
		"/repos/acme/widgets/issues":                           `[{"number":4,"pull_request":{}}]`,
		"/repos/acme/widgets/pulls/4": `{"draft":false,"head":{"sha":"` + strings.Repeat("4", 40) + `","ref":"feature","repo":{"full_name":"acme/widgets"}},` +
			`"base":{"ref":"main","repo":{"full_name":"acme/widgets"}}}`,
		"/repos/acme/widgets/issues/4/timeline":              `[{"event":"labeled","label":{"name":"queue"},"actor":{"login":"alice"},"created_at":"2026-10-11T10:00:00Z"}]`,
		"/repos/acme/widgets/collaborators/alice/permission": `{"permission":"write"}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("a dry run sent %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer read-only" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		body, ok := responses[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	env := map[string]string{"GITHUB_REPOSITORY": "acme/widgets", "GITHUB_TOKEN": "read-only", "GITHUB_API_URL": server.URL}
	var output bytes.Buffer
	if err := runMergeQueueTick(nil, &output, func(name string) string { return env[name] }); err != nil {
		t.Fatalf("tick: %v\n%s", err, output.String())
	}
	for _, expected := range []string{"dry run with GITHUB_TOKEN", "Plan (dry run) for acme/widgets: 1 action(s)", "- form a stack of #4 on bbbbbbb"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output is missing %q:\n%s", expected, output.String())
		}
	}
}

func TestMergeQueueTickRefusesBadInput(t *testing.T) {
	for name, test := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no token":   {env: map[string]string{"GITHUB_REPOSITORY": "acme/widgets"}, want: "MERGE_QUEUE_TOKEN"},
		"no repo":    {env: map[string]string{"MERGE_QUEUE_TOKEN": "t"}, want: "--repo must be owner/name"},
		"bad repo":   {args: []string{"--repo", "acme/widgets/extra"}, env: map[string]string{"MERGE_QUEUE_TOKEN": "t"}, want: "--repo must be owner/name"},
		"positional": {args: []string{"now"}, want: "positional"},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			err := runMergeQueueTick(test.args, &output, func(name string) string { return test.env[name] })
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	if err := run([]string{"merge-queue"}); err == nil {
		t.Fatal("merge-queue without a subcommand must fail")
	}
}
