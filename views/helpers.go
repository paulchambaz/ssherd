package views

import (
	"encoding/json"
	"fmt"

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
		axes = append(axes, axisJS{Name: ax.Name, Values: ax.Values})
	}
	b, _ := json.Marshal(axes)
	return string(b)
}

func formatFileSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func vizCommandTemplate(viz *internal.Visualization, p *internal.Project) string {
    cmd := viz.VizCommand
    if viz.InputArgument != "" && viz.InputPath != "" {
        cmd += " " + viz.InputArgument + " " + viz.InputPath
    }
    if viz.OutputArgument != "" && viz.OutputFileTemplate != "" {
        cmd += " " + viz.OutputArgument + " " + viz.OutputFileTemplate
    }
    for _, ax := range viz.Axes {
        cmd += " {" + ax.Name + "}"
    }
    return cmd
}
