package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
)

func TestTransactionPersistsAtomicallyAndRejectsStaleRevision(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := s.Transact(nil, func(state *domain.State) (Effects, error) {
		state.Projects = append(state.Projects, domain.Project{ID: "one", SourceID: "one", Name: "One", Root: t.TempDir(), GitRoot: t.TempDir(), Entries: []domain.Entry{}})
		return Effects{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 1 {
		t.Fatalf("revision = %d, want 1", state.Revision)
	}
	stale := uint64(0)
	_, _, err = s.Transact(&stale, func(state *domain.State) (Effects, error) {
		t.Fatal("stale transaction callback must not run")
		return Effects{}, nil
	})
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("expected stale revision, got %v", err)
	}
	loaded, err := s.Read()
	if err != nil || loaded.Revision != 1 || len(loaded.Projects) != 1 {
		t.Fatalf("unexpected persisted state: %+v, %v", loaded, err)
	}
	data, err := os.ReadFile(filepath.Join(s.Root(), "state.json"))
	if err != nil || len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("manifest is not a complete newline-terminated file: %v", err)
	}
}

func TestDistinctStoreInstancesSerializeWithFileLock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := first.Transact(nil, func(state *domain.State) (Effects, error) {
			close(entered)
			<-release
			return Effects{}, nil
		})
		firstDone <- err
	}()
	<-entered
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		_, _, err := second.Transact(nil, func(state *domain.State) (Effects, error) {
			close(secondEntered)
			return Effects{}, nil
		})
		secondDone <- err
	}()
	select {
	case <-secondEntered:
		t.Fatal("second Store entered while the first held the file lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("second Store did not enter after file lock release")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestCommitFailureIsReturnedAfterManifestIsDurable(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, warnings, err := s.Transact(nil, func(state *domain.State) (Effects, error) {
		state.AddEvent(domain.Event{ID: "event", Action: "test", Summary: "durable"})
		return Effects{Commit: func() error { return errors.New("injected commit failure") }}, nil
	})
	if !errors.Is(err, ErrCommitIncomplete) || state.Revision != 1 || len(warnings) != 1 {
		t.Fatalf("commit failure was not surfaced: state=%+v warnings=%v err=%v", state, warnings, err)
	}
	loaded, err := s.Read()
	if err != nil || loaded.Revision != 1 || len(loaded.Activity) != 1 {
		t.Fatalf("manifest was not durable before commit failure: %+v, %v", loaded, err)
	}
}

func TestReadRejectsTamperedSourcePath(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	state := domain.NewState()
	state.Projects = []domain.Project{{
		ID: "project", SourceID: "project", Name: "Project", Root: root, GitRoot: root,
		Entries: []domain.Entry{{Path: "safe.txt", Kind: "file", SourceRel: "../../outside"}},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(s.Root(), "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(); err == nil {
		t.Fatal("tampered source path was accepted")
	}
}

func TestTransactionRollsBackFilesystemEffectWhenManifestMutationFails(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(s.Root(), "marker")
	_, _, err = s.Transact(nil, func(state *domain.State) (Effects, error) {
		if err := os.WriteFile(marker, []byte("temporary"), 0o600); err != nil {
			return Effects{}, err
		}
		return Effects{Rollback: func() error { return os.Remove(marker) }}, errors.New("injected failure")
	})
	if err == nil {
		t.Fatal("expected injected failure")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("rollback did not remove marker: %v", err)
	}
}
