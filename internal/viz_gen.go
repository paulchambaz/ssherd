package internal

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// vizLoop tourne toutes les VizInterval et régénère les visualisations périmées.
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

	// Pas de jobs connus pour ce projet — rien à visualiser
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

func (s *Scheduler) vizTickOne(project *Project, viz *Visualization, jobs []*Job, localRepoDir string) {
	jobsWriting := runningJobsWriteToVizData(jobs, viz, project)
	localDataPath := ResolveToLocal(viz.DataPath, project.RemotePath, localRepoDir)

	for _, combo := range viz.AllCombos() {
		outputPath := VizLocalOutputPath(localRepoDir, viz.OutputFileTemplate, combo.Key)

		if !vizNeedsRegen(outputPath, localDataPath) {
			continue
		}

		var err error
		if jobsWriting {
			// Mode remote obligatoire : les données sont en cours d'écriture
			// on ne touche pas au local
			err = s.generateVizRemote(project, viz, combo, localRepoDir)
		} else {
			// Mode local par défaut quand le job est terminé
			err = s.generateVizLocal(project, viz, combo, localRepoDir)
		}
		if err != nil {
			log.Printf("viz: tick %s/%s combo %s: %v", project.Slug, viz.Name, combo.Key, err)
		}
	}
}

// GenerateVizNow est appelé par le handler HTTP pour une régénération manuelle.
// mode : "auto", "local", "remote".
// En mode "local" avec des jobs en écriture, on procède quand même (choix explicite
// de l'utilisateur) mais on log un avertissement.
// Retourne le nombre de combos générés avec succès et l'éventuelle dernière erreur.
func (s *Scheduler) GenerateVizNow(project *Project, viz *Visualization, mode string) (int, error) {
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

	var lastErr error
	ok := 0
	for _, combo := range viz.AllCombos() {
		var err error
		if useRemote {
			err = s.generateVizRemote(project, viz, combo, localRepoDir)
		} else {
			err = s.generateVizLocal(project, viz, combo, localRepoDir)
		}
		if err != nil {
			log.Printf("viz: GenerateVizNow %s combo %s: %v", viz.Name, combo.Key, err)
			lastErr = err
		} else {
			ok++
		}
	}
	return ok, lastErr
}

// VizJobsWriting indique si des jobs running écrivent sur les données de cette viz.
// Exposé pour que le handler HTTP puisse informer l'UI.
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
		// non fatal — le SVG est déjà là
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

	// SVG — fatal si échec
	svgRemotePath, err := runRemote(svgVizCmd, svgOutputTpl)
	if err != nil {
		return err
	}
	svgLocalPath := viz.ResolveOutputPath(localRepoDir, sel)
	if err := fetchAndSave(svgRemotePath, svgLocalPath); err != nil {
		return err
	}

	// PNG — non fatal
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

// runningJobsWriteToVizData retourne true si un job running du projet écrit
// sur des fichiers qui se chevauchent avec le data_path de la viz.
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

// pathsOverlap retourne true si l'un des chemins est un préfixe de l'autre.
func pathsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a = strings.TrimSuffix(a, "/")
	b = strings.TrimSuffix(b, "/")
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// vizNeedsRegen retourne true si le fichier de sortie n'existe pas ou si un
// fichier dans dataDir est plus récent que le fichier de sortie.
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

// checkDataOnRemote vérifie l'existence d'un chemin sur le remote via SSH.
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

// buildVizCommand substitue les placeholders dans la commande viz pour un combo donné.
// {version}        → slug de toutes les valeurs du combo (ex: "sine_0")
// {<axis_name>}    → valeur de l'axe nommé pour ce combo
func buildVizCommand(vizCommand string, combo VizCombo, viz *Visualization) string {
	versionParts := make([]string, 0, len(viz.Axes))
	for i := range viz.Axes {
		val := comboValueForAxis(combo, viz, i)
		versionParts = append(versionParts, Slugify(lastArgToken(val)))
	}
	version := strings.Join(versionParts, "_")
	if version == "" {
		version = "default"
	}

	vars := map[string]string{
		"version": version,
	}
	for i, ax := range viz.Axes {
		vars[ax.Name] = lastArgToken(comboValueForAxis(combo, viz, i))
	}

	cmd := vizCommand
	for k, v := range vars {
		cmd = strings.ReplaceAll(cmd, "{"+k+"}", v)
	}
	return cmd
}

// comboValueForAxis retourne la valeur brute (ex: "--env sine") pour l'axe toggleable à l'index i.
func comboValueForAxis(combo VizCombo, viz *Visualization, axisIdx int) string {
	if axisIdx >= len(combo.Args) {
		return ""
	}
	return combo.Args[axisIdx]
}

// lastArgToken extrait le dernier token d'une valeur CLI (ex: "--env sine" → "sine").
func lastArgToken(s string) string {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return s
	}
	return parts[len(parts)-1]
}
