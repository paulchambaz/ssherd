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

type Visualization struct {
	ID                 string          `json:"id"`
	ProjectID          string          `json:"project_id"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	VizCommand         string          `json:"viz_command"`
	DataPath           string          `json:"data_path"`
	OutputFileTemplate string          `json:"output_file_template"`
	BuildRemote        bool            `json:"build_remote"`
	Axes               []VizAxis       `json:"axes"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	InputArgument      string          `json:"input_argument,omitempty"`
	InputPath          string          `json:"input_path,omitempty"`
	OutputArgument     string          `json:"output_argument,omitempty"`
	FormState          json.RawMessage `json:"form_state,omitempty"`
}

type VizAxis struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

type VizCombo struct {
	Key  string
	Args []string
}

func (v *Visualization) ToggleableAxes() []VizAxis {
	return v.Axes
}

func (v *Visualization) SVGCount() int {
	if len(v.Axes) == 0 {
		return 1
	}
	n := 1
	for _, ax := range v.Axes {
		if len(ax.Values) > 0 {
			n *= len(ax.Values)
		}
	}
	return n
}

func (v *Visualization) DefaultSelection() map[string]string {
	sel := map[string]string{}
	for _, ax := range v.Axes {
		if len(ax.Values) > 0 {
			sel[ax.Name] = ax.Values[0]
		}
	}
	return sel
}

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

func (v *Visualization) AllCombos() []VizCombo {
	if len(v.Axes) == 0 {
		return []VizCombo{{Key: "default", Args: nil}}
	}

	indexCombos := vizCartesianIndices(v.Axes)
	var result []VizCombo
	for _, idxCombo := range indexCombos {
		parts := make([]string, len(idxCombo))
		for i, idx := range idxCombo {
			parts[i] = fmt.Sprintf("%d", idx)
		}
		key := strings.Join(parts, "-")

		args := make([]string, 0, len(idxCombo))
		for i, ax := range v.Axes {
			args = append(args, ax.Values[idxCombo[i]])
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

// VizLocalOutputPath retourne le chemin absolu local pour un combo donné.
// Le template est relatif au repo local (ex: "results/figures/fig_{combo_key}.svg").
func VizLocalOutputPath(localRepoDir, template, comboKey string) string {
	path := strings.ReplaceAll(template, "{combo_key}", comboKey)
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(localRepoDir, path)
}

// VizRemoteTmpPath retourne le chemin temporaire sur le remote NFS pour un combo.
// remoteBase est le parent du remoteProjectPath (là où vit .ssherd/).
func VizRemoteTmpPath(remoteBase, vizID, comboKey, ext string) string {
	return filepath.Join(remoteBase, ".ssherd", "viz", vizID, comboKey+ext)
}

// VizOutputExt extrait l'extension du template de sortie (".svg", ".png"...).
func VizOutputExt(template string) string {
	ext := filepath.Ext(template)
	if ext == "" {
		return ".svg"
	}
	return ext
}

// ResolveToLocal convertit un chemin absolu remote (sous remoteProjectPath)
// vers son équivalent dans le repo local.
func ResolveToLocal(remoteAbsPath, remoteProjectPath, localRepoDir string) string {
	clean := strings.TrimSuffix(remoteProjectPath, "/")
	if strings.HasPrefix(remoteAbsPath, clean+"/") {
		rel := strings.TrimPrefix(remoteAbsPath, clean+"/")
		return filepath.Join(localRepoDir, rel)
	}
	if remoteAbsPath == clean {
		return localRepoDir
	}
	// chemin déjà relatif ou inconnu : on le colle au repo
	return filepath.Join(localRepoDir, remoteAbsPath)
}

func vizDir(cachePath, projectID, vizID string) string {
	return filepath.Join(cachePath, projectID, "visualizations", vizID)
}

func vizFile(cachePath, projectID, vizID string) string {
	return filepath.Join(vizDir(cachePath, projectID, vizID), "viz.json")
}

func SaveVisualization(cachePath string, viz *Visualization) error {
	dir := vizDir(cachePath, viz.ProjectID, viz.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
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

	seen := make(map[string]bool)
	deduped := vizs[:0]
	for _, v := range vizs {
		if !seen[v.ID] {
			seen[v.ID] = true
			deduped = append(deduped, v)
		}
	}
	vizs = deduped

	sort.Slice(vizs, func(i, j int) bool {
		return vizs[i].CreatedAt.Before(vizs[j].CreatedAt)
	})
	return vizs, nil
}

func DeleteVisualization(cachePath, projectID, vizID string) error {
	return os.RemoveAll(vizDir(cachePath, projectID, vizID))
}

func (v *Visualization) ResolveOutputPath(localRepoDir string, selection map[string]string) string {
	for _, combo := range v.AllCombos() {
		comboSel := map[string]string{}
		for i, ax := range v.Axes {
			if i < len(combo.Args) {
				comboSel[ax.Name] = combo.Args[i]
			}
		}
		match := true
		for name, val := range selection {
			if comboSel[name] != val {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		resolved := buildVizCommand(v.OutputFileTemplate, combo, v)
		if filepath.IsAbs(resolved) {
			return resolved
		}
		return filepath.Join(localRepoDir, resolved)
	}
	// fallback
	key := v.ComboKey(selection)
	return VizLocalOutputPath(localRepoDir, v.OutputFileTemplate, key)
}

func VizLocalPNGPath(svgPath string) string {
	ext := filepath.Ext(svgPath)
	if ext == "" {
		return svgPath + ".png"
	}
	return svgPath[:len(svgPath)-len(ext)] + ".png"
}
