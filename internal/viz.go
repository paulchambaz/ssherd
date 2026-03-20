package internal

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var reStripInvalid = regexp.MustCompile(`[^a-zA-Z0-9\-\_\.]+`)
var reCollapseUnder = regexp.MustCompile(`_+`)

// vizLoop tourne toutes les VizInterval et régénère les visualisations périmées.
// Chaque combo est lancé dans une goroutine dédiée via vizTickOne — la boucle
// principale ne bloque plus pendant l'exécution des scripts.
func (s *Scheduler) vizLoop() {
	for {
		time.Sleep(s.getConfig().VizInterval)
		s.vizTick()
	}
}

func (s *Scheduler) vizTick() {
	projects, err := LoadProjects(s.cachePath)
	if err != nil {
		log.Printf("viz: load projects: %v", err)
		return
	}
	for _, project := range projects {
		s.vizTickProject(project)
	}
}

func (s *Scheduler) vizTickProject(project *Project) {
	vizs, err := LoadVisualizations(s.cachePath, project.ID)
	if err != nil || len(vizs) == 0 {
		return
	}

	s.mu.RLock()
	var projectJobs []*Job
	for _, j := range s.jobs {
		if j.ProjectID == project.ID {
			projectJobs = append(projectJobs, j)
		}
	}
	s.mu.RUnlock()

	hasRelevantJob := false
	for _, j := range projectJobs {
		if j.Status == JobRunning || j.Status == JobDone || j.Status == JobFailed {
			hasRelevantJob = true
			break
		}
	}
	if !hasRelevantJob {
		return
	}

	localRepoDir := filepath.Join(s.cachePath, project.ID, "repo")

	for _, viz := range vizs {
		if viz.OutputFileTemplate == "" {
			continue
		}
		s.vizTickOne(project, viz, projectJobs, localRepoDir)
	}
}

// vizTickOne lance en arrière-plan la régénération de chaque combo périmé.
// Elle retourne immédiatement : chaque combo est une goroutine indépendante
// qui émet EventVizDone à la fin (succès ou erreur).
func (s *Scheduler) vizTickOne(project *Project, viz *Visualization, jobs []*Job, localRepoDir string) {
	var localDataPath string
	if viz.InputPath != "" {
		localDataPath = filepath.Join(localRepoDir, viz.InputPath)
	} else {
		localDataPath = ResolveToLocal(viz.DataPath, project.RemotePath, localRepoDir)
	}

	for _, combo := range viz.AllCombos() {
		// Use buildVizCommand so {version} is substituted consistently with generation.
		resolvedTpl := buildVizCommand(viz.OutputFileTemplate, combo, viz)
		outputPath := filepath.Join(localRepoDir, resolvedTpl)

		if !vizNeedsRegen(outputPath, localDataPath) {
			continue
		}

		if !s.tryMarkVizGenerating(viz.ID, combo.Key) {
			continue
		}

		combo := combo
		go func() {
			defer s.clearVizGenerating(viz.ID, combo.Key)
			err := s.generateVizLocal(project, viz, combo, localRepoDir)
			vizErr := ""
			if err != nil {
				vizErr = err.Error()
				log.Printf("viz: tick %s/%s combo %s: %v", project.Slug, viz.Name, combo.Key, err)
			}
			s.emitViz(viz.ID, project.ID, project.Slug, combo.Key, vizErr)
		}()
	}
}

// GenerateVizNow est appelé par le handler HTTP pour une régénération manuelle.
// Elle lance chaque combo dans une goroutine, émet immédiatement un événement
// "generating" pour chacun, puis un EventVizDone à la fin. Elle retourne sans
// attendre la fin des générations.
func (s *Scheduler) GenerateVizNow(project *Project, viz *Visualization, mode string) {
	localRepoDir := filepath.Join(s.cachePath, project.ID, "repo")

	// mode est ignoré — toutes les visualisations sont générées en local.
	// Les données sont rapatriées périodiquement depuis les machines distantes.
	if mode == "local" {
		s.mu.RLock()
		var projectJobs []*Job
		for _, j := range s.jobs {
			if j.ProjectID == project.ID {
				projectJobs = append(projectJobs, j)
			}
		}
		s.mu.RUnlock()
		if runningJobsWriteToVizData(projectJobs, viz, project) {
			log.Printf("viz: WARNING: force-local requested for %s but jobs are still writing to data — proceeding anyway", viz.Name)
		}
	}

	for _, combo := range viz.AllCombos() {
		combo := combo

		if !s.tryMarkVizGenerating(viz.ID, combo.Key) {
			s.emitViz(viz.ID, project.ID, project.Slug, combo.Key, "generating")
			continue
		}

		s.emitViz(viz.ID, project.ID, project.Slug, combo.Key, "generating")

		go func() {
			defer s.clearVizGenerating(viz.ID, combo.Key)

			err := s.generateVizLocal(project, viz, combo, localRepoDir)

			vizErr := ""
			if err != nil {
				vizErr = err.Error()
				log.Printf("viz: GenerateVizNow %s combo %s: %v", viz.Name, combo.Key, err)
			}
			s.emitViz(viz.ID, project.ID, project.Slug, combo.Key, vizErr)
		}()
	}
}

