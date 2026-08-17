package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
)

var (
	ErrStaleRevision       = errors.New("state revision changed; create a fresh plan")
	ErrBusy                = errors.New("state is busy with another operation")
	ErrDurabilityUncertain = errors.New("atomic replacement succeeded but directory sync failed")
	ErrCommitIncomplete    = errors.New("state committed but post-commit effects are incomplete")
)

type Effects struct {
	Rollback func() error
	Commit   func() error
}

type Store struct {
	root string
	mu   sync.Mutex
}

func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve data root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create data root: %w", err)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve data root symlinks: %w", err)
	}
	for _, dir := range []string{"profiles", "archive", "transactions", "recovery"} {
		if err := os.MkdirAll(filepath.Join(abs, dir), 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return &Store{root: abs}, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) Read() (domain.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.lock(false)
	if err != nil {
		return domain.State{}, err
	}
	defer unlock(lock)
	return s.load()
}

// WithExclusive runs a recovery/maintenance callback while holding the same
// process and cross-process lock used by manifest transactions. It does not
// change the manifest revision.
func (s *Store) WithExclusive(fn func(domain.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.lock(true)
	if err != nil {
		return err
	}
	defer unlock(lock)
	state, err := s.load()
	if err != nil {
		return err
	}
	return fn(state)
}

// Transact serializes filesystem and manifest changes under the same
// cross-process lock. The callback must return rollback effects for any
// filesystem mutation it has completed. The manifest is synced and atomically
// renamed before Commit cleanup runs.
func (s *Store) Transact(
	expected *uint64,
	fn func(*domain.State) (Effects, error),
) (domain.State, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.lock(true)
	if err != nil {
		return domain.State{}, nil, err
	}
	defer unlock(lock)

	state, err := s.load()
	if err != nil {
		return domain.State{}, nil, err
	}
	if expected != nil && state.Revision != *expected {
		return state, nil, fmt.Errorf("%w: planned revision %d, current revision %d", ErrStaleRevision, *expected, state.Revision)
	}

	effects, err := fn(&state)
	if err != nil {
		if effects.Rollback != nil {
			if rollbackErr := effects.Rollback(); rollbackErr != nil {
				return state, nil, errors.Join(err, fmt.Errorf("rollback failed: %w", rollbackErr))
			}
		}
		return state, nil, err
	}

	state.Revision++
	if err := s.save(state); err != nil {
		if errors.Is(err, ErrDurabilityUncertain) {
			// The new manifest is already visible. Rolling the filesystem back
			// would make that committed manifest false. Leave the journal and
			// backups intact so Recover can converge deterministically.
			return state, []string{"manifest was replaced but its directory sync could not be confirmed; recovery is required"}, fmt.Errorf("persist manifest: %w", err)
		}
		if effects.Rollback != nil {
			if rollbackErr := effects.Rollback(); rollbackErr != nil {
				return state, nil, errors.Join(err, fmt.Errorf("manifest save and rollback failed: %w", rollbackErr))
			}
		}
		return state, nil, fmt.Errorf("persist manifest: %w", err)
	}

	warnings := []string{}
	if effects.Commit != nil {
		if err := effects.Commit(); err != nil {
			warnings = append(warnings, "state committed; recovery or reconciliation is required: "+err.Error())
			return state, warnings, fmt.Errorf("%w at revision %d: %v", ErrCommitIncomplete, state.Revision, err)
		}
	}
	return state, warnings, nil
}

func (s *Store) lock(exclusive bool) (*os.File, error) {
	path := filepath.Join(s.root, "state.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	mode := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		mode = syscall.LOCK_EX | syscall.LOCK_NB
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Flock(int(f.Fd()), mode)
		if err == nil {
			return f, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = f.Close()
			return nil, fmt.Errorf("lock state: %w", err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, ErrBusy
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func (s *Store) load() (domain.State, error) {
	path := filepath.Join(s.root, "state.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return domain.NewState(), nil
	}
	if err != nil {
		return domain.State{}, fmt.Errorf("read manifest: %w", err)
	}
	var state domain.State
	if err := json.Unmarshal(data, &state); err != nil {
		return domain.State{}, fmt.Errorf("decode manifest: %w", err)
	}
	if state.SchemaVersion != domain.SchemaVersion {
		return domain.State{}, fmt.Errorf("unsupported manifest schema %d", state.SchemaVersion)
	}
	if state.Projects == nil {
		state.Projects = []domain.Project{}
	}
	for index := range state.Projects {
		if state.Projects[index].SourceID == "" {
			state.Projects[index].SourceID = state.Projects[index].ID
		}
	}
	if state.Activity == nil {
		state.Activity = []domain.Event{}
	}
	if err := validateState(state); err != nil {
		return domain.State{}, fmt.Errorf("validate manifest: %w", err)
	}
	return state, nil
}

func (s *Store) save(state domain.State) error {
	if err := validateState(state); err != nil {
		return fmt.Errorf("refuse invalid manifest: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return AtomicWrite(filepath.Join(s.root, "state.json"), data, 0o600)
}

func validateState(state domain.State) error {
	projectIDs := map[string]bool{}
	projectRoots := map[string]bool{}
	for _, project := range state.Projects {
		if project.ID == "" || strings.ContainsAny(project.ID, `/\\`) || project.ID == "." || project.ID == ".." {
			return fmt.Errorf("invalid project id %q", project.ID)
		}
		if project.SourceID == "" || strings.ContainsAny(project.SourceID, `/\\`) || project.SourceID == "." || project.SourceID == ".." {
			return fmt.Errorf("invalid source id %q", project.SourceID)
		}
		if projectIDs[project.ID] {
			return fmt.Errorf("duplicate project id %q", project.ID)
		}
		projectIDs[project.ID] = true
		if !filepath.IsAbs(project.Root) || !filepath.IsAbs(project.GitRoot) {
			return fmt.Errorf("project %q roots must be absolute", project.ID)
		}
		if projectRoots[project.Root] {
			return fmt.Errorf("duplicate project root %q", project.Root)
		}
		for existingRoot := range projectRoots {
			if rootsOverlap(existingRoot, project.Root) {
				return fmt.Errorf("project root %q overlaps %q", project.Root, existingRoot)
			}
		}
		projectRoots[project.Root] = true
		entryPaths := map[string]bool{}
		for _, entry := range project.Entries {
			clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(entry.Path)))
			if entry.Path == "" || clean != entry.Path || entry.Path == "." || entry.Path == ".." || strings.HasPrefix(entry.Path, "../") || filepath.IsAbs(entry.Path) {
				return fmt.Errorf("project %q has unsafe entry path %q", project.ID, entry.Path)
			}
			if entryPaths[entry.Path] {
				return fmt.Errorf("project %q has duplicate entry %q", project.ID, entry.Path)
			}
			for _, component := range strings.Split(entry.Path, "/") {
				if component == ".git" {
					return fmt.Errorf("project %q manages forbidden Git control path %q", project.ID, entry.Path)
				}
			}
			entryPaths[entry.Path] = true
			expectedSource := filepath.ToSlash(filepath.Join("profiles", project.SourceID, "files", filepath.FromSlash(entry.Path)))
			if entry.SourceRel != expectedSource {
				return fmt.Errorf("project %q entry %q has unexpected source %q", project.ID, entry.Path, entry.SourceRel)
			}
			if entry.Kind != "file" && entry.Kind != "directory" {
				return fmt.Errorf("project %q entry %q has invalid kind %q", project.ID, entry.Path, entry.Kind)
			}
		}
	}
	return nil
}

func rootsOverlap(left, right string) bool {
	contains := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
	}
	return contains(left, right) || contains(right, left)
}

func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ewasd-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		_ = tmp.Close()
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	removeTemp = false
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("%w: open parent: %v", ErrDurabilityUncertain, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("%w: sync parent: %v", ErrDurabilityUncertain, err)
	}
	return nil
}
