package watcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadStateHandlesNullMalformedAndUnreadableFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state, err := LoadState(path); err == nil {
		t.Fatalf("existing JSON null state was accepted as %#v", state)
	}

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("malformed state JSON was accepted")
	}

	if err := os.WriteFile(path, []byte(`{} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("multiple top-level state values were accepted")
	}

	blocker := filepath.Join(dir, "not-a-directory")

	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(filepath.Join(blocker, "state.json")); err == nil {
		t.Fatal("unreadable state path was treated as missing")
	}
}

func TestLoadStateDoesNotReturnStateUntilDirectorySyncSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"repo":{"target":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := loadState(path, func(string) error {
		return errors.New("directory sync unavailable")
	})
	if err == nil || state != nil {
		t.Fatalf("unconfirmed state became actionable: state=%#v err=%v", state, err)
	}
}
func TestLoadStateRetriesDirectoryAncestrySyncAfterFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state", "nested", "state.json")
	syncFailure := errors.New("ancestor sync unavailable")
	firstSync := true
	if state, err := loadState(path, func(string) error {
		if firstSync {
			firstSync = false
			return syncFailure
		}
		return nil
	}); !errors.Is(err, syncFailure) || state != nil {
		t.Fatalf("first load after directory creation = %#v, %v; want fail closed", state, err)
	}

	var synced []string
	state, err := loadState(path, func(directory string) error {
		synced = append(synced, directory)
		return nil
	})
	if err != nil || state == nil {
		t.Fatalf("retry load = %#v, %v; want empty state after durability confirmation", state, err)
	}
	want := directoryAncestry(filepath.Dir(path))
	if len(synced) != len(want) {
		t.Fatalf("retry synced directories = %v; want %v", synced, want)
	}
	for index := range want {
		if synced[index] != want[index] {
			t.Fatalf("retry synced directories = %v; want %v", synced, want)
		}
	}
}

func TestLoadStateConfirmsExistingDirectoryAncestry(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state", "nested", "state.json")
	if err := SaveState(path, State{"repo": "new"}); err != nil {
		t.Fatal(err)
	}
	var synced []string
	if _, err := loadState(path, func(directory string) error {
		synced = append(synced, directory)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := directoryAncestry(filepath.Dir(path))
	if len(synced) != len(want) {
		t.Fatalf("existing-state syncs = %v; want %v", synced, want)
	}
	for index := range want {
		if synced[index] != want[index] {
			t.Fatalf("existing-state syncs = %v; want %v", synced, want)
		}
	}
}

func directoryAncestry(directory string) []string {
	ancestry := []string{}
	for current := filepath.Clean(directory); ; current = filepath.Dir(current) {
		ancestry = append(ancestry, current)
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
	for left, right := 0, len(ancestry)-1; left < right; left, right = left+1, right-1 {
		ancestry[left], ancestry[right] = ancestry[right], ancestry[left]
	}
	return ancestry
}

func TestSaveStateConfirmsDirectoryAncestryOnEverySave(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state", "nested", "state.json")
	stateDirectory := filepath.Dir(path)
	want := append(directoryAncestry(stateDirectory), stateDirectory)
	var synced []string
	for range 2 {
		synced = synced[:0]
		if err := saveState(path, State{"repo": "new"}, func(directory string) error {
			synced = append(synced, directory)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(synced) != len(want) {
			t.Fatalf("synced directories = %v; want %v", synced, want)
		}
		for index := range want {
			if synced[index] != want[index] {
				t.Fatalf("synced directories = %v; want %v", synced, want)
			}
		}
	}
}

func TestSaveStatePostRenameSyncErrorRequiresLoadConfirmation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	syncFailure := errors.New("directory sync unavailable")
	syncCalls := 0
	postRenameSync := len(directoryAncestry(filepath.Dir(path))) + 1
	if err := saveState(path, State{"repo": "new"}, func(string) error {
		syncCalls++
		if syncCalls == postRenameSync {
			return syncFailure
		}
		return nil
	}); !errors.Is(err, syncFailure) {
		t.Fatalf("post-rename sync error = %v; want injected failure", err)
	}
	state, err := loadState(path, func(string) error { return syncFailure })
	if !errors.Is(err, syncFailure) || state != nil {
		t.Fatalf("unconfirmed renamed state = %#v, %v; want no state and sync failure", state, err)
	}
	state, err = loadState(path, func(string) error { return nil })
	if err != nil || state["repo"] != "new" {
		t.Fatalf("state could not be consumed after successful directory-sync confirmation: %#v, %v", state, err)
	}
}
func TestSaveStateUsesAbsoluteAncestryForRelativePath(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	relativeRoot, err := filepath.Rel(workingDirectory, root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(relativeRoot, "state", "nested", "state.json")
	targetDirectory := filepath.Join(root, "state", "nested")
	var synced []string
	if err := saveState(path, State{"repo": "new"}, func(directory string) error {
		synced = append(synced, directory)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertSyncOrder(t, synced, string(filepath.Separator), targetDirectory)
	if _, err := os.Stat(filepath.Join(targetDirectory, "state.json")); err != nil {
		t.Fatalf("relative state path was not saved at its absolute target: %v", err)
	}
}

func TestSaveStateSyncsResolvedSymlinkTargetAncestry(t *testing.T) {
	root := t.TempDir()
	physicalBase := filepath.Join(root, "physical")
	physicalParent := filepath.Join(physicalBase, "parent")
	if err := os.MkdirAll(physicalParent, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias")
	if err := os.Symlink(physicalParent, link); err != nil {
		t.Fatal(err)
	}
	targetDirectory := filepath.Join(physicalParent, "child", "state")
	path := filepath.Join(link, "child", "state", "state.json")
	var synced []string
	if err := saveState(path, State{"repo": "new"}, func(directory string) error {
		synced = append(synced, directory)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertSyncOrder(t, synced, physicalBase, physicalParent, targetDirectory)
}

func TestSaveStateNormalizesSymlinkDotDotBeforeIO(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "physical")
	if err := os.MkdirAll(filepath.Join(physical, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(physical, "outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(physical, "parent"), link); err != nil {
		t.Fatal(err)
	}
	rawPath := link + string(filepath.Separator) + ".." + string(filepath.Separator) + "outside" + string(filepath.Separator) + "state.json"
	normalizedPath := filepath.Clean(rawPath)
	unexpectedPath := filepath.Join(physical, "outside", "state.json")
	if err := saveState(rawPath, State{"repo": "new"}, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(normalizedPath); err != nil {
		t.Fatalf("normalized state path was not written: %v", err)
	}
	if _, err := os.Stat(unexpectedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state was written through symlink-dot-dot path: err=%v", err)
	}
	state, err := loadState(rawPath, func(string) error { return nil })
	if err != nil || state["repo"] != "new" {
		t.Fatalf("normalized state path could not be loaded: %#v, %v", state, err)
	}
}

func TestSaveStateFailsWhenIntermediateSymlinkAncestryCannotSync(t *testing.T) {
	root := t.TempDir()
	transit := filepath.Join(root, "transit")
	physicalParent := filepath.Join(root, "physical", "parent")
	if err := os.MkdirAll(transit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(physicalParent, 0o755); err != nil {
		t.Fatal(err)
	}
	hop := filepath.Join(transit, "hop")
	if err := os.Symlink(physicalParent, hop); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(hop, alias); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(alias, "child", "state", "state.json")
	statePath := filepath.Join(physicalParent, "child", "state", "state.json")
	syncFailure := errors.New("intermediate ancestry sync unavailable")
	intermediateSynced := false
	err := saveState(path, State{"repo": "new"}, func(directory string) error {
		if directory == transit {
			intermediateSynced = true
			return syncFailure
		}
		return nil
	})
	if !errors.Is(err, syncFailure) || !intermediateSynced {
		t.Fatalf("save error = %v; intermediate directory synced=%t, want injected failure", err, intermediateSynced)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state became visible after intermediate ancestry sync failure: %v", err)
	}
}

func TestSaveStateSyncsRelativeSymlinkTargetWithDotDot(t *testing.T) {
	root := t.TempDir()
	transit := filepath.Join(root, "transit")
	physicalParent := filepath.Join(root, "physical", "parent")
	if err := os.MkdirAll(transit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(physicalParent, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "outside", "state", "child", "state", "state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	hop := filepath.Join(transit, "hop")
	if err := os.Symlink(filepath.Join("..", "physical", "parent"), hop); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	aliasTarget := "transit" + string(filepath.Separator) + "hop" + string(filepath.Separator) + ".." + string(filepath.Separator) + ".." + string(filepath.Separator) + "outside" + string(filepath.Separator) + "state"
	if err := os.Symlink(aliasTarget, alias); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(alias, "child", "state", "state.json")
	resolvedDirectory, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || resolvedDirectory != filepath.Dir(statePath) {
		t.Fatalf("relative symlink target resolves to %q, %v; want %q", resolvedDirectory, err, filepath.Dir(statePath))
	}
	syncFailure := errors.New("relative symlink ancestry sync unavailable")
	intermediateSynced := false
	err = saveState(path, State{"repo": "new"}, func(directory string) error {
		if directory == transit {
			intermediateSynced = true
			return syncFailure
		}
		return nil
	})
	if !errors.Is(err, syncFailure) || !intermediateSynced {
		t.Fatalf("save error = %v; relative intermediate directory synced=%t, want injected failure", err, intermediateSynced)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state became visible after intermediate ancestry sync failure: %v", err)
	}
}

func TestStatePathRejectsLeafSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target", "state.json")
	if err := SaveState(target, State{"repo": "original"}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "state.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(link, func(string) error { return nil }); err == nil {
		t.Fatal("state load followed a symlink at the state-file leaf")
	}
	if err := saveState(link, State{"repo": "replacement"}, func(string) error { return nil }); err == nil {
		t.Fatal("state save accepted a symlink at the state-file leaf")
	}
	state, err := LoadState(target)
	if err != nil || state["repo"] != "original" {
		t.Fatalf("leaf-symlink rejection changed target state: %#v, %v", state, err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("leaf symlink was replaced: info=%v err=%v", info, err)
	}
}

func assertSyncOrder(t *testing.T, synced []string, paths ...string) {
	t.Helper()
	lastIndex := -1
	for _, path := range paths {
		foundIndex := -1
		for index := lastIndex + 1; index < len(synced); index++ {
			if synced[index] == path {
				foundIndex = index
				break
			}
		}
		if foundIndex < 0 {
			t.Fatalf("sync calls %v do not contain %q after index %d", synced, path, lastIndex)
		}
		lastIndex = foundIndex
	}
}

func TestSaveStateRenameFailureKeepsDestinationAndRemovesTemporaryFile(t *testing.T) {
	parent := t.TempDir()
	destination := filepath.Join(parent, "state.json")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(destination, State{"repo": "new"}); err == nil {
		t.Fatal("state rename over existing directory unexpectedly succeeded")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" || !entries[0].IsDir() {
		t.Fatalf("failed atomic save left temporary or replaced destination: entries=%v", entries)
	}
}
