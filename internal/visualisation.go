package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type VizAxis struct {
	Name       string   `json:"name"`
	Flag       string   `json:"flag"`
	Values     []string `json:"values"`
	Toggleable bool     `json:"toggleable"`
}

type VizCombo struct {
	Key  string
	Args []string
}

type Visualization struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	VizScript string    `json:"viz_script"`
	DataPath  string    `json:"data_path"`
	Axes      []VizAxis `json:"axes"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (v *Visualization) ToggleableAxes() []VizAxis {
	var out []VizAxis
	for _, ax := range v.Axes {
		if ax.Toggleable {
			out = append(out, ax)
		}
	}
	return out
}

func (v *Visualization) SVGCount() int {
	axes := v.ToggleableAxes()
	if len(axes) == 0 {
		return 1
	}
	n := 1
	for _, ax := range axes {
		if len(ax.Values) > 0 {
			n *= len(ax.Values)
		}
	}
	return n
}

// ComboKey returns a filesystem-safe key for a given axis name → value string selection.
func (v *Visualization) ComboKey(selection map[string]string) string {
	axes := v.ToggleableAxes()
	if len(axes) == 0 {
		return "default"
	}
	parts := make([]string, len(axes))
	for i, ax := range axes {
		val := selection[ax.Name]
		idx := 0
		for j, v := range ax.Values {
			if v == val {
				idx = j
				break
			}
		}
		parts[i] = fmt.Sprintf("%d", idx)
	}
	return strings.Join(parts, "-")
}

func (v *Visualization) DefaultSelection() map[string]string {
	sel := map[string]string{}
	for _, ax := range v.ToggleableAxes() {
		if len(ax.Values) > 0 {
			sel[ax.Name] = ax.Values[0]
		}
	}
	return sel
}

func (v *Visualization) AllCombos() []VizCombo {
	toggleable := v.ToggleableAxes()

	var fixedArgs []string
	for _, ax := range v.Axes {
		if !ax.Toggleable && len(ax.Values) > 0 {
			fixedArgs = append(fixedArgs, ax.Flag, ax.Values[0])
		}
	}

	if len(toggleable) == 0 {
		return []VizCombo{{Key: "default", Args: fixedArgs}}
	}

	indexCombos := vizCartesianIndices(toggleable)
	var result []VizCombo
	for _, idxCombo := range indexCombos {
		parts := make([]string, len(idxCombo))
		for i, idx := range idxCombo {
			parts[i] = fmt.Sprintf("%d", idx)
		}
		key := strings.Join(parts, "-")

		args := append([]string{}, fixedArgs...)
		for i, ax := range toggleable {
			args = append(args, ax.Flag, ax.Values[idxCombo[i]])
		}
		result = append(result, VizCombo{Key: key, Args: args})
	}
	return result
}

func vizCartesianIndices(axes []VizAxis) [][]int {
	if len(axes) == 0 {
		return [][]int{{}}
	}
	rest := vizCartesianIndices(axes[1:])
	var result [][]int
	for i := range axes[0].Values {
		for _, r := range rest {
			combo := make([]int, 0, 1+len(r))
			combo = append(combo, i)
			combo = append(combo, r...)
			result = append(result, combo)
		}
	}
	return result
}

func vizDir(cachePath, projectID, vizID string) string {
	return filepath.Join(cachePath, projectID, "visualizations", vizID)
}

func vizFile(cachePath, projectID, vizID string) string {
	return filepath.Join(vizDir(cachePath, projectID, vizID), "viz.json")
}

func VizSVGPath(cachePath, projectID, vizID, key string) string {
	return filepath.Join(vizDir(cachePath, projectID, vizID), "svgs", key+".svg")
}

func SaveVisualization(cachePath string, viz *Visualization) error {
	dir := vizDir(cachePath, viz.ProjectID, viz.ID)
	if err := os.MkdirAll(filepath.Join(dir, "svgs"), 0755); err != nil {
		return fmt.Errorf("cannot create viz directory: %w", err)
	}
	data, err := json.MarshalIndent(viz, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot marshal viz: %w", err)
	}
	if err := os.WriteFile(vizFile(cachePath, viz.ProjectID, viz.ID), data, 0644); err != nil {
		return fmt.Errorf("cannot write viz.json: %w", err)
	}
	return nil
}

func LoadVisualization(cachePath, projectID, vizID string) (*Visualization, error) {
	data, err := os.ReadFile(vizFile(cachePath, projectID, vizID))
	if err != nil {
		return nil, fmt.Errorf("cannot read viz.json: %w", err)
	}
	var viz Visualization
	if err := json.Unmarshal(data, &viz); err != nil {
		return nil, fmt.Errorf("cannot parse viz.json: %w", err)
	}
	return &viz, nil
}

func LoadVisualizations(cachePath, projectID string) ([]*Visualization, error) {
	pattern := filepath.Join(cachePath, projectID, "visualizations", "*", "viz.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("cannot glob visualizations: %w", err)
	}
	var vizs []*Visualization
	for _, match := range matches {
		vizID := filepath.Base(filepath.Dir(match))
		viz, err := LoadVisualization(cachePath, projectID, vizID)
		if err != nil {
			continue
		}
		vizs = append(vizs, viz)
	}
	sort.Slice(vizs, func(i, j int) bool {
		return vizs[i].CreatedAt.Before(vizs[j].CreatedAt)
	})
	return vizs, nil
}

func DeleteVisualization(cachePath, projectID, vizID string) error {
	return os.RemoveAll(vizDir(cachePath, projectID, vizID))
}
