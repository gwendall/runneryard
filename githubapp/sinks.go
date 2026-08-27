package githubapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type FileSink struct {
	ProjectDirectory string
	Force            bool
}

func (sink FileSink) Description() string {
	return filepath.Join(".runneryard", "github-app.env") + " and " + filepath.Join(".runneryard", "github-app.pem")
}

func (sink FileSink) Store(_ context.Context, credentials Credentials) error {
	if err := credentials.Validate(); err != nil {
		return err
	}
	projectDirectory := sink.ProjectDirectory
	if projectDirectory == "" {
		projectDirectory = "."
	}
	absoluteProject, err := filepath.Abs(projectDirectory)
	if err != nil {
		return errors.New("resolve project directory")
	}
	realProject, err := filepath.EvalSymlinks(absoluteProject)
	if err != nil {
		return fmt.Errorf("resolve project directory: %w", err)
	}
	info, err := os.Stat(realProject)
	if err != nil || !info.IsDir() {
		return errors.New("project directory does not exist or is not a directory")
	}
	secretDirectory := filepath.Join(realProject, ".runneryard")
	if info, err := os.Lstat(secretDirectory); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("refusing to store credentials through a non-directory or symlinked .runneryard path")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if err := os.Mkdir(secretDirectory, 0o700); err != nil {
		return fmt.Errorf("create .runneryard credential directory: %w", err)
	}
	// This is a directory mode, not a broadly readable file mode.
	if err := os.Chmod(secretDirectory, 0o700); err != nil { // #nosec G302
		return fmt.Errorf("secure .runneryard credential directory: %w", err)
	}

	keyPath := filepath.Join(secretDirectory, "github-app.pem")
	envPath := filepath.Join(secretDirectory, "github-app.env")
	for _, target := range []string{keyPath, envPath} {
		if info, err := os.Lstat(target); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("refusing to replace non-regular credential path %s", target)
			}
			if !sink.Force {
				return fmt.Errorf("credential path %s already exists; pass --force only for an intentional rotation", target)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	envContents := fmt.Sprintf("GITHUB_APP_CLIENT_ID=%s\nGITHUB_APP_INSTALLATION_ID=%d\nGITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/github-app.pem\n", credentials.ClientID, credentials.InstallationID)
	if err := ensureGitignore(filepath.Join(secretDirectory, ".gitignore"), []string{"github-app.env", "github-app.pem"}); err != nil {
		return err
	}
	keyBackup, err := backupCredentialFile(keyPath)
	if err != nil {
		return err
	}
	envBackup, err := backupCredentialFile(envPath)
	if err != nil {
		restoreCredentialFile(keyPath, keyBackup)
		return err
	}
	committed := false
	defer func() {
		if committed {
			_ = os.Remove(keyBackup)
			_ = os.Remove(envBackup)
			return
		}
		restoreCredentialFile(keyPath, keyBackup)
		restoreCredentialFile(envPath, envBackup)
	}()
	if err := writeSecretFile(keyPath, []byte(credentials.PrivateKey)); err != nil {
		return err
	}
	if err := writeSecretFile(envPath, []byte(envContents)); err != nil {
		return err
	}
	committed = true
	return nil
}

func backupCredentialFile(target string) (string, error) {
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	placeholder, err := os.CreateTemp(filepath.Dir(target), ".runneryard-backup-*")
	if err != nil {
		return "", errors.New("reserve credential backup path")
	}
	backup := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		return "", errors.New("close credential backup placeholder")
	}
	if err := os.Remove(backup); err != nil {
		return "", errors.New("prepare credential backup path")
	}
	if err := os.Rename(target, backup); err != nil {
		return "", errors.New("back up existing credential file")
	}
	return backup, nil
}

func restoreCredentialFile(target, backup string) {
	_ = os.Remove(target)
	if backup != "" {
		_ = os.Rename(backup, target)
	}
}

