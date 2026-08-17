package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dmitrii-galantsev/ewasd/internal/domain"
	"github.com/dmitrii-galantsev/ewasd/internal/fsops"
	"github.com/dmitrii-galantsev/ewasd/internal/gitutil"
	"github.com/dmitrii-galantsev/ewasd/internal/store"
)

func (e *Engine) journalPath(id string) string {
	return filepath.Join(e.store.Root(), "transactions", id+".json")
}

func (e *Engine) writeJournal(journal domain.Journal) error {
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return store.AtomicWrite(e.journalPath(journal.ID), data, 0o600)
}

func (e *Engine) removeJournal(id string) error {
	err := os.Remove(e.journalPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Join(e.store.Root(), "transactions"))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (e *Engine) listJournals() ([]domain.Journal, error) {
	dir := filepath.Join(e.store.Root(), "transactions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	result := []domain.Journal{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var journal domain.Journal
		if err := json.Unmarshal(data, &journal); err != nil {
			return nil, fmt.Errorf("decode journal %s: %w", entry.Name(), err)
		}
		filenameID := strings.TrimSuffix(entry.Name(), ".json")
		if journal.ID != filenameID || journal.ID == "" || len(journal.ID) > 128 || strings.ContainsAny(journal.ID, `/\\`) {
			return nil, fmt.Errorf("journal %s has an invalid or mismatched id", entry.Name())
		}
		result = append(result, journal)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

// Recover conservatively resolves journals left by an interrupted process. It
// never overwrites an unrelated target. Uncertain source copies are retained
// under recovery/ for manual inspection.
func (e *Engine) Recover() ([]string, error) {
	messages := []string{}
	pending, err := e.listJournals()
	if err != nil || len(pending) == 0 {
		return messages, err
	}
	for _, queued := range pending {
		message := ""
		_, _, err := e.store.Transact(nil, func(state *domain.State) (store.Effects, error) {
			journals, err := e.listJournals()
			if err != nil {
				return store.Effects{}, err
			}
			var current *domain.Journal
			for i := range journals {
				if journals[i].ID == queued.ID {
					current = &journals[i]
					break
				}
			}
			if current == nil {
				return store.Effects{}, nil
			}
			message, err = e.recoverOne(state, *current)
			return store.Effects{}, err
		})
		if message != "" {
			messages = append(messages, message)
		}
		if err != nil {
			return messages, err
		}
	}
	return messages, nil
}

func (e *Engine) recoverOne(state *domain.State, journal domain.Journal) (string, error) {
	if err := e.validateJournal(*state, journal); err != nil {
		return "", fmt.Errorf("refuse unsafe journal %s: %w", journal.ID, err)
	}
	project := state.ProjectByID(journal.ProjectID)
	managed := project != nil && project.EntryByPath(journal.Path) != nil
	message := ""
	summary := ""
	switch journal.Action {
	case "adopt":
		if managed {
			if !fsopsLink(journal.Target, journal.Source) {
				return "", errors.New("adoption is committed in the manifest but its target changed; backup and journal were retained for manual recovery")
			}
			if err := os.RemoveAll(journal.Backup); err != nil {
				return "", err
			}
			if err := e.removeJournal(journal.ID); err != nil {
				return "", err
			}
			if err := gitutil.UpdateExclude(project.ID, project.Root, project.GitRoot, entryPaths(project.Entries)); err != nil {
				return "", err
			}
			message, summary = "completed cleanup for adopted "+journal.Path, "Completed interrupted adoption cleanup"
			break
		}
		if err := e.rollbackAdopt(journal, true); err != nil {
			return "", err
		}
		if err := gitutil.UpdateExclude(project.ID, project.Root, project.GitRoot, entryPaths(project.Entries)); err != nil {
			return "", err
		}
		message = "rolled back interrupted adoption of " + journal.Path + " and retained its copied source"
		summary = "Rolled back interrupted adoption and retained its copied source"
	case "detach":
		if !managed {
			if err := e.removeJournal(journal.ID); err != nil {
				return "", err
			}
			message, summary = "completed cleanup for detached "+journal.Path, "Completed interrupted detach cleanup"
			break
		}
		if err := e.rollbackDetach(journal); err != nil {
			return "", err
		}
		message, summary = "rolled back interrupted detach of "+journal.Path, "Rolled back interrupted detach"
	case "reconcile":
		if managed && fsopsLink(journal.Target, journal.Source) {
			if err := e.removeJournal(journal.ID); err != nil {
				return "", err
			}
			message, summary = "verified restored link "+journal.Path, "Verified interrupted reconcile link"
			break
		}
		if managed {
			if _, targetErr := os.Lstat(journal.Target); errors.Is(targetErr, os.ErrNotExist) {
				if err := e.removeJournal(journal.ID); err != nil {
					return "", err
				}
				message = "cleared interrupted reconcile before link creation for " + journal.Path
				summary = "Cleared reconcile journal before link creation"
				break
			}
			return "", errors.New("restored-link journal target is occupied by unexpected content; it was retained for manual recovery")
		}
		return "", errors.New("restored-link journal no longer matches the manifest and target; it was retained for manual recovery")
	case "legacy-import":
		if managed {
			if err := e.completeLegacySwitch(journal); err != nil {
				return "", err
			}
			if err := gitutil.UpdateExclude(project.ID, project.Root, project.GitRoot, entryPaths(project.Entries)); err != nil {
				return "", err
			}
			if err := fsops.RemoveAny(journal.Backup); err != nil {
				return "", err
			}
			if err := e.removeJournal(journal.ID); err != nil {
				return "", err
			}
			message, summary = "completed legacy import for "+journal.Path, "Completed interrupted legacy import"
			break
		}
		if err := e.rollbackLegacySwitch(journal); err != nil {
			return "", err
		}
		sourceID := journal.SourceID
		if sourceID == "" {
			sourceID = journal.ProjectID
		}
		profileRoot := filepath.Join(e.store.Root(), "profiles", sourceID)
		remaining, _ := e.listJournals()
		hasProjectJournals := false
		for _, candidate := range remaining {
			if candidate.ProjectID == journal.ProjectID && candidate.Action == "legacy-import" {
				hasProjectJournals = true
				break
			}
		}
		if !hasProjectJournals {
			sourceInUse := false
			for _, existing := range state.Projects {
				if existing.SourceID == sourceID {
					sourceInUse = true
					break
				}
			}
			if !sourceInUse {
				_ = os.RemoveAll(profileRoot)
			}
		}
		message, summary = "rolled back legacy import for "+journal.Path, "Rolled back interrupted legacy import"
	default:
		return "", errors.New("unknown transaction journal action; journal was retained")
	}
	state.AddEvent(domain.Event{ID: randomID(8), ProjectID: journal.ProjectID, Action: "recover", Path: journal.Path, Summary: summary, CreatedAt: e.now()})
	return message, nil
}

// DiscardJournal removes a recovery blocker without touching any source,
// target, backup, or archive path. The journal itself is retained under
// recovery/discarded for forensic inspection.
func (e *Engine) DiscardJournal(id string) (string, error) {
	if id == "" || len(id) > 128 || strings.ContainsAny(id, `/\\`) {
		return "", errors.New("invalid journal id")
	}
	destination := ""
	err := e.store.WithExclusive(func(_ domain.State) error {
		journals, err := e.listJournals()
		if err != nil {
			return err
		}
		found := false
		for _, journal := range journals {
			if journal.ID == id {
				found = true
				break
			}
		}
		if !found {
			return ErrNotFound
		}
		dir := filepath.Join(e.store.Root(), "recovery", "discarded")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		destination = filepath.Join(dir, id+".json")
		if _, err := os.Lstat(destination); err == nil {
			return errors.New("discarded journal archive already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(e.journalPath(id), destination); err != nil {
			return err
		}
		return errors.Join(fsops.SyncDir(filepath.Join(e.store.Root(), "transactions")), fsops.SyncDir(dir))
	})
	return destination, err
}

func (e *Engine) validateJournal(state domain.State, journal domain.Journal) error {
	project := state.ProjectByID(journal.ProjectID)
	if project == nil && journal.Action != "legacy-import" {
		return errors.New("recorded project no longer exists")
	}
	path, err := fsops.ValidateRelative(journal.Path)
	if err != nil {
		return err
	}
	projectRoot := journal.ProjectRoot
	if project != nil {
		projectRoot = project.Root
	}
	if projectRoot == "" {
		return errors.New("journal has no project root")
	}
	sourceID := journal.SourceID
	if project != nil {
		sourceID = project.SourceID
	}
	if sourceID == "" {
		sourceID = journal.ProjectID
	}
	expectedSource, err := fsops.SafeTarget(e.store.Root(), e.sourceRel(sourceID, path))
	if err != nil {
		return err
	}
	expectedTarget, err := fsops.SafeTarget(projectRoot, path)
	if err != nil {
		return err
	}
	if filepath.Clean(journal.Source) != filepath.Clean(expectedSource) || filepath.Clean(journal.Target) != filepath.Clean(expectedTarget) {
		return errors.New("source or target falls outside the recorded project mapping")
	}
	switch journal.Action {
	case "adopt":
		if journal.Phase != "copying" && journal.Phase != "source-durable" && journal.Phase != "target-backed-up" && journal.Phase != "linked" {
			return errors.New("invalid adopt journal phase")
		}
		expectedStage := filepath.Join(e.store.Root(), "transactions", journal.ID+".stage")
		expectedBackup := filepath.Join(filepath.Dir(expectedTarget), ".ewasd-"+journal.ID+".backup")
		if filepath.Clean(journal.Stage) != filepath.Clean(expectedStage) || filepath.Clean(journal.Backup) != filepath.Clean(expectedBackup) {
			return errors.New("stage or backup path is outside the expected operation locations")
		}
	case "detach":
		if journal.Phase != "materializing" && journal.Phase != "materialized" && journal.Phase != "source-archived" && journal.Phase != "detached" {
			return errors.New("invalid detach journal phase")
		}
		expectedStage := filepath.Join(filepath.Dir(expectedTarget), ".ewasd-"+journal.ID+".materialize")
		expectedArchive := filepath.Join(e.store.Root(), "archive", journal.ID, project.ID, filepath.FromSlash(path))
		if filepath.Clean(journal.Stage) != filepath.Clean(expectedStage) || filepath.Clean(journal.Archive) != filepath.Clean(expectedArchive) {
			return errors.New("stage or archive path is outside the expected operation locations")
		}
	case "reconcile":
		if journal.Phase != "prepared" {
			return errors.New("invalid reconcile journal phase")
		}
		if journal.Stage != "" || journal.Backup != "" || journal.Archive != "" {
			return errors.New("reconcile journal contains unexpected backup paths")
		}
	case "legacy-import":
		if journal.Phase != "prepared" && journal.Phase != "legacy-backed-up" && journal.Phase != "switched" {
			return errors.New("invalid legacy import journal phase")
		}
		if journal.LegacySource == "" || !filepath.IsAbs(journal.LegacySource) {
			return errors.New("legacy import journal has an invalid legacy source")
		}
		if filepath.Dir(journal.Backup) != filepath.Dir(expectedTarget) || !strings.HasPrefix(filepath.Base(journal.Backup), ".ewasd-migrate-") || !strings.HasSuffix(journal.Backup, ".link") {
			return errors.New("legacy import backup is not an expected target sibling")
		}
		if journal.Stage != "" || journal.Archive != "" {
			return errors.New("legacy import journal contains unexpected stage or archive paths")
		}
	default:
		return errors.New("unknown journal action")
	}
	return nil
}

func fsopsLink(target, source string) bool {
	return fsops.LinkPointsTo(target, source)
}
