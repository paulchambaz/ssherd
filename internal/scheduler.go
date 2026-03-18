package internal

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Scheduler struct {
	cfgMu     sync.RWMutex
	cfg       SchedulerConfig
	cachePath string

	mu            sync.RWMutex
	jobs          []*Job
	machineStates map[string]*MachineState
	syncDirty     map[string]bool

	finalizingMu   sync.Mutex
	finalizingJobs map[string]bool

	generatingVizsMu sync.Mutex
	generatingVizs   map[string]bool

	syncMuMu sync.Mutex
	syncMu   map[string]*sync.Mutex

	wasRunning bool

	Events chan JobEvent
}

type MachineState struct {
	MachineID    string
	CurrentJobID string
	Launching    bool
}

type SchedulerConfig struct {
	UseRatio         float64
	DispatchInterval time.Duration
	MonitorInterval  time.Duration
	StallTimeout     time.Duration
	SyncInterval     time.Duration
	VizInterval      time.Duration
	LocalPrefix      string
	ProbeParallelism int
	DispatchStart    int
	DispatchEnd      int
	NtfyURL          string
	NtfyChannel      string
	NtfyUser         string
	NtfyPassword     string
}

func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		UseRatio:         0.5,
		DispatchInterval: 60 * time.Second,
		MonitorInterval:  2 * time.Minute,
		StallTimeout:     10 * time.Minute,
		SyncInterval:     30 * time.Minute,
		VizInterval:      10 * time.Minute,
		LocalPrefix:      "",
		ProbeParallelism: 8,
		DispatchStart:    20,
		DispatchEnd:      8,
	}
}

func NewScheduler(cfg SchedulerConfig, cachePath string) (*Scheduler, error) {
	if persisted, err := loadSchedulerConfig(cachePath); err == nil {
		cfg = persisted
	}

	store, err := LoadMachinesStore(cachePath)
	if err != nil {
		return nil, fmt.Errorf("load machines store: %w", err)
	}

	projects, err := LoadProjects(cachePath)
	if err != nil {
		return nil, fmt.Errorf("load projects: %w", err)
	}

	var allJobs []*Job
	for _, p := range projects {
		jobs, err := LoadJobs(cachePath, p.ID)
		if err != nil {
			log.Printf("scheduler: failed to load jobs for project %s: %v", p.ID, err)
			continue
		}
		for _, j := range jobs {
			if j.Status == JobPending || j.Status == JobRunning {
				allJobs = append(allJobs, j)
			}
		}
	}

	machineStates := make(map[string]*MachineState, len(store.Machines))
	for _, m := range store.Machines {
		machineStates[m.ID] = &MachineState{MachineID: m.ID}
	}

	log.Printf("scheduler: loaded %d jobs, %d machines", len(allJobs), len(store.Machines))

	return &Scheduler{
		cfg:            cfg,
		cachePath:      cachePath,
		jobs:           allJobs,
		machineStates:  machineStates,
		syncDirty:      make(map[string]bool),
		finalizingJobs: make(map[string]bool),
		generatingVizs: make(map[string]bool),
		Events:         make(chan JobEvent, 256),
	}, nil
}

// ─── Config ───────────────────────────────────────────────────────────────────

func (s *Scheduler) getConfig() SchedulerConfig {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Scheduler) UpdateConfig(cfg SchedulerConfig) error {
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
	return saveSchedulerConfig(s.cachePath, cfg)
}

func (s *Scheduler) GetConfig() SchedulerConfig {
	return s.getConfig()
}

// ─── Ntfy ─────────────────────────────────────────────────────────────────────

func (s *Scheduler) notify(title, msg string) {
	cfg := s.getConfig()
	if cfg.NtfyURL == "" || cfg.NtfyChannel == "" {
		return
	}

	endpoint := strings.TrimRight(cfg.NtfyURL, "/") + "/" + cfg.NtfyChannel
	req, err := http.NewRequest("POST", endpoint, strings.NewReader(msg))
	if err != nil {
		log.Printf("ntfy: build request: %v", err)
		return
	}
	req.Header.Set("Title", title)
	req.Header.Set("Content-Type", "text/plain")
	if cfg.NtfyUser != "" && cfg.NtfyPassword != "" {
		req.SetBasicAuth(cfg.NtfyUser, cfg.NtfyPassword)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("ntfy: send: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("ntfy: unexpected status %d", resp.StatusCode)
	}
}

// ─── Events ───────────────────────────────────────────────────────────────────

func (s *Scheduler) emit(kind EventKind, job *Job) {
	snapshot := *job
	if job.Progress != nil {
		p := *job.Progress
		snapshot.Progress = &p
	}
	select {
	case s.Events <- JobEvent{Kind: kind, Job: &snapshot}:
	default:
	}
}

func (s *Scheduler) emitViz(vizID, projectID, projectSlug, comboKey, vizErr string) {
	select {
	case s.Events <- JobEvent{
		Kind:        EventVizDone,
		VizID:       vizID,
		ProjectID:   projectID,
		ProjectSlug: projectSlug,
		ComboKey:    comboKey,
		VizErr:      vizErr,
	}:
	default:
	}
}

// ─── Viz generating guard ─────────────────────────────────────────────────────

// tryMarkVizGenerating returns true and marks the combo as generating if it was
// not already in progress. Returns false if the combo is already being generated.
func (s *Scheduler) tryMarkVizGenerating(vizID, comboKey string) bool {
	key := vizID + "/" + comboKey
	s.generatingVizsMu.Lock()
	defer s.generatingVizsMu.Unlock()
	if s.generatingVizs[key] {
		return false
	}
	s.generatingVizs[key] = true
	return true
}

func (s *Scheduler) clearVizGenerating(vizID, comboKey string) {
	s.generatingVizsMu.Lock()
	delete(s.generatingVizs, vizID+"/"+comboKey)
	s.generatingVizsMu.Unlock()
}

// IsVizGenerating is used by the WebSocket snapshot to report in-progress combos.
func (s *Scheduler) IsVizGenerating(vizID, comboKey string) bool {
	s.generatingVizsMu.Lock()
	defer s.generatingVizsMu.Unlock()
	return s.generatingVizs[vizID+"/"+comboKey]
}

// ─── Lifecycle ────────────────────────────────────────────────────────────────

func (s *Scheduler) Start() {
	go s.dispatchLoop()
	go s.monitorLoop()
	go s.syncLoop()
	go s.vizLoop()
	log.Printf("scheduler: started (use_ratio=%.0f%%, dispatch=%s, monitor=%s, sync=%s, viz=%s)",
		s.getConfig().UseRatio*100,
		s.getConfig().DispatchInterval,
		s.getConfig().MonitorInterval,
		s.getConfig().SyncInterval,
		s.getConfig().VizInterval,
	)
}

func (s *Scheduler) AddTask(job *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, job)
	log.Printf("scheduler: task queued %s (%s)", job.ID, job.DisplayName)
}

