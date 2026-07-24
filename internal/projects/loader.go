// Package projects implements the "Project" entity: a lightweight container
// that groups sessions and carries project-scoped instructions + memory.
//
// A project is orthogonal to an agent. An agent is a reusable capability
// (e.g. a trip planner) that can be used inside many projects; a project is
// the box a conversation's sessions, deliverables, and memory belong to. A
// session references its owning project by id (session.ProjectID); when set,
// the agent loop layers the project's instructions and memory on top of the
// global + agent tiers.
//
// On-disk layout (mirrors the agents container, minus the config/skills
// machinery):
//
//	<ShannonDir>/projects/<id>/
//	    project.yaml    — metadata (name, description, timestamps)
//	    instructions.md — project-scoped instructions (optional)
//	    MEMORY.md       — project-scoped memory, auto-accumulated + user-editable (optional)
//
// The id is an opaque slug ("proj-<hex>") — users pick a display name, never
// the id, exactly like agent slugs.
package projects

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// projectIDRe matches both generated slugs ("proj-<hex>") and any
// client-supplied id. Kept identical to the agent-name rule so the two
// container namespaces validate consistently.
var projectIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

const (
	metaFile         = "project.yaml"
	instructionsFile = "instructions.md"
	memoryFile       = "MEMORY.md"

	maxProjectNameLen        = 200
	maxProjectDescriptionLen = 2000
)

// ProjectColors is the fixed macOS-Finder-style palette a project's theme color
// is drawn from (semantic keys; the renderer maps each to a hue for light/dark).
// A new project gets a random one by default; the user can change it.
var ProjectColors = []string{"red", "orange", "yellow", "green", "blue", "purple", "gray"}

func isValidColor(c string) bool {
	for _, v := range ProjectColors {
		if v == c {
			return true
		}
	}
	return false
}

// randomColor picks a palette color using crypto/rand (math/rand-free, matching
// the slug generator). Falls back to the first color on a rand failure.
func randomColor() string {
	b := make([]byte, 1)
	if _, err := rand.Read(b); err != nil {
		return ProjectColors[0]
	}
	return ProjectColors[int(b[0])%len(ProjectColors)]
}

// Project is a fully-loaded project entity.
type Project struct {
	ID           string
	Name         string
	Description  string
	Color        string
	Instructions string
	Memory       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// projectMeta is the persisted project.yaml shape.
type projectMeta struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description,omitempty"`
	Color       string    `yaml:"color,omitempty"`
	CreatedAt   time.Time `yaml:"created_at"`
	UpdatedAt   time.Time `yaml:"updated_at"`
}

// ValidateProjectID checks that id is a syntactically valid project id.
func ValidateProjectID(id string) error {
	if !projectIDRe.MatchString(id) {
		return fmt.Errorf("invalid project id %q: must match %s", id, projectIDRe.String())
	}
	return nil
}

// ValidateProjectName checks a user-supplied display name.
func ValidateProjectName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("project name is not valid UTF-8")
	}
	if utf8.RuneCountInString(name) > maxProjectNameLen {
		return fmt.Errorf("project name exceeds %d characters", maxProjectNameLen)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("project name contains a control character")
		}
	}
	return nil
}

// GenerateProjectSlug returns a fresh, unused project slug "proj-<6 hex>".
// Retries on the vanishingly small chance of a directory collision.
func GenerateProjectSlug(projectsDir string) (string, error) {
	for i := 0; i < 10; i++ {
		b := make([]byte, 3)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		slug := "proj-" + hex.EncodeToString(b)
		if _, err := os.Stat(filepath.Join(projectsDir, slug, metaFile)); os.IsNotExist(err) {
			return slug, nil
		}
	}
	return "", fmt.Errorf("could not generate a unique project slug after 10 attempts")
}

// LoadProject reads a project from <projectsDir>/<id>. The presence of
// project.yaml defines a project directory; instructions.md and MEMORY.md are
// optional.
func LoadProject(projectsDir, id string) (*Project, error) {
	if err := ValidateProjectID(id); err != nil {
		return nil, err
	}
	dir := filepath.Join(projectsDir, id)
	metaData, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return nil, fmt.Errorf("project %q: missing %s: %w", id, metaFile, err)
	}
	var meta projectMeta
	if err := yaml.Unmarshal(metaData, &meta); err != nil {
		return nil, fmt.Errorf("project %q: bad %s: %w", id, metaFile, err)
	}

	color := meta.Color
	if !isValidColor(color) {
		color = ProjectColors[len(ProjectColors)-1] // legacy/invalid → gray
	}
	p := &Project{
		ID:          id,
		Name:        meta.Name,
		Description: meta.Description,
		Color:       color,
		CreatedAt:   meta.CreatedAt,
		UpdatedAt:   meta.UpdatedAt,
	}
	if data, err := os.ReadFile(filepath.Join(dir, instructionsFile)); err == nil {
		p.Instructions = string(data)
	}
	if data, err := os.ReadFile(filepath.Join(dir, memoryFile)); err == nil {
		p.Memory = string(data)
	}
	return p, nil
}

// ListProjects returns every project under projectsDir, most-recently-updated
// first. A missing projectsDir yields an empty list, not an error.
func ListProjects(projectsDir string) ([]*Project, error) {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Project
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if err := ValidateProjectID(id); err != nil {
			continue // skip stray/hidden dirs
		}
		if _, err := os.Stat(filepath.Join(projectsDir, id, metaFile)); err != nil {
			continue
		}
		p, err := LoadProject(projectsDir, id)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// MemoryDir returns the directory whose MEMORY.md holds this project's memory.
// It is the value handed to instructions.LoadMemoryFrom (read/injection) and to
// the memory-append write path (auto-accumulation) so both sides agree on where
// project memory lives.
func MemoryDir(projectsDir, id string) string {
	return filepath.Join(projectsDir, id)
}

// InstructionsDir returns the directory containing this project's
// instructions.md, for the instructions loader's project-entity tier.
func InstructionsDir(projectsDir, id string) string {
	return filepath.Join(projectsDir, id)
}