// VizJobsWriting indique si des jobs running écrivent sur les données de cette viz.
func (s *Scheduler) VizJobsWriting(project *Project, viz *Visualization) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var jobs []*Job
	for _, j := range s.jobs {
		if j.ProjectID == project.ID {
			jobs = append(jobs, j)
		}
	}
	return runningJobsWriteToVizData(jobs, viz, project)
}

func (s *Scheduler) generateVizLocal(project *Project, viz *Visualization, combo VizCombo, localRepoDir string) error {
	if err := localGitCloneOrPull(project, localRepoDir); err != nil {
		return fmt.Errorf("git pull before local viz: %w", err)
	}

	prefix := s.getConfig().LocalPrefix

	baseCmd := viz.VizCommand

	// Append --input resolved to absolute local path. No variable substitution needed.
	if viz.InputArgument != "" && viz.InputPath != "" {
		resolvedInput := filepath.Join(localRepoDir, viz.InputPath)
		baseCmd += " " + viz.InputArgument + " " + resolvedInput
	}

	// Append --output with the full template path. {version} and axis vars will be
	// substituted by buildVizCommand inside buildAndRun.
	if viz.OutputArgument != "" && viz.OutputFileTemplate != "" {
		resolvedOutputTpl := filepath.Join(localRepoDir, viz.OutputFileTemplate)
		baseCmd += " " + viz.OutputArgument + " " + resolvedOutputTpl
	}

	buildAndRun := func(vizCmd string) error {
		resolvedCmd := buildVizCommand(vizCmd, combo, viz)
		var axisParts []string
		for _, arg := range combo.Args {
			for _, part := range strings.Fields(arg) {
				axisParts = append(axisParts, shellEscape(part))
			}
		}
		if prefix != "" {
			resolvedCmd = prefix + " " + resolvedCmd
		}
		cmdStr := fmt.Sprintf("cd %s && %s %s",
			shellEscape(localRepoDir),
			resolvedCmd,
			strings.Join(axisParts, " "),
		)
		cmd := exec.Command("sh", "-c", cmdStr)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("cmd: %s\noutput: %s", cmdStr, out)
		}
		return nil
	}

	var svgVizCmd, pngVizCmd string
	if strings.Contains(baseCmd, ".png") {
		pngVizCmd = baseCmd
		svgVizCmd = strings.ReplaceAll(baseCmd, ".png", ".svg")
	} else {
		svgVizCmd = baseCmd
		pngVizCmd = strings.ReplaceAll(baseCmd, ".svg", ".png")
	}

	if err := buildAndRun(svgVizCmd); err != nil {
		return fmt.Errorf("local viz SVG failed: %w", err)
	}
	if err := buildAndRun(pngVizCmd); err != nil {
		log.Printf("viz: PNG generation failed for %s combo %s: %v", viz.Name, combo.Key, err)
		return fmt.Errorf("SVG ok, PNG failed: %w", err)
	}

	log.Printf("viz: generated local %s combo %s", viz.Name, combo.Key)
	return nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func runningJobsWriteToVizData(jobs []*Job, viz *Visualization, project *Project) bool {
	for _, job := range jobs {
		if job.Status != JobRunning {
			continue
		}
		if pathsOverlap(job.OutputPath, viz.DataPath) {
			return true
		}
		for _, f := range job.OutputFiles {
			if pathsOverlap(f, viz.DataPath) {
				return true
			}
		}
	}
	return false
}

func pathsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a = strings.TrimSuffix(a, "/")
	b = strings.TrimSuffix(b, "/")
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func vizNeedsRegen(outputPath, dataDir string) bool {
	outInfo, err := os.Stat(outputPath)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return true
	}
	outMod := outInfo.ModTime()

	needsRegen := false
	filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || needsRegen {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(outMod) {
			needsRegen = true
		}
		return nil
	})
	return needsRegen
}

func buildVizCommand(vizCommand string, combo VizCombo, viz *Visualization) string {
	versionParts := make([]string, 0, len(viz.Axes))
	for i := range viz.Axes {
		val := comboValueForAxis(combo, viz, i)
		versionParts = append(versionParts, SanitizeAxisValue(val))
	}
	version := strings.Join(versionParts, "_")
	if version == "" {
		version = "default"
	}

	vars := map[string]string{
		"version": version,
	}
	for i, ax := range viz.Axes {
		vars[ax.Name] = SanitizeAxisValue(comboValueForAxis(combo, viz, i))
	}

	cmd := vizCommand
	for k, v := range vars {
		cmd = strings.ReplaceAll(cmd, "{"+k+"}", v)
	}
	return cmd
}

func comboValueForAxis(combo VizCombo, viz *Visualization, axisIdx int) string {
	if axisIdx >= len(combo.Args) {
		return ""
	}
	return combo.Args[axisIdx]
}
