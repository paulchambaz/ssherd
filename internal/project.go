package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Project struct {
	ID         string    `json:"id"`
	Slug       string    `json:"slug"`
	Name       string    `json:"name"`
	RemotePath string    `json:"remote_path"`
	GitRepo    string    `json:"git_repo"`
	Branch     string    `json:"branch"`
	GitToken   string    `json:"git_token"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func LoadProjects(cachePath string) ([]*Project, error) {
	pattern := filepath.Join(cachePath, "*", "project.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("cannot glob projects: %w", err)
	}
	var projects []*Project
	for _, match := range matches {
		id := filepath.Base(filepath.Dir(match))
		p, err := LoadProject(cachePath, id)
		if err != nil {
			continue
		}
		projects = append(projects, p)
	}
	return projects, nil
}

func SaveProject(cachePath string, p *Project) error {
	dir := projectDir(cachePath, p.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create project directory: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot marshal project: %w", err)
	}
	if err := os.WriteFile(projectFile(cachePath, p.ID), data, 0644); err != nil {
		return fmt.Errorf("cannot write project.json: %w", err)
	}
	return nil
}

func LoadProject(cachePath, id string) (*Project, error) {
	data, err := os.ReadFile(projectFile(cachePath, id))
	if err != nil {
		return nil, fmt.Errorf("cannot read project.json: %w", err)
	}
	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("cannot parse project.json: %w", err)
	}
	return &p, nil
}

func DeleteProject(cachePath, id string) error {
	dir := projectDir(cachePath, id)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("cannot delete project directory: %w", err)
	}
	return nil
}

func projectDir(cachePath, slug string) string {
	return filepath.Join(cachePath, slug)
}

func projectFile(cachePath, slug string) string {
	return filepath.Join(projectDir(cachePath, slug), "project.json")
}
