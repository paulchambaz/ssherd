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
	jobsWriting := runningJobsWriteToVizData(jobs, viz, project)
	localDataPath := ResolveToLocal(viz.DataPath, project.RemotePath, localRepoDir)

	for _, combo := range viz.AllCombos() {
		outputPath := VizLocalOutputPath(localRepoDir, viz.OutputFileTemplate, combo.Key)

		if !vizNeedsRegen(outputPath, localDataPath) {
			continue
		}

		if !s.tryMarkVizGenerating(viz.ID, combo.Key) {
			// Déjà en cours de génération par une autre goroutine, on saute.
			continue
		}

		combo := combo    // capture pour la goroutine
		jw := jobsWriting // capture pour la goroutine
		go func() {
			defer s.clearVizGenerating(viz.ID, combo.Key)

			var err error
			if jw {
				err = s.generateVizRemote(project, viz, combo, localRepoDir)
			} else {
				err = s.generateVizLocal(project, viz, combo, localRepoDir)
			}

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

	s.mu.RLock()
	var projectJobs []*Job
	for _, j := range s.jobs {
		if j.ProjectID == project.ID {
			projectJobs = append(projectJobs, j)
		}
	}
	s.mu.RUnlock()

	jobsWriting := runningJobsWriteToVizData(projectJobs, viz, project)

	useRemote := viz.BuildRemote
	switch mode {
	case "local":
		useRemote = false
		if jobsWriting {
			log.Printf("viz: WARNING: force-local requested for %s but jobs are still writing to data — proceeding anyway", viz.Name)
		}
	case "remote":
		useRemote = true
	default: // "auto"
		if jobsWriting {
			useRemote = true
		}
	}

	for _, combo := range viz.AllCombos() {
		combo := combo
		ur := useRemote

		if !s.tryMarkVizGenerating(viz.ID, combo.Key) {
			// Déjà en cours — on signale quand même "generating" pour que l'UI
			// affiche le spinner si ce combo est la sélection courante.
			s.emitViz(viz.ID, project.ID, project.Slug, combo.Key, "generating")
			continue
		}

		// Signal immédiat : le spinner s'affiche avant même que la goroutine démarre.
		s.emitViz(viz.ID, project.ID, project.Slug, combo.Key, "generating")

		go func() {
			defer s.clearVizGenerating(viz.ID, combo.Key)

			var err error
			if ur {
				err = s.generateVizRemote(project, viz, combo, localRepoDir)
			} else {
				err = s.generateVizLocal(project, viz, combo, localRepoDir)
			}

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
	if strings.Contains(viz.VizCommand, ".png") {
		pngVizCmd = viz.VizCommand
		svgVizCmd = strings.ReplaceAll(viz.VizCommand, ".png", ".svg")
	} else {
		svgVizCmd = viz.VizCommand
		pngVizCmd = strings.ReplaceAll(viz.VizCommand, ".svg", ".png")
	}

	if err := buildAndRun(svgVizCmd); err != nil {
		return fmt.Errorf("local viz SVG failed: %w", err)
	}
	if err := buildAndRun(pngVizCmd); err != nil {
		log.Printf("viz: PNG generation failed for %s combo %s: %v", viz.Name, combo.Key, err)
	}

	log.Printf("viz: generated local %s combo %s", viz.Name, combo.Key)
	return nil
}

func (s *Scheduler) generateVizRemote(project *Project, viz *Visualization, combo VizCombo, localRepoDir string) error {
	machine, proxy, err := s.findMachineForRsync()
	if err != nil {
		return fmt.Errorf("no machine available for remote viz: %w", err)
	}

	client, err := Connect(SSHConfig{
		GatewayHost:     proxy.Hostname,
		GatewayPort:     proxy.Port,
		GatewayUser:     proxy.User,
		GatewayPassword: proxy.Password,
		TargetHost:      machine.Hostname,
		TargetPort:      22,
		TargetUser:      machine.User,
		TargetPassword:  proxy.Password,
		ConnectTimeout:  30 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("connect to %s: %w", machine.Name, err)
	}
	defer client.Close()

	if err := client.GitPull(project); err != nil {
		return fmt.Errorf("git pull before remote viz: %w", err)
	}

	present, err := checkDataOnRemote(client, viz.DataPath)
	if err != nil {
		return fmt.Errorf("check remote data: %w", err)
	}
	if !present {
		return fmt.Errorf("data not available on remote (%s)", viz.DataPath)
	}

	var axisParts []string
	for _, arg := range combo.Args {
		for _, part := range strings.Fields(arg) {
			axisParts = append(axisParts, shellEscape(part))
		}
	}

	runRemote := func(vizCmd, outputTpl string) (string, error) {
		resolvedCmd := buildVizCommand(vizCmd, combo, viz)
		cmdStr := fmt.Sprintf("cd %s && %s %s",
			shellEscape(project.RemotePath),
			resolvedCmd,
			strings.Join(axisParts, " "),
		)
		_, stderr, code, err := client.Run(cmdStr)
		if err != nil || code != 0 {
			return "", fmt.Errorf("remote viz failed (code %d)\n  cmd: %s\n  output: %s", code, cmdStr, stderr)
		}
		remoteOutputPath := filepath.Join(project.RemotePath, buildVizCommand(outputTpl, combo, viz))
		return remoteOutputPath, nil
	}

	fetchAndSave := func(remotePath, localPath string) error {
		data, err := client.ReadRemoteFileBinary(remotePath)
		if err != nil {
			return fmt.Errorf("read remote file %s: %w", remotePath, err)
		}
		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			return fmt.Errorf("mkdir: %w", err)
		}
		if err := os.WriteFile(localPath, data, 0644); err != nil {
			return fmt.Errorf("write %s: %w", localPath, err)
		}
		client.Run(fmt.Sprintf("rm -f %s", shellEscape(remotePath)))
		return nil
	}

	sel := map[string]string{}
	for i, ax := range viz.Axes {
		if i < len(combo.Args) {
			sel[ax.Name] = combo.Args[i]
		}
	}

	var svgVizCmd, pngVizCmd, svgOutputTpl, pngOutputTpl string
	if strings.Contains(viz.VizCommand, ".png") {
		pngVizCmd = viz.VizCommand
		svgVizCmd = strings.ReplaceAll(viz.VizCommand, ".png", ".svg")
		pngOutputTpl = viz.OutputFileTemplate
		svgOutputTpl = strings.ReplaceAll(viz.OutputFileTemplate, ".png", ".svg")
	} else {
		svgVizCmd = viz.VizCommand
		pngVizCmd = strings.ReplaceAll(viz.VizCommand, ".svg", ".png")
		svgOutputTpl = viz.OutputFileTemplate
		pngOutputTpl = strings.ReplaceAll(viz.OutputFileTemplate, ".svg", ".png")
	}

	svgRemotePath, err := runRemote(svgVizCmd, svgOutputTpl)
	if err != nil {
		return err
	}
	svgLocalPath := viz.ResolveOutputPath(localRepoDir, sel)
	if err := fetchAndSave(svgRemotePath, svgLocalPath); err != nil {
		return err
	}

	pngRemotePath, err := runRemote(pngVizCmd, pngOutputTpl)
	if err != nil {
		log.Printf("viz: remote PNG generation failed for %s combo %s: %v", viz.Name, combo.Key, err)
	} else {
		pngLocalPath := VizLocalPNGPath(svgLocalPath)
		if err := fetchAndSave(pngRemotePath, pngLocalPath); err != nil {
			log.Printf("viz: fetch remote PNG failed for %s combo %s: %v", viz.Name, combo.Key, err)
		}
	}

	log.Printf("viz: generateVizRemote: done %s combo=%s", viz.Name, combo.Key)
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

func checkDataOnRemote(client *Client, remotePath string) (bool, error) {
	if remotePath == "" {
		return false, nil
	}
	_, _, code, err := client.Run(fmt.Sprintf("test -e %s", shellEscape(remotePath)))
	if err != nil {
		return false, err
	}
	return code == 0, nil
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
