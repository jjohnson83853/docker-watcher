package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTailWriterRetainsOnlyBoundedByteSuffix(t *testing.T) {
	writer := newTailWriter(8)
	for _, chunk := range [][]byte{[]byte("prefix"), []byte("suffix")} {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatalf("tail writer rejected output: %v", err)
		}
	}
	if len(writer.data) > 8 {
		t.Fatalf("tail writer retained %d bytes; maximum is 8", len(writer.data))
	}
	if got, want := string(writer.data), "ixsuffix"; got != want {
		t.Fatalf("tail writer retained %q; want byte suffix %q", got, want)
	}
}

func TestGitCloneUsesExternalPrivateCredentialFileWithoutTokenInArgvOrEnvironment(t *testing.T) {
	const token = "credential-file-only-pat"
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "git-observation")
	git := `#!/bin/sh
printf '%s\n' "$*" > "$GIT_OBSERVATION_ARGS"
printf '%s' "${GITHUB_TOKEN-}" > "$GIT_OBSERVATION_ENV"
is_clone=false
is_rev_parse=false
credential_file=
destination=
for argument in "$@"; do
  case "$argument" in
    clone) is_clone=true ;;
    rev-parse) is_rev_parse=true ;;
    credential.helper=store\ --file=*) credential_file=${argument#*--file=} ;;
  esac
  destination=$argument
done
if [ "$is_clone" = true ]; then
  test -n "$credential_file" || exit 31
  case "$credential_file" in "$destination"/*) exit 32 ;; esac
  test -f "$credential_file" || exit 33
  mode=$(stat -c '%a' "$credential_file") || exit 34
  test "$mode" = 600 || exit 35
  credential=$(cat "$credential_file") || exit 36
  test "$credential" = 'https://x-access-token:credential-file-only-pat@github.com' || exit 37
  printf '%s' "$credential_file" > "$GIT_OBSERVATION_CREDENTIAL_FILE"
  mkdir -p "$destination/.git"
elif [ "$is_rev_parse" = true ]; then
  printf 'abcdef0123456789\n'
fi
`
	for name, script := range map[string]string{
		"git": git,
		"docker": `#!/bin/sh
credential_file=$(cat "$GIT_OBSERVATION_CREDENTIAL_FILE") || exit 38
test -n "$credential_file" || exit 39
test ! -e "$credential_file" || exit 40
`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_OBSERVATION_ARGS", record+"-args")
	t.Setenv("GIT_OBSERVATION_ENV", record+"-env")
	t.Setenv("GIT_OBSERVATION_CREDENTIAL_FILE", record+"-credential-file")
	t.Setenv("GITHUB_TOKEN", token)

	_, err := BuildAndPush(context.Background(), BuildRequest{
		CloneURL: "https://github.com/owner/repo", Branch: "main", Token: token,
		RegistryURL: "registry.local", Image: "repo", Timeout: time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("fake Git clone using private credential file failed: %v", err)
	}
	args, err := os.ReadFile(record + "-args")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), token) {
		t.Fatalf("PAT appeared in Git argv: %q", args)
	}
	gotTokenEnv, err := os.ReadFile(record + "-env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(gotTokenEnv)) != "" {
		t.Fatalf("GITHUB_TOKEN exposed to Git subprocess: %q", gotTokenEnv)
	}
	credentialFile, err := os.ReadFile(record + "-credential-file")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(credentialFile)) == "" {
		t.Fatal("fake Git did not observe external credential file")
	}
	if _, err := os.Stat(strings.TrimSpace(string(credentialFile))); !os.IsNotExist(err) {
		t.Fatalf("temporary credential file was not removed after clone: stat err=%v", err)
	}
}

func TestBuildDiagnosticTailIsBoundedAndRedactsCredentialSplitAcrossWrites(t *testing.T) {
	const token = "split-secret-token"
	installFakeExecutables(t, `#!/bin/sh
case "$3" in
  clone) mkdir -p "$9/.git" ;;
esac
if [ "$1" = rev-parse ]; then printf 'abcdef0123456789\n'; fi
`, `#!/bin/sh
if [ "$1" = build ]; then
  printf '%05000d' 0 >&2
  printf '%s' 'split-secret-' >&2
  printf '%s' 'token' >&2
  printf '%s' 'END-OF-DIAGNOSTIC' >&2
  exit 7
fi
`)
	_, err := BuildAndPush(context.Background(), BuildRequest{
		CloneURL: "https://github.com/owner/repo", Branch: "main", Token: token,
		RegistryURL: "registry.local", Image: "repo", Timeout: time.Second,
	}, nil)
	if err == nil {
		t.Fatal("fake Docker build unexpectedly succeeded")
	}
	failure := RenderBuildFailure(err)
	if len(failure.OutputTail) != MaxBuildOutputChars || !strings.HasSuffix(failure.OutputTail, "END-OF-DIAGNOSTIC") {
		suffixStart := max(0, len(failure.OutputTail)-32)
		t.Fatalf("diagnostic tail length/suffix = %d/%q", len(failure.OutputTail), failure.OutputTail[suffixStart:])
	}
	if !strings.Contains(failure.OutputTail, "[REDACTED]") || strings.Contains(failure.OutputTail, token) || strings.Contains(failure.OutputTail, "split-secret-") {
		t.Fatalf("credential was not redacted across output writes: %q", failure.OutputTail)
	}
}
func TestGitCredentialRejectsUnsafeCloneOrigins(t *testing.T) {
	for _, cloneURL := range []string{
		"https://example.com/owner/repo",
		"https://user@github.com/owner/repo",
		"https://github.com/owner/repo?redirect=private",
		"https://github.com/owner/repo#private",
	} {
		t.Run(cloneURL, func(t *testing.T) {
			credential, err := gitCredential(cloneURL, "test-token")
			if err == nil || credential != "" {
				t.Fatalf("gitCredential(%q) = %q, %v; want rejection", cloneURL, credential, err)
			}
		})
	}
	if got, err := gitCredential("https://github.com/owner/repo", "test-token"); err != nil || got != "https://x-access-token:test-token@github.com\n" {
		t.Fatalf("valid GitHub credential = %q, %v", got, err)
	}
}