// ─── Dispatch ─────────────────────────────────────────────────────────────────

func (s *Scheduler) dispatchLoop() {
	for {
		time.Sleep(s.getConfig().DispatchInterval)
		s.dispatchTick()
	}
}

func (s *Scheduler) dispatchTick() {
	s.mu.Lock()
	log.Printf("scheduler: dispatch: LOCK acquired, loading store")
	defer s.mu.Unlock()

	store, err := LoadMachinesStore(s.cachePath)
	if err != nil {
		log.Printf("scheduler: dispatch: failed to load machines store: %v", err)
		return
	}

	usableMachines := 0
	for _, m := range store.Machines {
		if m.Status != MachineStatusDeprecated {
			usableMachines++
		}
	}
	log.Printf("scheduler: dispatch: store loaded (%d usable / %d total machines)", usableMachines, len(store.Machines))

	maxRunning := int(math.Floor(float64(usableMachines) * s.getConfig().UseRatio))
	if maxRunning == 0 {
		log.Printf("scheduler: dispatch: maxRunning=0, nothing to do")
		return
	}

	running := s.countRunning()
	if running >= maxRunning {
		log.Printf("scheduler: dispatch: running=%d >= maxRunning=%d, skip", running, maxRunning)
		return
	}

	if !s.isDispatchAllowed() {
		return
	}

	s.requeueFailedJobs()

	job := s.nextPending()
	if job == nil {
		return
	}

	machine := s.findAvailableMachine(store, job.GPURequirements)
	if machine == nil {
		log.Printf("scheduler: dispatch: no available machine for job %s", job.ID)
		return
	}

	project, err := LoadProject(s.cachePath, job.ProjectID)
	if err != nil {
		log.Printf("scheduler: dispatch: project %s not found for job %s: %v", job.ProjectID, job.ID, err)
		return
	}

	if _, ok := s.machineStates[machine.ID]; !ok {
		s.machineStates[machine.ID] = &MachineState{MachineID: machine.ID}
	}
	s.machineStates[machine.ID].CurrentJobID = job.ID
	s.machineStates[machine.ID].Launching = true

	job.Status = JobRunning
	now := time.Now()
	job.StartedAt = &now
	job.Machine = machine.Name

	if err := SaveJob(s.cachePath, job); err != nil {
		log.Printf("scheduler: dispatch: failed to save job %s: %v", job.ID, err)
	}
	s.emit(EventJobStatus, job)

	log.Printf("scheduler: dispatch: job=%s → machine=%s (running=%d/%d)", job.ID, machine.Name, running+1, maxRunning)
	go s.launchJob(job, machine, project, store)
}

func (s *Scheduler) countRunning() int {
	n := 0
	for _, j := range s.jobs {
		if j.Status == JobRunning {
			n++
		}
	}
	return n
}

func (s *Scheduler) nextPending() *Job {
	for _, j := range s.jobs {
		if j.Status == JobPending {
			return j
		}
	}
	return nil
}

func (s *Scheduler) requeueFailedJobs() {
	var requeued []*Job
	var rest []*Job

	for _, j := range s.jobs {
		if j.Status == JobFailed && j.RetryCount < j.MaxRetries {
			j.Status = JobPending
			j.RetryCount++
			j.RunCommand = j.RetryCommand
			j.Machine = ""
			j.StartedAt = nil
			j.FinishedAt = nil
			j.Progress = nil
			log.Printf("scheduler: dispatch: auto-requeue failed job=%s (retry %d/%d)", j.ID, j.RetryCount, j.MaxRetries)
			if err := SaveJob(s.cachePath, j); err != nil {
				log.Printf("scheduler: dispatch: save requeued job %s: %v", j.ID, err)
			}
			s.emit(EventJobStatus, j)
			requeued = append(requeued, j)
		} else {
			rest = append(rest, j)
		}
	}

	if len(requeued) > 0 {
		s.jobs = append(requeued, rest...)
	}
}

func (s *Scheduler) isDispatchAllowed() bool {
	cfg := s.getConfig()
	hour := time.Now().Hour()
	start := cfg.DispatchStart
	end := cfg.DispatchEnd

	if start == end {
		return true
	}

	var active bool
	if start > end {
		// plage traverse minuit : ex 20h → 8h
		active = hour >= start || hour < end
	} else {
		// plage dans la même journée : ex 8h → 20h
		active = hour >= start && hour < end
	}

	if !active {
		log.Printf("scheduler: dispatch: outside allowed window [%02d:00-%02d:00), skipping", start, end)
	}
	return active
}

func (s *Scheduler) findAvailableMachine(store *MachinesStore, req GPURequirements) *Machine {
	inUse := make(map[string]bool)
	for _, state := range s.machineStates {
		if state.CurrentJobID != "" {
			inUse[state.MachineID] = true
		}
	}

	machines := make([]*Machine, len(store.Machines))
	copy(machines, store.Machines)
	rand.Shuffle(len(machines), func(i, j int) {
		machines[i], machines[j] = machines[j], machines[i]
	})

	var preferred, fallback *Machine
	for _, m := range machines {
		if inUse[m.ID] || !m.SatisfiesRequirements(req) {
			continue
		}
		if req.PreferredGPU != "" && req.PreferredGPU != "any" && m.GPUModel == req.PreferredGPU {
			if preferred == nil {
				preferred = m
			}
		} else {
			if fallback == nil {
				fallback = m
			}
		}
	}
	if preferred != nil {
		return preferred
	}
	return fallback
}

