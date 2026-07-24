package projects

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
	"gopkg.in/yaml.v3"
)

// ProjectAPI is the JSON representation of a project for the HTTP API.
type ProjectAPI struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Color        string    `json:"color"` // theme color key from ProjectColors
	Instructions *string   `json:"instructions"` // null when no instructions.md
	Memory       *string   `json:"memory"`       // null when no MEMORY.md
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// SessionCount is populated at HTTP-list time by the daemon (which owns the
	// session index); the package leaves it zero.
	SessionCount int `json:"session_count"`
}

// ToAPI converts a loaded Project into its API shape.
func (p *Project) ToAPI() *ProjectAPI {
	api := &ProjectAPI{
		ID:          p.ID,
		Name:        p.Name,
		Description: p.Description,
		Color:       p.Color,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
	if p.Instructions != "" {
		v := p.Instructions
		api.Instructions = &v
	}
	if p.Memory != "" {
		v := p.Memory
		api.Memory = &v
	}
	return api
}

// ProjectCreateRequest is the POST /projects body.
type ProjectCreateRequest struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Color        string `json:"color,omitempty"` // empty → random palette color
	Instructions string `json:"instructions,omitempty"`
	Memory       string `json:"memory,omitempty"`
}

// Validate checks a create request.
func (r *ProjectCreateRequest) Validate() error {
	if err := ValidateProjectName(r.Name); err != nil {
		return err
	}
	if len(r.Description) > maxProjectDescriptionLen {
		return fmt.Errorf("project description exceeds %d characters", maxProjectDescriptionLen)
	}
	if r.Color != "" && !isValidColor(r.Color) {
		return fmt.Errorf("invalid project color %q", r.Color)
	}
	return nil
}

// ProjectUpdateRequest is the PUT /projects/{id} body. Pointer fields
// distinguish "absent" (leave unchanged) from "present" (set, possibly to "").
type ProjectUpdateRequest struct {
	Name         *string `json:"name,omitempty"`
	Description  *string `json:"description,omitempty"`
	Color        *string `json:"color,omitempty"`
	Instructions *string `json:"instructions,omitempty"`
	Memory       *string `json:"memory,omitempty"`
}

// CreateProject writes a new project directory under projectsDir and returns
// the generated id. Timestamps are stamped to now.
func CreateProject(projectsDir string, req *ProjectCreateRequest) (*Project, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	id, err := GenerateProjectSlug(projectsDir)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(projectsDir, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	color := req.Color
	if color == "" {
		color = randomColor() // macOS-style: a new project gets a random theme color
	}
	now := time.Now().UTC()
	meta := projectMeta{
		Name:        strings.TrimSpace(req.Name),
		Description: strings.TrimSpace(req.Description),
		Color:       color,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := writeMeta(projectsDir, id, meta); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if req.Instructions != "" {
		if err := WriteProjectInstructions(projectsDir, id, req.Instructions); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
	}
	if req.Memory != "" {
		if err := WriteProjectMemory(projectsDir, id, req.Memory); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
	}
	return LoadProject(projectsDir, id)
}

// UpdateProject applies a partial update to an existing project. Only the
// present (non-nil) fields are written; UpdatedAt is bumped when any field
// changes.
func UpdateProject(projectsDir, id string, req *ProjectUpdateRequest) (*Project, error) {
	p, err := LoadProject(projectsDir, id)
	if err != nil {
		return nil, err
	}
	metaChanged := false
	if req.Name != nil {
		if err := ValidateProjectName(*req.Name); err != nil {
			return nil, err
		}
		p.Name = strings.TrimSpace(*req.Name)
		metaChanged = true
	}
	if req.Description != nil {
		if len(*req.Description) > maxProjectDescriptionLen {
			return nil, fmt.Errorf("project description exceeds %d characters", maxProjectDescriptionLen)
		}
		p.Description = strings.TrimSpace(*req.Description)
		metaChanged = true
	}
	if req.Color != nil {
		if !isValidColor(*req.Color) {
			return nil, fmt.Errorf("invalid project color %q", *req.Color)
		}
		p.Color = *req.Color
		metaChanged = true
	}
	if req.Instructions != nil {
		if err := WriteProjectInstructions(projectsDir, id, *req.Instructions); err != nil {
			return nil, err
		}
		metaChanged = true
	}
	if req.Memory != nil {
		if err := WriteProjectMemory(projectsDir, id, *req.Memory); err != nil {
			return nil, err
		}
		metaChanged = true
	}
	if metaChanged {
		meta := projectMeta{
			Name:        p.Name,
			Description: p.Description,
			Color:       p.Color,
			CreatedAt:   p.CreatedAt,
			UpdatedAt:   time.Now().UTC(),
		}
		if err := writeMeta(projectsDir, id, meta); err != nil {
			return nil, err
		}
	}
	return LoadProject(projectsDir, id)
}

func writeMeta(projectsDir, id string, meta projectMeta) error {
	dir := filepath.Join(projectsDir, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal project meta: %w", err)
	}
	return atomicWrite(filepath.Join(dir, metaFile), data)
}

// WriteProjectInstructions writes instructions.md atomically; an empty string
// removes the file.
func WriteProjectInstructions(projectsDir, id, instructions string) error {
	path := filepath.Join(projectsDir, id, instructionsFile)
	if strings.TrimSpace(instructions) == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicWrite(path, []byte(instructions))
}

// WriteProjectMemory writes MEMORY.md as a full replace (user edit); an empty
// string removes the file. The agent's incremental auto-accumulation writes the
// SAME file via ctxwin.BoundedAppend, which serializes on <dir>/MEMORY.md.lock.
// This path takes that identical flock so a user save and a concurrent
// agent append cannot clobber each other (last-writer-wins data loss).
func WriteProjectMemory(projectsDir, id, memory string) error {
	dir := filepath.Join(projectsDir, id)
	path := filepath.Join(dir, memoryFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	// Acquire the same exclusive lock BoundedAppend uses for this MEMORY.md.
	lockPath := path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open memory lock: %w", err)
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return fmt.Errorf("flock memory: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd()) //nolint:errcheck

	if strings.TrimSpace(memory) == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return atomicWrite(path, []byte(memory))
}

// DeleteProjectDir removes the entire project directory. The daemon's delete
// handler first permanently deletes the sessions filed under this project
// (destructive, ChatGPT/Claude-style); the run path also guards on project
// existence so any straggler with a dangling project_id degrades to "unfiled".
func DeleteProjectDir(projectsDir, id string) error {
	if err := ValidateProjectID(id); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(projectsDir, id))
}

// atomicWrite writes data to path via a temp file + rename.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}
