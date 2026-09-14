// Package skills discovers SKILL.md definitions and exposes a concurrency-safe,
// read-only catalog to the agent runtime. It never executes code from a skill.
package skills

import "sync"

const (
	DefaultMaxFileBytes int64 = 256 * 1024
	DefaultMaxSkills          = 500
)

// Source describes where a skill definition came from.
type Source struct {
	Kind  string // project | custom | user | compatibility
	Label string // stable human-readable provider label
	Root  string // scanned root
}

// Skill is discovery metadata. The body is deliberately not retained in the
// prompt catalog; it is read again only when the skill is invoked.
type Skill struct {
	Name                   string
	Description            string
	FilePath               string
	BaseDir                string
	Source                 Source
	DisableModelInvocation bool
	UserInvocable          bool
	AllowedTools           []string
}

// Warning is a non-fatal discovery or parsing problem.
type Warning struct {
	Path    string
	Message string
}

// Compatibility controls opt-in discovery of other harness layouts.
type Compatibility struct {
	Claude bool
	Codex  bool
	Agents bool
}

// Options is the complete, immutable discovery policy used by Manager.Reload.
type Options struct {
	CWD               string
	UserSkillsDir     string
	UserHome          string
	CustomDirectories []string
	Include           []string
	Ignore            []string
	Compatibility     Compatibility
	MaxFileBytes      int64
	MaxSkills         int
}

type snapshot struct {
	list   []Skill
	byName map[string]Skill
}

// Manager owns the active catalog. Reload builds a complete replacement before
// taking the lock, so readers never observe a partially discovered skill set.
type Manager struct {
	mu       sync.RWMutex
	opts     Options
	snapshot snapshot
	warnings []Warning
}

// NewManager discovers the initial catalog.
func NewManager(opts Options) (*Manager, error) {
	opts = normalizeOptions(opts)
	snap, warnings, err := discover(opts)
	if err != nil {
		return nil, err
	}
	return &Manager{opts: opts, snapshot: snap, warnings: warnings}, nil
}

func normalizeOptions(opts Options) Options {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = DefaultMaxFileBytes
	}
	if opts.MaxSkills <= 0 {
		opts.MaxSkills = DefaultMaxSkills
	}
	opts.CustomDirectories = append([]string(nil), opts.CustomDirectories...)
	opts.Include = append([]string(nil), opts.Include...)
	opts.Ignore = append([]string(nil), opts.Ignore...)
	return opts
}

// Reload atomically replaces the active catalog. On a fatal discovery error the
// previous snapshot remains active.
func (m *Manager) Reload() ([]Warning, error) {
	snap, warnings, err := discover(m.opts)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.snapshot, m.warnings = snap, warnings
	m.mu.Unlock()
	return append([]Warning(nil), warnings...), nil
}

// List returns a stable copy of the active skills.
func (m *Manager) List() []Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Skill, len(m.snapshot.list))
	for i, skill := range m.snapshot.list {
		out[i] = cloneSkill(skill)
	}
	return out
}

// Get resolves a skill name case-insensitively.
func (m *Manager) Get(name string) (Skill, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.snapshot.byName[normalizeName(name)]
	return cloneSkill(s), ok
}

// Warnings returns the warnings from the most recent successful discovery.
func (m *Manager) Warnings() []Warning {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Warning(nil), m.warnings...)
}

func cloneSkill(skill Skill) Skill {
	skill.AllowedTools = append([]string(nil), skill.AllowedTools...)
	return skill
}
