package hostagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBootLowersItsOutOfMemoryScoreBeforeDocker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oom_score_adj")
	if err := os.WriteFile(path, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := oomScoreAdjPath
	oomScoreAdjPath = path
	t.Cleanup(func() { oomScoreAdjPath = previous })

	if err := protectFromOOMKiller(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "-500" {
		t.Fatalf("oom_score_adj = %q, want the -500 systemd gives dockerd", got)
	}
}
