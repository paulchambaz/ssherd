package views

import (
	"encoding/json"

	"github.com/paulchambaz/ssherd/internal"
)

func countJobsByStatus(jobs []*internal.Job) (active, done, failed int) {
	for _, j := range jobs {
		switch j.Status {
		case internal.JobRunning, internal.JobPending:
			active++
		case internal.JobDone:
			done++
		case internal.JobFailed, internal.JobStalled:
			failed++
		}
	}
	return
}

func vizToggleAxesJSON(viz *internal.Visualization) string {
	type axisJS struct {
		Name   string   `json:"name"`
		Values []string `json:"values"`
	}
	var axes []axisJS
	for _, ax := range viz.Axes {
		if ax.Toggleable {
			axes = append(axes, axisJS{Name: ax.Name, Values: ax.Values})
		}
	}
	b, _ := json.Marshal(axes)
	return string(b)
}