// ─── Launch ───────────────────────────────────────────────────────────────────

func (s *Scheduler) launchJob(job *Job, machine *Machine, project *Project, store *MachinesStore) {
	log.Printf("scheduler: launchJob: start job=%s machine=%s", job.ID, machine.Name)

	clearLaunching := func() {
		s.mu.Lock()
		if state, ok := s.machineStates[machine.ID]; ok {
			state.Launching = false
		}
		s.mu.Unlock()
	}

	proxy := store.FindProxy(machine.ProxyID)
	if proxy == nil {
		log.Printf("scheduler: launchJob: proxy not found for machine %s", machine.Name)
		clearLaunching()
		s.revertJob(job, machine)
		return
	}

	cfg := SSHConfig{
		GatewayHost:     proxy.Hostname,
		GatewayPort:     proxy.Port,
		GatewayUser:     proxy.User,
		GatewayPassword: proxy.Password,
		TargetHost:      machine.Hostname,
		TargetPort:      22,
		TargetUser:      machine.User,
		TargetPassword:  proxy.Password,
		ConnectTimeout:  30 * time.Second,
	}

	log.Printf("scheduler: launchJob: connecting to %s", machine.Name)
	client, err := Connect(cfg)
	if err != nil {
		log.Printf("scheduler: launchJob: connect to %s failed: %v", machine.Name, err)
		store, _ := LoadMachinesStore(s.cachePath)
		s.markMachineUnreachable(store, machine)
		clearLaunching()
		s.revertJob(job, machine)
		return
	}
	defer client.Close()
	log.Printf("scheduler: launchJob: connected to %s", machine.Name)

	if _, _, code, err := client.Run("nvidia-smi -L"); err != nil || code != 0 {
		log.Printf("scheduler: launchJob: nvidia-smi failed on %s, marking deprecated", machine.Name)
		store, _ := LoadMachinesStore(s.cachePath)
		s.markMachineDeprecated(store, machine)
		clearLaunching()
		s.revertJob(job, machine)
		return
	}

	if _, _, code, err := client.Run("git --version"); err != nil || code != 0 {
		log.Printf("scheduler: launchJob: git not found on %s, marking deprecated", machine.Name)
		store, _ := LoadMachinesStore(s.cachePath)
		s.markMachineDeprecated(store, machine)
		clearLaunching()
		s.revertJob(job, machine)
		return
	}

	if job.GPURequirements.MinVRAMMB > 0 {
		freeRaw, _, code, err := client.Run(
			"nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits 2>/dev/null | head -1",
		)
		if err != nil || code != 0 {
			log.Printf("scheduler: launchJob: cannot query VRAM on %s, skipping", machine.Name)
			clearLaunching()
			s.revertJob(job, machine)
			return
		}
		freeMiB, err := strconv.Atoi(strings.TrimSpace(freeRaw))
		if err != nil {
			log.Printf("scheduler: launchJob: cannot parse VRAM %q on %s, skipping", strings.TrimSpace(freeRaw), machine.Name)
			clearLaunching()
			s.revertJob(job, machine)
			return
		}
		if freeMiB < job.GPURequirements.MinVRAMMB {
			log.Printf("scheduler: launchJob: not enough VRAM on %s (free=%dMiB required=%dMB), requeueing", machine.Name, freeMiB, job.GPURequirements.MinVRAMMB)
			clearLaunching()
			s.revertJob(job, machine)
			return
		}
	}

	if err := client.GitPull(project); err != nil {
		log.Printf("scheduler: launchJob: git pull failed for job %s on %s: %v", job.ID, machine.Name, err)
		clearLaunching()
		s.revertJob(job, machine)
		return
	}

	// Si des données locales existent pour ce job (checkpoints d'un retry
	// précédent), les copier vers le temporary de la nouvelle machine avant
	// de lancer le script.
	if machine.TemporaryPath != "" && project.DataPath != "" && job.OutputPath != "" {
		localOutputDir := filepath.Join(s.cachePath, job.ProjectID, "repo", project.DataPath, job.OutputPath)
		if info, err := os.Stat(localOutputDir); err == nil && info.IsDir() {
			remoteOutputDir := machine.TemporaryPath + "/" + project.DataPath + "/" + job.OutputPath
			log.Printf("scheduler: launchJob: copying local data to %s:%s", machine.Name, remoteOutputDir)
			if err := client.CopyLocalToRemote(localOutputDir, remoteOutputDir); err != nil {
				log.Printf("scheduler: launchJob: CopyLocalToRemote failed for job %s: %v — proceeding anyway", job.ID, err)
				// Non fatal : le job peut démarrer sans les checkpoints,
				// retry_command --resume échouera proprement s'ils manquent.
			}
		}
	}

	if err := client.RunBackground(LaunchParams{Job: job, Project: project, Machine: machine}); err != nil {
		log.Printf("scheduler: launchJob: launch failed for job %s on %s: %v", job.ID, machine.Name, err)
		clearLaunching()
		s.revertJob(job, machine)
		return
	}

	clearLaunching()
	log.Printf("scheduler: launchJob: done job=%s running on %s", job.ID, machine.Name)
}

func (s *Scheduler) markMachineDeprecated(store *MachinesStore, machine *Machine) {
	for _, m := range store.Machines {
		if m.ID == machine.ID {
			m.Status = MachineStatusDeprecated
			break
		}
	}
	if err := SaveMachinesStore(s.cachePath, store); err != nil {
		log.Printf("scheduler: failed to save deprecated machine %s: %v", machine.Name, err)
	}
}

func (s *Scheduler) markMachineUnreachable(store *MachinesStore, machine *Machine) {
	for _, m := range store.Machines {
		if m.ID == machine.ID {
			m.Status = MachineStatusUnreachable
			break
		}
	}
	if err := SaveMachinesStore(s.cachePath, store); err != nil {
		log.Printf("scheduler: failed to save unreachable machine %s: %v", machine.Name, err)
	}
}