func writeSecretFile(target string, contents []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(target), ".runneryard-secret-*")
	if err != nil {
		return errors.New("create temporary credential file")
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		if closeErr := temporary.Close(); closeErr != nil {
			return fmt.Errorf("secure temporary credential file and close it: %v; %w", err, closeErr)
		}
		return errors.New("secure temporary credential file")
	}
	if _, err := temporary.Write(contents); err != nil {
		if closeErr := temporary.Close(); closeErr != nil {
			return fmt.Errorf("write temporary credential file and close it: %v; %w", err, closeErr)
		}
		return errors.New("write credential file")
	}
	if err := temporary.Sync(); err != nil {
		if closeErr := temporary.Close(); closeErr != nil {
			return fmt.Errorf("sync temporary credential file and close it: %v; %w", err, closeErr)
		}
		return errors.New("sync credential file")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close credential file")
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return errors.New("install credential file")
	}
	if err := os.Chmod(target, 0o600); err != nil {
		return errors.New("secure credential file")
	}
	return nil
}

func ensureGitignore(path string, entries []string) error {
	// path is always the verified project root plus the fixed
	// .runneryard/.gitignore suffix.
	contents, err := os.ReadFile(path) // #nosec G304
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read .runneryard/.gitignore: %w", err)
	}
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to edit symlinked .runneryard/.gitignore")
	}
	existing := map[string]bool{}
	for _, line := range strings.Split(string(contents), "\n") {
		existing[strings.TrimSpace(line)] = true
	}
	var additions strings.Builder
	if len(contents) > 0 && !bytes.HasSuffix(contents, []byte("\n")) {
		additions.WriteByte('\n')
	}
	for _, entry := range entries {
		if !existing[entry] {
			additions.WriteString(entry)
			additions.WriteByte('\n')
		}
	}
	if additions.Len() == 0 {
		return nil
	}
	// This file contains ignore patterns only, never a credential.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) // #nosec G302 G304
	if err != nil {
		return errors.New("open .runneryard/.gitignore")
	}
	defer file.Close()
	if _, err := file.WriteString(additions.String()); err != nil {
		return errors.New("update .runneryard/.gitignore")
	}
	return nil
}

type CommandRunner func(context.Context, []string, []byte) error

type FlySink struct {
	App string
	Run CommandRunner
}

var safeFlyApp = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (sink FlySink) Description() string {
	return "Fly secret store for " + sink.App
}

func (sink FlySink) Store(ctx context.Context, credentials Credentials) error {
	if err := credentials.Validate(); err != nil {
		return err
	}
	if !safeFlyApp.MatchString(sink.App) {
		return errors.New("fly controller app must contain only lowercase letters, numbers, and hyphens")
	}
	run := sink.Run
	if run == nil {
		run = runCommand
	}
	input := "GITHUB_APP_CLIENT_ID=" + credentials.ClientID + "\n" +
		"GITHUB_APP_INSTALLATION_ID=" + strconv.FormatInt(credentials.InstallationID, 10) + "\n" +
		"GITHUB_APP_PRIVATE_KEY=\"\"\"" + strings.TrimSuffix(credentials.PrivateKey, "\n") + "\"\"\"\n"
	if err := run(ctx, []string{"secrets", "import", "--app", sink.App}, []byte(input)); err != nil {
		return fmt.Errorf("fly secrets import failed: %w", err)
	}
	return nil
}

func runCommand(ctx context.Context, arguments []string, stdin []byte) error {
	// Arguments are fixed by FlySink after validating the only variable value,
	// the app name. exec.CommandContext does not invoke a shell.
	command := exec.CommandContext(ctx, "fly", arguments...) // #nosec G204
	command.Stdin = bytes.NewReader(stdin)
	// Do not return provider output. A future CLI version might echo imported
	// values on an error path.
	command.Stdout = ioDiscard{}
	command.Stderr = ioDiscard{}
	if err := command.Run(); err != nil {
		return err
	}
	return nil
}

type ioDiscard struct{}

func (ioDiscard) Write(buffer []byte) (int, error) { return len(buffer), nil }
