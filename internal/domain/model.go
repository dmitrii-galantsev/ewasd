package domain

import "time"

const SchemaVersion = 1

type State struct {
	SchemaVersion int       `json:"schema_version"`
	Revision      uint64    `json:"revision"`
	Projects      []Project `json:"projects"`
	Activity      []Event   `json:"activity"`
}

type Project struct {
	ID               string    `json:"id"`
	SourceID         string    `json:"source_id"`
	Name             string    `json:"name"`
	Root             string    `json:"root"`
	GitRoot          string    `json:"git_root"`
	Remote           string    `json:"remote,omitempty"`
	LegacySourceRoot string    `json:"legacy_source_root,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Entries          []Entry   `json:"entries"`
}

type Entry struct {
	Path      string    `json:"path"`
	Kind      string    `json:"kind"`
	SourceRel string    `json:"source_rel"`
	CreatedAt time.Time `json:"created_at"`
}

type Event struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id,omitempty"`
	Action    string    `json:"action"`
	Path      string    `json:"path,omitempty"`
	Summary   string    `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
}

type Snapshot struct {
	SchemaVersion int           `json:"schema_version"`
	Revision      uint64        `json:"revision"`
	DataRoot      string        `json:"data_root"`
	Projects      []ProjectView `json:"projects"`
	Activity      []Event       `json:"activity"`
	Recovery      []Journal     `json:"recovery"`
}

type ProjectView struct {
	Project
	EntriesView    []EntryView `json:"entries_view"`
	Health         Health      `json:"health"`
	GitIgnoreOK    bool        `json:"git_ignore_ok"`
	GitIgnoreState string      `json:"git_ignore_state"`
}

type EntryView struct {
	Entry
	Source string `json:"source"`
	Target string `json:"target"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type Health struct {
	Linked        int `json:"linked"`
	Missing       int `json:"missing"`
	Conflicts     int `json:"conflicts"`
	SourceMissing int `json:"source_missing"`
	Total         int `json:"total"`
}

type Plan struct {
	ID                string     `json:"id"`
	Fingerprint       string     `json:"fingerprint"`
	Action            string     `json:"action"`
	ProjectID         string     `json:"project_id"`
	ProjectName       string     `json:"project_name"`
	TemplateProjectID string     `json:"template_project_id,omitempty"`
	TargetRoot        string     `json:"target_root,omitempty"`
	GitRoot           string     `json:"git_root,omitempty"`
	Remote            string     `json:"remote,omitempty"`
	DetectionMethod   string     `json:"detection_method,omitempty"`
	NewProject        bool       `json:"new_project,omitempty"`
	ExpectedRevision  uint64     `json:"expected_revision"`
	Safe              bool       `json:"safe"`
	Summary           string     `json:"summary"`
	Steps             []PlanStep `json:"steps"`
	Conflicts         []Conflict `json:"conflicts"`
	Guarantees        []string   `json:"guarantees"`
	CreatedAt         time.Time  `json:"created_at"`
}

type DetectionResult struct {
	Matched           bool                 `json:"matched"`
	Method            string               `json:"method,omitempty"`
	Confidence        string               `json:"confidence,omitempty"`
	CWD               string               `json:"cwd"`
	GitRoot           string               `json:"git_root,omitempty"`
	TargetRoot        string               `json:"target_root,omitempty"`
	Remote            string               `json:"remote,omitempty"`
	Remotes           []string             `json:"remotes"`
	ProjectID         string               `json:"project_id,omitempty"`
	TemplateProjectID string               `json:"template_project_id,omitempty"`
	ProjectName       string               `json:"project_name,omitempty"`
	SourceID          string               `json:"source_id,omitempty"`
	Trace             []string             `json:"trace"`
	Candidates        []DetectionCandidate `json:"candidates"`
}

type DetectionCandidate struct {
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	SourceID    string `json:"source_id"`
	TargetRoot  string `json:"target_root"`
	Method      string `json:"method"`
	Reason      string `json:"reason"`
}

type CleanPlan struct {
	ID                  string               `json:"id"`
	Fingerprint         string               `json:"fingerprint"`
	ExpectedRevision    uint64               `json:"expected_revision"`
	ProjectID           string               `json:"project_id"`
	ProjectName         string               `json:"project_name"`
	Root                string               `json:"root"`
	GitRoot             string               `json:"git_root"`
	Scope               string               `json:"scope"`
	DetectionMethod     string               `json:"detection_method"`
	Mode                string               `json:"mode"`
	IncludeDirectories  bool                 `json:"include_directories"`
	Candidates          []string             `json:"candidates"`
	ProtectedPatterns   []string             `json:"protected_patterns"`
	ProtectedPaths      []string             `json:"protected_paths"`
	HealthyPaths        []string             `json:"healthy_paths"`
	HealthyLinks        []CleanProtectedLink `json:"healthy_links"`
	SkippedRepositories []string             `json:"skipped_repositories"`
	Command             []string             `json:"command"`
	CreatedAt           time.Time            `json:"created_at"`
}

type CleanProtectedLink struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

type PlanStep struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Detail string `json:"detail"`
}