func (s *Scheduler) revertJob(job *Job, machine *Machine) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if job.Status == JobCancelled {
		if state, ok := s.machineStates[machine.ID]; ok {
			state.CurrentJobID = ""
		}
		return
	}

	job.Status = JobPending
	job.Machine = ""
	job.StartedAt = nil

	if state, ok := s.machineStates[machine.ID]; ok {
		state.CurrentJobID = ""
	}

	if err := SaveJob(s.cachePath, job); err != nil {
		log.Printf("scheduler: failed to revert job %s: %v", job.ID, err)
	}
	s.emit(EventJobStatus, job)
}

// ─── Monitor ──────────────────────────────────────────────────────────────────

func (s *Scheduler) monitorLoop() {
	var watcher *Client
	var watcherMachineID string

	closeWatcher := func() {
		if watcher != nil {
			log.Printf("scheduler: monitor: closing watcher (machine=%s)", watcherMachineID)
			watcher.Close()
			watcher = nil
			watcherMachineID = ""
		}
	}
	defer closeWatcher()

	for {
		time.Sleep(s.getConfig().MonitorInterval)

		s.mu.RLock()
		var running []*Job
		for _, j := range s.jobs {
			if j.Status == JobRunning {
				running = append(running, j)
			}
		}
		s.mu.RUnlock()

		nowRunning := len(running) > 0
		if s.wasRunning && !nowRunning {
			go s.notify("ssherd", "All jobs finished")
			log.Printf("scheduler: monitor: all jobs finished — ntfy sent")
		}
		s.wasRunning = nowRunning

		if len(running) == 0 {
			log.Printf("scheduler: monitorTick: no running jobs, skip")
			closeWatcher()
			continue
		}

		log.Printf("scheduler: monitorTick: start — %d running jobs", len(running))
		start := time.Now()

		if watcher != nil && !watcher.IsAlive() {
			log.Printf("scheduler: monitor: watcher dead, reconnecting")
			closeWatcher()
		}
		if watcher == nil {
			w, machineID, err := s.openWatcher(running)
			if err != nil {
				log.Printf("scheduler: monitor: cannot open watcher: %v — skipping tick", err)
				continue
			}
			watcher = w
			watcherMachineID = machineID
			log.Printf("scheduler: monitor: watcher opened on machine=%s", watcherMachineID)
		}

		for _, job := range running {
			if err := s.checkJob(watcher, job); err != nil {
				log.Printf("scheduler: monitor: checkJob %s failed: %v — invalidating watcher", job.ID, err)
				closeWatcher()
				break
			}
		}

		log.Printf("scheduler: monitorTick: done in %s", time.Since(start).Round(time.Millisecond))
	}
}

func (s *Scheduler) openWatcher(running []*Job) (*Client, string, error) {
	store, err := LoadMachinesStore(s.cachePath)
	if err != nil {
		return nil, "", fmt.Errorf("load store: %w", err)
	}

	var candidate *Machine
	for _, j := range running {
		if j.Machine == "" {
			continue
		}
		for _, m := range store.Machines {
			if m.Name == j.Machine {
				candidate = m
				break
			}
		}
		if candidate != nil {
			break
		}
	}
	if candidate == nil {
		return nil, "", fmt.Errorf("no machine found among running jobs")
	}

	proxy := store.FindProxy(candidate.ProxyID)
	if proxy == nil {
		return nil, "", fmt.Errorf("proxy not found for machine %s", candidate.Name)
	}

	client, err := Connect(SSHConfig{
		GatewayHost:     proxy.Hostname,
		GatewayPort:     proxy.Port,
		GatewayUser:     proxy.User,
		GatewayPassword: proxy.Password,
		TargetHost:      candidate.Hostname,
		TargetPort:      22,
		TargetUser:      candidate.User,
		TargetPassword:  proxy.Password,
		ConnectTimeout:  30 * time.Second,
	})
	if err != nil {
		return nil, "", fmt.Errorf("connect to %s: %w", candidate.Name, err)
	}
	return client, candidate.ID, nil
}

func (s *Scheduler) checkJob(watcher *Client, job *Job) error {
	log.Printf("scheduler: checkJob: start job=%s (%s)", job.ID, job.DisplayName)

	s.finalizingMu.Lock()
	if s.finalizingJobs[job.ID] {
		s.finalizingMu.Unlock()
		log.Printf("scheduler: checkJob: job=%s already finalizing, skip", job.ID)
		return nil
	}
	s.finalizingMu.Unlock()

	statusOut, _, code, err := watcher.Run(fmt.Sprintf("cat %s/status 2>/dev/null", job.NfsJobDir))
	if err != nil {
		return fmt.Errorf("read status for job %s: %w", job.ID, err)
	}
	status := strings.TrimSpace(statusOut)
	log.Printf("scheduler: checkJob: job=%s status=%q (exit=%d)", job.ID, status, code)

	localJobDir := jobDir(s.cachePath, job.ProjectID, job.ID)

	switch status {
	case "done":
		s.markFinalizing(job.ID)
		s.syncProgress(watcher, job)
		s.syncOutputToLocal(watcher, job)
		log.Printf("scheduler: checkJob: job=%s done — finalizing", job.ID)
		s.finalizeJobInline(watcher, job, localJobDir, JobDone)
		return nil
	case "failed":
		s.markFinalizing(job.ID)
		s.syncProgress(watcher, job)
		s.syncOutputToLocal(watcher, job)
		log.Printf("scheduler: checkJob: job=%s failed — finalizing", job.ID)
		s.finalizeJobInline(watcher, job, localJobDir, JobFailed)
		return nil
	}

	hbOut, _, _, err := watcher.Run(fmt.Sprintf("cat %s/heartbeat 2>/dev/null", job.NfsJobDir))
	if err != nil {
		return fmt.Errorf("read heartbeat for job %s: %w", job.ID, err)
	}

	if hb := strings.TrimSpace(hbOut); hb != "" {
		hbTime, err := time.Parse(time.RFC3339, hb)
		if err != nil {
			log.Printf("scheduler: checkJob: job=%s heartbeat parse error: %v", job.ID, err)
		} else {
			age := time.Since(hbTime).Round(time.Second)
			if age > s.getConfig().StallTimeout {
				s.markFinalizing(job.ID)
				log.Printf("scheduler: checkJob: job=%s stalled", job.ID)
				s.finalizeJobInline(watcher, job, localJobDir, "")
				return nil
			}
		}
	}

	if err := watcher.SyncLogsToLocal(job.NfsJobDir, localJobDir); err != nil {
		log.Printf("scheduler: checkJob: sync logs failed for job %s: %v", job.ID, err)
	}

	s.syncProgress(watcher, job)

	snapshot := *job
	event := JobEvent{Kind: EventJobProgress, Job: &snapshot}
	if data, err := os.ReadFile(filepath.Join(localJobDir, "stdout.log")); err == nil {
		event.StdoutLog = string(data)
	}
	if data, err := os.ReadFile(filepath.Join(localJobDir, "stderr.log")); err == nil {
		event.StderrLog = string(data)
	}
	select {
	case s.Events <- event:
	default:
	}

	return nil
}

