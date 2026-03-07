package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type Job struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"project_id"`
	ProjectSlug string     `json:"project_slug"`
	DisplayName string     `json:"display_name"`
	Status      JobStatus  `json:"status"`
	Machine     string     `json:"machine,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	RetryCount  int        `json:"retry_count"`
	MaxRetries  int        `json:"max_retries"`

	RunCommand   string `json:"run_command"`
	RetryCommand string `json:"retry_command"`

	LogPath     string   `json:"log_path"`
	OutputPath  string   `json:"output_path"`
	NfsJobDir   string   `json:"nfs_job_dir"`
	OutputFiles []string `json:"output_files,omitempty"`

	Progress        *JobProgress    `json:"progress,omitempty"`
	GPURequirements GPURequirements `json:"gpu_requirements"`

	FormState json.RawMessage `json:"form_state,omitempty"`
}

type JobProgress struct {
	CurrentStep int       `json:"current_step"`
	TotalSteps  int       `json:"total_steps"`
	StartTime   time.Time `json:"start_time"`
	CurrentTime time.Time `json:"current_time"`
	Percent     float64   `json:"percent"`
	ETASeconds  float64   `json:"eta_seconds"`
	LastUpdated time.Time `json:"last_updated"`
}

type GPURequirements struct {
	MinVRAMMB    int    `json:"min_vram_mb"`
	PreferredGPU string `json:"preferred_gpu"`
}

type JobStatus string

const (
	JobPending   JobStatus = "pending"
	JobRunning   JobStatus = "running"
	JobDone      JobStatus = "done"
	JobFailed    JobStatus = "failed"
	JobStalled   JobStatus = "stalled"
	JobCancelled JobStatus = "cancelled"
)

func (s JobStatus) Label() string {
	switch s {
	case JobPending:
		return "Pending"
	case JobRunning:
		return "Running"
	case JobDone:
		return "Done"
	case JobFailed:
		return "Failed"
	case JobStalled:
		return "Stalled"
	case JobCancelled:
		return "Cancelled"
	default:
		return string(s)
	}
}

func (s JobStatus) BadgeClass() string {
	switch s {
	case JobRunning:
		return "bg-blue-100 text-blue-700"
	case JobDone:
		return "bg-green-100 text-green-700"
	case JobFailed:
		return "bg-red-100 text-red-700"
	case JobStalled:
		return "bg-yellow-100 text-yellow-700"
	default:
		return "bg-base-100 text-base-500"
	}
}

func jobDir(cachePath, projectID, jobID string) string {
	return filepath.Join(cachePath, projectID, "jobs", jobID)
}

func jobFile(cachePath, projectID, jobID string) string {
	return filepath.Join(jobDir(cachePath, projectID, jobID), "job.json")
}

func SaveJob(cachePath string, j *Job) error {
	dir := jobDir(cachePath, j.ProjectID, j.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create job directory: %w", err)
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot marshal job: %w", err)
	}
	if err := os.WriteFile(jobFile(cachePath, j.ProjectID, j.ID), data, 0644); err != nil {
		return fmt.Errorf("cannot write job.json: %w", err)
	}
	return nil
}

func LoadJob(cachePath, projectID, jobID string) (*Job, error) {
	data, err := os.ReadFile(jobFile(cachePath, projectID, jobID))
	if err != nil {
		return nil, fmt.Errorf("cannot read job.json: %w", err)
	}
	var j Job
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("cannot parse job.json: %w", err)
	}
	return &j, nil
}

func LoadJobs(cachePath, projectID string) ([]*Job, error) {
	pattern := filepath.Join(cachePath, projectID, "jobs", "*", "job.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("cannot glob jobs: %w", err)
	}
	var jobs []*Job
	for _, match := range matches {
		jobID := filepath.Base(filepath.Dir(match))
		j, err := LoadJob(cachePath, projectID, jobID)
		if err != nil {
			continue
		}
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, k int) bool {
		return jobs[i].CreatedAt.After(jobs[k].CreatedAt)
	})
	return jobs, nil
}

func CartesianProduct(axes [][]string) [][]string {
	if len(axes) == 0 {
		return [][]string{{}}
	}
	rest := CartesianProduct(axes[1:])
	var result [][]string
	for _, v := range axes[0] {
		for _, r := range rest {
			combo := make([]string, 0, 1+len(r))
			combo = append(combo, v)
			combo = append(combo, r...)
			result = append(result, combo)
		}
	}
	return result
}