type Conflict struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

type ApplyResult struct {
	OK          bool     `json:"ok"`
	Outcome     string   `json:"outcome"`
	OperationID string   `json:"operation_id,omitempty"`
	Revision    uint64   `json:"revision"`
	Action      string   `json:"action"`
	Changed     []string `json:"changed"`
	Skipped     []string `json:"skipped"`
	Warnings    []string `json:"warnings"`
	Summary     string   `json:"summary"`
}

type Journal struct {
	ID           string    `json:"id"`
	Action       string    `json:"action"`
	Phase        string    `json:"phase"`
	ProjectID    string    `json:"project_id"`
	SourceID     string    `json:"source_id,omitempty"`
	Path         string    `json:"path"`
	Source       string    `json:"source"`
	Target       string    `json:"target"`
	Stage        string    `json:"stage,omitempty"`
	Backup       string    `json:"backup,omitempty"`
	Archive      string    `json:"archive,omitempty"`
	LegacySource string    `json:"legacy_source,omitempty"`
	ProjectRoot  string    `json:"project_root,omitempty"`
	ExpectedRev  uint64    `json:"expected_revision"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type LegacyMigrationPlan struct {
	LegacyWorkspace string              `json:"legacy_workspace"`
	ScanRoots       []string            `json:"scan_roots"`
	Projects        []LegacyProjectPlan `json:"projects"`
	Markers         []LegacyMarkerPlan  `json:"markers"`
	Skipped         []LegacySkippedItem `json:"skipped"`
}

type LegacyProjectPlan struct {
	Name       string            `json:"name"`
	Root       string            `json:"root"`
	GitRoot    string            `json:"git_root"`
	Remote     string            `json:"remote,omitempty"`
	SourceRoot string            `json:"source_root"`
	Entries    []LegacyEntryPlan `json:"entries"`
}

type LegacyEntryPlan struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	LegacySource string `json:"legacy_source"`
	Target       string `json:"target"`
}

type LegacyMarkerPlan struct {
	Path     string   `json:"path"`
	GitRoot  string   `json:"git_root"`
	Entries  []string `json:"entries"`
	Residual []string `json:"residual"`
}

type LegacySkippedItem struct {
	Marker   string `json:"marker,omitempty"`
	Path     string `json:"path,omitempty"`
	Reason   string `json:"reason"`
	Blocking bool   `json:"blocking"`
}

func NewState() State {
	return State{SchemaVersion: SchemaVersion, Projects: []Project{}, Activity: []Event{}}
}

func (s *State) ProjectByID(id string) *Project {
	for i := range s.Projects {
		if s.Projects[i].ID == id {
			return &s.Projects[i]
		}
	}
	return nil
}

func (s *State) ProjectByRoot(root string) *Project {
	for i := range s.Projects {
		if s.Projects[i].Root == root {
			return &s.Projects[i]
		}
	}
	return nil
}

func (p *Project) EntryByPath(path string) *Entry {
	for i := range p.Entries {
		if p.Entries[i].Path == path {
			return &p.Entries[i]
		}
	}
	return nil
}

func (s *State) AddEvent(event Event) {
	s.Activity = append([]Event{event}, s.Activity...)
	if len(s.Activity) > 200 {
		s.Activity = s.Activity[:200]
	}
}