func (s *Scheduler) markFinalizing(jobID string) {
	s.finalizingMu.Lock()
	s.finalizingJobs[jobID] = true
	s.finalizingMu.Unlock()
}

func (s *Scheduler) clearFinalizing(jobID string) {
	s.finalizingMu.Lock()
	delete(s.finalizingJobs, jobID)
	s.finalizingMu.Unlock()
}

func (s *Scheduler) syncProgress(watcher *Client, job *Job) {
	if job.LogPath == "" {
		return
	}

	raw, err := watcher.ReadRemoteFile(job.LogPath)
	if err != nil || strings.TrimSpace(raw) == "" {
		return
	}

	var parsed struct {
		CurrentStep int    `json:"current_step"`
		TotalSteps  int    `json:"total_steps"`
		StartTime   string `json:"start_time"`
		CurrentTime string `json:"current_time"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &parsed); err != nil {
		return
	}

	startTime, err := time.Parse(time.RFC3339, parsed.StartTime)
	if err != nil {
		return
	}
	currentTime, err := time.Parse(time.RFC3339, parsed.CurrentTime)
	if err != nil {
		return
	}

	var percent, etaSeconds float64
	if parsed.TotalSteps > 0 {
		percent = float64(parsed.CurrentStep) / float64(parsed.TotalSteps) * 100
	}
	elapsed := currentTime.Sub(startTime).Seconds()
	if parsed.CurrentStep > 0 && elapsed > 0 {
		rate := float64(parsed.CurrentStep) / elapsed
		remaining := float64(parsed.TotalSteps - parsed.CurrentStep)
		etaSeconds = remaining / rate
	}

	progress := &JobProgress{
		CurrentStep: parsed.CurrentStep,
		TotalSteps:  parsed.TotalSteps,
		StartTime:   startTime,
		CurrentTime: currentTime,
		Percent:     percent,
		ETASeconds:  etaSeconds,
		LastUpdated: time.Now(),
	}

	localJobDir := jobDir(s.cachePath, job.ProjectID, job.ID)
	if data, err := json.MarshalIndent(progress, "", "  "); err == nil {
		_ = writeFileIfChanged(filepath.Join(localJobDir, "progress.json"), data)
	}

	s.mu.Lock()
	job.Progress = progress
	s.mu.Unlock()

	if err := SaveJob(s.cachePath, job); err != nil {
		log.Printf("scheduler: failed to save progress for job %s: %v", job.ID, err)
	}

	s.emit(EventJobProgress, job)
}

func writeFileIfChanged(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	existing, _ := os.ReadFile(path)
	if string(existing) == string(data) {
		return nil
	}
	return os.WriteFile(path, data, 0644)
}

func (s *Scheduler) Snapshot() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Job, len(s.jobs))
	copy(out, s.jobs)
	return out
}

func (s *Scheduler) CancelJob(jobID string) error {
	s.mu.Lock()
	var job *Job
	for _, j := range s.jobs {
		if j.ID == jobID {
			job = j
			break
		}
	}
	if job == nil {
		s.mu.Unlock()
		return fmt.Errorf("job %s not found", jobID)
	}

	wasRunning := job.Status == JobRunning
	machineName := job.Machine
	nfsJobDir := job.NfsJobDir

	job.Status = JobCancelled
	now := time.Now()
	job.FinishedAt = &now
	for _, state := range s.machineStates {
		if state.CurrentJobID == jobID {
			state.CurrentJobID = ""
			break
		}
	}
	s.mu.Unlock()

	if err := SaveJob(s.cachePath, job); err != nil {
		log.Printf("scheduler: cancel: save job %s failed: %v", jobID, err)
	}
	s.emit(EventJobStatus, job)

	if !wasRunning || machineName == "" {
		return nil
	}

	go func() {
		store, err := LoadMachinesStore(s.cachePath)
		if err != nil {
			log.Printf("scheduler: cancel: load store: %v", err)
			return
		}
		var machine *Machine
		for _, m := range store.Machines {
			if m.Name == machineName {
				machine = m
				break
			}
		}
		if machine == nil {
			return
		}
		proxy := store.FindProxy(machine.ProxyID)
		if proxy == nil {
			return
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
			ConnectTimeout:  15 * time.Second,
		})
		if err != nil {
			log.Printf("scheduler: cancel: connect to %s failed: %v", machineName, err)
			return
		}
		defer client.Close()

		if _, _, _, err := client.Run(fmt.Sprintf("echo cancelled > %s/status", nfsJobDir)); err != nil {
			log.Printf("scheduler: cancel: write status failed: %v", err)
		}
		pidRaw, err := client.ReadRemoteFile(nfsJobDir + "/pid")
		if err == nil && strings.TrimSpace(pidRaw) != "" {
			pid := strings.TrimSpace(pidRaw)
			if _, _, code, err := client.Run("kill " + pid + " 2>/dev/null"); err != nil || code != 0 {
				log.Printf("scheduler: cancel: kill pid=%s on %s failed", pid, machineName)
			} else {
				log.Printf("scheduler: cancel: killed pid=%s on %s", pid, machineName)
			}
		}
	}()

	return nil
}

func (s *Scheduler) DeleteJob(jobID, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var found *Job
	for _, j := range s.jobs {
		if j.ID == jobID {
			found = j
			break
		}
	}
	if found == nil {
		// Déjà absent de la mémoire, on tente quand même le disque.
		return internal_deleteJobDir(s.cachePath, projectID, jobID)
	}

	switch found.Status {
	case JobDone, JobFailed, JobStalled, JobCancelled:
		// ok
	default:
		return fmt.Errorf("job %s is not in a terminal state", jobID)
	}

	filtered := make([]*Job, 0, len(s.jobs)-1)
	for _, j := range s.jobs {
		if j.ID != jobID {
			filtered = append(filtered, j)
		}
	}
	s.jobs = filtered

	return internal_deleteJobDir(s.cachePath, projectID, jobID)
}

func internal_deleteJobDir(cachePath, projectID, jobID string) error {
	dir := jobDir(cachePath, projectID, jobID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete job dir: %w", err)
	}
	return nil
}

func (s *Scheduler) DeleteFinishedJobs(projectID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var deleted []string
	var remaining []*Job

	// Supprimer les jobs terminés présents en mémoire.
	for _, j := range s.jobs {
		if j.ProjectID != projectID {
			remaining = append(remaining, j)
			continue
		}
		switch j.Status {
		case JobDone, JobFailed, JobStalled, JobCancelled:
			if err := internal_deleteJobDir(s.cachePath, projectID, j.ID); err != nil {
				log.Printf("scheduler: DeleteFinishedJobs: %v", err)
				remaining = append(remaining, j)
			} else {
				deleted = append(deleted, j.ID)
			}
		default:
			remaining = append(remaining, j)
		}
	}
	s.jobs = remaining

	// Supprimer les jobs terminés présents sur disque mais pas en mémoire
	// (done/failed/stalled/cancelled ne sont pas chargés au démarrage).
	seen := make(map[string]bool, len(deleted)+len(s.jobs))
	for _, id := range deleted {
		seen[id] = true
	}
	for _, j := range s.jobs {
		seen[j.ID] = true
	}

	allJobs, err := LoadJobs(s.cachePath, projectID)
	if err != nil {
		return deleted, nil
	}
	for _, j := range allJobs {
		if seen[j.ID] {
			continue
		}
		switch j.Status {
		case JobDone, JobFailed, JobStalled, JobCancelled:
			if err := internal_deleteJobDir(s.cachePath, projectID, j.ID); err != nil {
				log.Printf("scheduler: DeleteFinishedJobs: disk: %v", err)
			} else {
				deleted = append(deleted, j.ID)
			}
		}
	}

	return deleted, nil
}

func (s *Scheduler) RequeueJob(job *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == job.ID {
			*j = *job
			return
		}
	}
	s.jobs = append(s.jobs, job)
}

// ─── Sync ─────────────────────────────────────────────────────────────────────

func (s *Scheduler) syncLoop() {
	for {
		time.Sleep(s.getConfig().SyncInterval)
		s.syncTick()
	}
}

func (s *Scheduler) syncTick() {
	// Collecter les jobs running et les grouper par machine.
	s.mu.RLock()
	type machineJob struct {
		job     *Job
		project *Project
	}
	byMachine := make(map[string][]*Job)
	for _, j := range s.jobs {
		if j.Status == JobRunning && j.Machine != "" {
			byMachine[j.Machine] = append(byMachine[j.Machine], j)
		}
	}
	s.mu.RUnlock()

	if len(byMachine) == 0 {
		log.Printf("scheduler: syncTick: no running jobs, skip")
		return
	}

	store, err := LoadMachinesStore(s.cachePath)
	if err != nil {
		log.Printf("scheduler: syncTick: load store: %v", err)
		return
	}

	for machineName, jobs := range byMachine {
		var machine *Machine
		for _, m := range store.Machines {
			if m.Name == machineName {
				machine = m
				break
			}
		}
		if machine == nil || machine.TemporaryPath == "" {
			log.Printf("scheduler: syncTick: machine %s has no TemporaryPath, skip", machineName)
			continue
		}

		proxy := store.FindProxy(machine.ProxyID)
		if proxy == nil {
			log.Printf("scheduler: syncTick: proxy not found for machine %s, skip", machineName)
			continue
		}

		if err := s.syncMachine(machine, proxy, jobs); err != nil {
			log.Printf("scheduler: syncTick: machine %s failed: %v", machineName, err)
		}
	}
}

func (s *Scheduler) projectSyncMu(projectID string) *sync.Mutex {
	s.syncMuMu.Lock()
	defer s.syncMuMu.Unlock()
	if s.syncMu == nil {
		s.syncMu = make(map[string]*sync.Mutex)
	}
	if s.syncMu[projectID] == nil {
		s.syncMu[projectID] = &sync.Mutex{}
	}
	return s.syncMu[projectID]
}

func (s *Scheduler) syncMachine(machine *Machine, proxy *Proxy, jobs []*Job) error {
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
		return fmt.Errorf("connect: %w", err)
	}
	defer client.Close()

	// Grouper les jobs par projet pour éviter de syncer le même DataPath deux fois.
	type projectSync struct {
		dataPath  string
		projectID string
	}
	seen := make(map[string]bool)
	for _, job := range jobs {
		project, err := LoadProject(s.cachePath, job.ProjectID)
		if err != nil || project.DataPath == "" {
			continue
		}
		key := project.ID + "/" + project.DataPath
		if seen[key] {
			continue
		}
		seen[key] = true

		remoteDir := machine.TemporaryPath + "/" + project.DataPath + "/"
		localDir := filepath.Join(s.cachePath, project.ID, "repo", project.DataPath)

		log.Printf("scheduler: syncMachine: %s — rsync %s → %s", machine.Name, remoteDir, localDir)
		if err := client.SyncDirToLocal(remoteDir, localDir); err != nil {
			log.Printf("scheduler: syncMachine: sync %s failed: %v", remoteDir, err)
		}
	}

	return nil
}

func (s *Scheduler) syncRepo(project *Project) error {
	mu := s.projectSyncMu(project.ID)
	if !mu.TryLock() {
		return nil
	}
	defer mu.Unlock()

	localRepoDir := filepath.Join(s.cachePath, project.ID, "repo")
	if err := os.MkdirAll(localRepoDir, 0755); err != nil {
		return fmt.Errorf("mkdir repo: %w", err)
	}

	if err := localGitCloneOrPull(project, localRepoDir); err != nil {
		return fmt.Errorf("local git: %w", err)
	}

	jobs, err := LoadJobs(s.cachePath, project.ID)
	if err != nil {
		return fmt.Errorf("load jobs: %w", err)
	}
	outputDirs := make(map[string]bool)
	for _, j := range jobs {
		if j.OutputPath != "" {
			outputDirs[j.OutputPath] = true
		}
	}
	if len(outputDirs) == 0 {
		return nil
	}

	machine, proxy, err := s.findMachineForRsync()
	if err != nil {
		return fmt.Errorf("no machine for sync: %w", err)
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
		return fmt.Errorf("connect for sync: %w", err)
	}
	defer client.Close()

	for outputDir := range outputDirs {
		if !strings.HasPrefix(outputDir, project.RemotePath) {
			continue
		}
		rel, _ := filepath.Rel(project.RemotePath, outputDir)
		localDir := filepath.Join(localRepoDir, rel)
		if err := client.SyncDirToLocal(outputDir, localDir); err != nil {
			log.Printf("scheduler: syncRepo: sync failed %s: %v", outputDir, err)
		}
	}

	return nil
}

func (s *Scheduler) SyncRepoNow(projectID string) error {
	go func() {
		s.mu.RLock()
		var runningJobs []*Job
		for _, j := range s.jobs {
			if j.ProjectID == projectID && j.Status == JobRunning && j.Machine != "" {
				runningJobs = append(runningJobs, j)
			}
		}
		s.mu.RUnlock()

		if len(runningJobs) == 0 {
			log.Printf("scheduler: SyncRepoNow: no running jobs for project %s, nothing to sync", projectID)
			return
		}

		store, err := LoadMachinesStore(s.cachePath)
		if err != nil {
			log.Printf("scheduler: SyncRepoNow: load store: %v", err)
			return
		}

		byMachine := make(map[string][]*Job)
		for _, j := range runningJobs {
			byMachine[j.Machine] = append(byMachine[j.Machine], j)
		}

		for machineName, jobs := range byMachine {
			var machine *Machine
			for _, m := range store.Machines {
				if m.Name == machineName {
					machine = m
					break
				}
			}
			if machine == nil || machine.TemporaryPath == "" {
				continue
			}
			proxy := store.FindProxy(machine.ProxyID)
			if proxy == nil {
				continue
			}
			if err := s.syncMachine(machine, proxy, jobs); err != nil {
				log.Printf("scheduler: SyncRepoNow: machine %s: %v", machineName, err)
			}
		}
	}()
	return nil
}

// ─── Finalization ─────────────────────────────────────────────────────────────
func (s *Scheduler) syncOutputToLocal(watcher *Client, job *Job) {
	if job.OutputPath == "" {
		return
	}

	store, err := LoadMachinesStore(s.cachePath)
	if err != nil {
		log.Printf("scheduler: syncOutputToLocal: load store: %v", err)
		return
	}
	var machine *Machine
	for _, m := range store.Machines {
		if m.Name == job.Machine {
			machine = m
			break
		}
	}
	if machine == nil || machine.TemporaryPath == "" {
		return
	}

	project, err := LoadProject(s.cachePath, job.ProjectID)
	if err != nil || project.DataPath == "" {
		return
	}

	remoteOutputDir := machine.TemporaryPath + "/" + project.DataPath + "/" + job.OutputPath
	localOutputDir := filepath.Join(s.cachePath, job.ProjectID, "repo", project.DataPath, job.OutputPath)

	log.Printf("scheduler: syncOutputToLocal: job=%s rsync %s → %s", job.ID, remoteOutputDir, localOutputDir)
	if err := watcher.SyncDirToLocal(remoteOutputDir, localOutputDir); err != nil {
		log.Printf("scheduler: syncOutputToLocal: sync failed: %v", err)
	}

	if _, _, code, err := watcher.Run("rm -rf " + remoteOutputDir); err != nil || code != 0 {
		log.Printf("scheduler: syncOutputToLocal: rm -rf %s failed (code=%d): %v", remoteOutputDir, code, err)
	} else {
		log.Printf("scheduler: syncOutputToLocal: cleaned up remote %s", remoteOutputDir)
	}
}

func (s *Scheduler) finalizeJobInline(watcher *Client, job *Job, localJobDir string, status JobStatus) {
	defer s.clearFinalizing(job.ID)

	if err := watcher.FinalizeLogsToLocal(job.NfsJobDir, localJobDir); err != nil {
		log.Printf("scheduler: finalize: logs copy failed for job %s: %v", job.ID, err)
	}

	if status == JobDone && len(job.OutputFiles) > 0 {
		if err := watcher.DeleteOutputFiles(job.OutputFiles); err != nil {
			log.Printf("scheduler: finalize: cleanup output files for job %s: %v", job.ID, err)
		}
	}

	s.mu.Lock()

	if job.Status == JobCancelled {
		for _, state := range s.machineStates {
			if state.CurrentJobID == job.ID {
				state.CurrentJobID = ""
				break
			}
		}
		s.mu.Unlock()
		return
	}

	for _, state := range s.machineStates {
		if state.CurrentJobID == job.ID {
			state.CurrentJobID = ""
			break
		}
	}

	now := time.Now()

	if status == "" {
		if job.RetryCount < job.MaxRetries {
			job.Status = JobPending
			job.RetryCount++
			job.RunCommand = job.RetryCommand
			job.Machine = ""
			job.StartedAt = nil
			job.FinishedAt = nil
			job.Progress = nil
			log.Printf("scheduler: finalize: job=%s stalled — requeuing (retry %d/%d)", job.ID, job.RetryCount, job.MaxRetries)
		} else {
			job.Status = JobStalled
			job.FinishedAt = &now
			log.Printf("scheduler: finalize: job=%s stalled — max retries reached", job.ID)
		}
	} else {
		job.Status = status
		job.FinishedAt = &now
		if status == JobDone && job.Progress != nil {
			job.Progress.Percent = 100
			job.Progress.CurrentStep = job.Progress.TotalSteps
			job.Progress.ETASeconds = 0
			job.Progress.LastUpdated = now
		}
	}

	if err := SaveJob(s.cachePath, job); err != nil {
		log.Printf("scheduler: finalize: save job %s failed: %v", job.ID, err)
	}
	s.emit(EventJobStatus, job)

	s.mu.Unlock()

	if status == JobFailed {
		go s.notify("ssherd — job failed", job.DisplayName+" failed")
	}
}

func (s *Scheduler) findMachineForRsync() (*Machine, *Proxy, error) {
	store, err := LoadMachinesStore(s.cachePath)
	if err != nil {
		return nil, nil, fmt.Errorf("load store: %w", err)
	}

	s.mu.RLock()
	var candidateID string
	for id, state := range s.machineStates {
		if state.CurrentJobID != "" && !state.Launching {
			candidateID = id
			break
		}
	}
	s.mu.RUnlock()

	if candidateID != "" {
		if m := store.FindMachine(candidateID); m != nil {
			if p := store.FindProxy(m.ProxyID); p != nil {
				return m, p, nil
			}
		}
	}

	s.mu.RLock()
	launching := make(map[string]bool)
	for id, state := range s.machineStates {
		if state.Launching {
			launching[id] = true
		}
	}
	s.mu.RUnlock()

	for _, m := range store.Machines {
		if m.Status == MachineStatusDeprecated || launching[m.ID] {
			continue
		}
		p := store.FindProxy(m.ProxyID)
		if p == nil {
			continue
		}
		return m, p, nil
	}

	return nil, nil, fmt.Errorf("no available machine")
}

// ─── Persistence config ───────────────────────────────────────────────────────

func schedulerConfigFile(cachePath string) string {
	return filepath.Join(cachePath, "scheduler.json")
}

func saveSchedulerConfig(cachePath string, cfg SchedulerConfig) error {
	type persistedConfig struct {
		UseRatio         float64 `json:"use_ratio"`
		DispatchInterval string  `json:"dispatch_interval"`
		MonitorInterval  string  `json:"monitor_interval"`
		StallTimeout     string  `json:"stall_timeout"`
		SyncInterval     string  `json:"sync_interval"`
		VizInterval      string  `json:"viz_interval"`
		LocalPrefix      string  `json:"local_prefix"`
		DispatchStart    int     `json:"dispatch_start"`
		DispatchEnd      int     `json:"dispatch_end"`
		NtfyURL          string  `json:"ntfy_url"`
		NtfyChannel      string  `json:"ntfy_channel"`
		NtfyUser         string  `json:"ntfy_user"`
		NtfyPassword     string  `json:"ntfy_password"`
	}
	data, err := json.MarshalIndent(persistedConfig{
		UseRatio:         cfg.UseRatio,
		DispatchInterval: cfg.DispatchInterval.String(),
		MonitorInterval:  cfg.MonitorInterval.String(),
		StallTimeout:     cfg.StallTimeout.String(),
		SyncInterval:     cfg.SyncInterval.String(),
		VizInterval:      cfg.VizInterval.String(),
		LocalPrefix:      cfg.LocalPrefix,
		DispatchStart:    cfg.DispatchStart,
		DispatchEnd:      cfg.DispatchEnd,
		NtfyURL:          cfg.NtfyURL,
		NtfyChannel:      cfg.NtfyChannel,
		NtfyUser:         cfg.NtfyUser,
		NtfyPassword:     cfg.NtfyPassword,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(schedulerConfigFile(cachePath), data, 0644)
}

func loadSchedulerConfig(cachePath string) (SchedulerConfig, error) {
	cfg := DefaultSchedulerConfig()
	data, err := os.ReadFile(schedulerConfigFile(cachePath))
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	var raw struct {
		UseRatio         float64 `json:"use_ratio"`
		DispatchInterval string  `json:"dispatch_interval"`
		MonitorInterval  string  `json:"monitor_interval"`
		StallTimeout     string  `json:"stall_timeout"`
		SyncInterval     string  `json:"sync_interval"`
		VizInterval      string  `json:"viz_interval"`
		LocalPrefix      string  `json:"local_prefix"`
		DispatchStart    int     `json:"dispatch_start"`
		DispatchEnd      int     `json:"dispatch_end"`
		NtfyURL          string  `json:"ntfy_url"`
		NtfyChannel      string  `json:"ntfy_channel"`
		NtfyUser         string  `json:"ntfy_user"`
		NtfyPassword     string  `json:"ntfy_password"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return cfg, err
	}
	if raw.UseRatio > 0 {
		cfg.UseRatio = raw.UseRatio
	}
	if d, err := time.ParseDuration(raw.DispatchInterval); err == nil {
		cfg.DispatchInterval = d
	}
	if d, err := time.ParseDuration(raw.MonitorInterval); err == nil {
		cfg.MonitorInterval = d
	}
	if d, err := time.ParseDuration(raw.StallTimeout); err == nil {
		cfg.StallTimeout = d
	}
	if d, err := time.ParseDuration(raw.SyncInterval); err == nil {
		cfg.SyncInterval = d
	}
	if d, err := time.ParseDuration(raw.VizInterval); err == nil {
		cfg.VizInterval = d
	}

	cfg.LocalPrefix = raw.LocalPrefix
	if raw.DispatchStart >= 0 && raw.DispatchStart <= 23 {
		cfg.DispatchStart = raw.DispatchStart
	}
	if raw.DispatchEnd >= 0 && raw.DispatchEnd <= 23 {
		cfg.DispatchEnd = raw.DispatchEnd
	}
	cfg.NtfyURL = raw.NtfyURL
	cfg.NtfyChannel = raw.NtfyChannel
	cfg.NtfyUser = raw.NtfyUser
	cfg.NtfyPassword = raw.NtfyPassword
	return cfg, nil
}
