package internal

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
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

	syncMuMu sync.Mutex
	syncMu   map[string]*sync.Mutex

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
		cfg:           cfg,
		cachePath:     cachePath,
		jobs:          allJobs,
		machineStates: machineStates,
		syncDirty:     make(map[string]bool),
		Events:        make(chan JobEvent, 256),
	}, nil
}

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

// --- Dispatch ---

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
	log.Printf("scheduler: dispatch: store loaded")
	if err != nil {
		log.Printf("scheduler: dispatch: failed to load machines store: %v", err)
		return
	}

	maxRunning := int(math.Floor(float64(len(store.Machines)) * s.getConfig().UseRatio))
	if maxRunning == 0 {
		log.Printf("scheduler: dispatch: maxRunning=0, nothing to do")
		return
	}

	running := s.countRunning()
	if running >= maxRunning {
		log.Printf("scheduler: dispatch: running=%d >= maxRunning=%d, skip", running, maxRunning)
		return
	}

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

func (s *Scheduler) findAvailableMachine(store *MachinesStore, req GPURequirements) *Machine {
	inUse := make(map[string]bool)
	for _, state := range s.machineStates {
		if state.CurrentJobID != "" {
			inUse[state.MachineID] = true
		}
	}

	var preferred, fallback *Machine
	for _, m := range store.Machines {
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

// --- Lancement ---

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
	log.Printf("scheduler: launchJob: nvidia-smi ok on %s", machine.Name)

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
		log.Printf("scheduler: launchJob: VRAM ok on %s (%dMiB free)", machine.Name, freeMiB)
	}

	if err := client.GitPull(project); err != nil {
		log.Printf("scheduler: launchJob: git pull failed for job %s on %s: %v", job.ID, machine.Name, err)
		clearLaunching()
		s.revertJob(job, machine)
		return
	}
	log.Printf("scheduler: launchJob: git pull done for job=%s on %s", job.ID, machine.Name)

	if err := client.RunBackground(LaunchParams{Job: job, Project: project}); err != nil {
		log.Printf("scheduler: launchJob: launch failed for job %s on %s: %v", job.ID, machine.Name, err)
		clearLaunching()
		s.revertJob(job, machine)
		return
	}

	// Launch terminé — rsync peut maintenant utiliser cette machine.
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

func (s *Scheduler) revertJob(job *Job, machine *Machine) {
	s.mu.Lock()
	defer s.mu.Unlock()

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

// --- Monitor ---
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

		log.Printf("scheduler: monitor: about to RLock")
		s.mu.RLock()
		log.Printf("scheduler: monitor: RLock acquired")
		var running []*Job
		for _, j := range s.jobs {
			if j.Status == JobRunning {
				running = append(running, j)
			}
		}
		s.mu.RUnlock()

		if len(running) == 0 {
			log.Printf("scheduler: monitorTick: no running jobs, skip")
			closeWatcher()
			continue
		}

		log.Printf("scheduler: monitorTick: start — %d running jobs", len(running))
		start := time.Now()

		// Ouvre ou vérifie le watcher — local à cette goroutine.
		if watcher != nil {
			if !watcher.IsAlive() {
				log.Printf("scheduler: monitor: watcher dead, reconnecting")
				closeWatcher()
			}
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

	// Chercher une machine qui a un job running.
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

	log.Printf("scheduler: monitor: connecting watcher to %s", candidate.Name)
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

// checkJob vérifie l'état d'un job via le watcher et le finalise inline si
// nécessaire. Retourne une erreur si le watcher semble mort.
func (s *Scheduler) checkJob(watcher *Client, job *Job) error {
	log.Printf("scheduler: checkJob: start job=%s (%s)", job.ID, job.DisplayName)

	statusOut, _, code, err := watcher.Run(fmt.Sprintf("cat %s/status 2>/dev/null", job.NfsJobDir))
	if err != nil {
		return fmt.Errorf("read status for job %s: %w", job.ID, err)
	}
	status := strings.TrimSpace(statusOut)
	log.Printf("scheduler: checkJob: job=%s status=%q (exit=%d)", job.ID, status, code)

	localJobDir := jobDir(s.cachePath, job.ProjectID, job.ID)

	switch status {
	case "done":
		log.Printf("scheduler: checkJob: job=%s done — finalizing", job.ID)
		s.finalizeJobInline(watcher, job, localJobDir, JobDone)
		return nil
	case "failed":
		log.Printf("scheduler: checkJob: job=%s failed — finalizing", job.ID)
		s.finalizeJobInline(watcher, job, localJobDir, JobFailed)
		return nil
	}

	// Job toujours en cours — vérifier le heartbeat.
	hbOut, _, _, err := watcher.Run(fmt.Sprintf("cat %s/heartbeat 2>/dev/null", job.NfsJobDir))
	if err != nil {
		return fmt.Errorf("read heartbeat for job %s: %w", job.ID, err)
	}

	if hb := strings.TrimSpace(hbOut); hb != "" {
		hbTime, err := time.Parse(time.RFC3339, hb)
		if err != nil {
			log.Printf("scheduler: checkJob: job=%s heartbeat parse error: %v (raw=%q)", job.ID, err, hb)
		} else {
			age := time.Since(hbTime).Round(time.Second)
			log.Printf("scheduler: checkJob: job=%s heartbeat age=%s (stall_timeout=%s)", job.ID, age, s.getConfig().StallTimeout)
			if age > s.getConfig().StallTimeout {
				log.Printf("scheduler: checkJob: job=%s stalled", job.ID)
				s.finalizeJobInline(watcher, job, localJobDir, "")
				return nil
			}
		}
	} else {
		log.Printf("scheduler: checkJob: job=%s no heartbeat yet", job.ID)
	}

	// Sync des logs.
	if err := watcher.SyncLogsToLocal(job.NfsJobDir, localJobDir); err != nil {
		log.Printf("scheduler: checkJob: sync logs failed for job %s: %v", job.ID, err)
	}

	// Progression.
	s.syncProgress(watcher, job)

	// Émettre l'événement avec logs.
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

	log.Printf("scheduler: checkJob: done job=%s", job.ID)
	return nil
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

func (s *Scheduler) handleStall(job *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, state := range s.machineStates {
		if state.CurrentJobID == job.ID {
			state.CurrentJobID = ""
			break
		}
	}

	if job.RetryCount < job.MaxRetries {
		job.Status = JobPending
		job.RetryCount++
		job.RunCommand = job.RetryCommand
		job.Machine = ""
		job.StartedAt = nil
		job.FinishedAt = nil
		job.Progress = nil
		log.Printf("scheduler: requeuing stalled job %s (retry %d/%d)", job.ID, job.RetryCount, job.MaxRetries)
	} else {
		job.Status = JobStalled
		now := time.Now()
		job.FinishedAt = &now
		log.Printf("scheduler: job %s stalled — max retries reached", job.ID)
	}

	if err := SaveJob(s.cachePath, job); err != nil {
		log.Printf("scheduler: failed to save stalled job %s: %v", job.ID, err)
	}
	s.emit(EventJobStatus, job)
}

// --- Watcher ---

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

// CancelJob est inchangé dans sa logique mais n'interagit plus avec le watcher.
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

	// Ouvrir une connexion dédiée pour tuer le process — pas de watcher.
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
			log.Printf("scheduler: cancel: machine %s not found", machineName)
			return
		}
		proxy := store.FindProxy(machine.ProxyID)
		if proxy == nil {
			log.Printf("scheduler: cancel: proxy not found for %s", machineName)
			return
		}
		log.Printf("scheduler: cancel: connecting to %s to kill job %s", machineName, jobID)
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
				log.Printf("scheduler: cancel: kill pid=%s on %s failed (may already be dead)", pid, machineName)
			} else {
				log.Printf("scheduler: cancel: killed pid=%s on %s", pid, machineName)
			}
		}
	}()

	return nil
}

func (s *Scheduler) RequeueJob(job *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == job.ID {
			*j = *job
			log.Printf("scheduler: requeue: updated existing job %s in queue", job.ID)
			return
		}
	}
	s.jobs = append(s.jobs, job)
	log.Printf("scheduler: requeue: added job %s to queue", job.ID)
}

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
	}
	data, err := json.MarshalIndent(persistedConfig{
		UseRatio:         cfg.UseRatio,
		DispatchInterval: cfg.DispatchInterval.String(),
		MonitorInterval:  cfg.MonitorInterval.String(),
		StallTimeout:     cfg.StallTimeout.String(),
		SyncInterval:     cfg.SyncInterval.String(),
		VizInterval:      cfg.VizInterval.String(),
		LocalPrefix:      cfg.LocalPrefix,
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
	if raw.LocalPrefix != "" {
		cfg.LocalPrefix = raw.LocalPrefix
	}
	return cfg, nil
}

// --- Sync ---
func (s *Scheduler) syncLoop() {
	for {
		time.Sleep(s.getConfig().SyncInterval)
		s.syncTick()
	}
}

func (s *Scheduler) syncTick() {
	s.mu.Lock()
	dirty := make(map[string]bool)
	for k, v := range s.syncDirty {
		dirty[k] = v
	}
	for k := range dirty {
		delete(s.syncDirty, k)
	}
	s.mu.Unlock()

	if len(dirty) == 0 {
		return
	}

	log.Printf("scheduler: syncTick: %d project(s) to sync", len(dirty))

	for projectID := range dirty {
		project, err := LoadProject(s.cachePath, projectID)
		if err != nil {
			log.Printf("scheduler: sync: load project %s: %v", projectID, err)
			continue
		}
		if err := s.syncRepo(project); err != nil {
			log.Printf("scheduler: sync: project %s failed: %v — will retry next tick", project.Slug, err)
			// Remettre dans dirty pour retry au prochain tick.
			s.mu.Lock()
			s.syncDirty[projectID] = true
			s.mu.Unlock()
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

func (s *Scheduler) syncRepo(project *Project) error {
	mu := s.projectSyncMu(project.ID)
	if !mu.TryLock() {
		log.Printf("scheduler: syncRepo: already in progress for %s, skipping", project.Slug)
		return nil
	}
	defer mu.Unlock()

	log.Printf("scheduler: syncRepo: start project=%s", project.Slug)
	start := time.Now()

	localRepoDir := filepath.Join(s.cachePath, project.ID, "repo")
	if err := os.MkdirAll(localRepoDir, 0755); err != nil {
		return fmt.Errorf("mkdir repo: %w", err)
	}

	if err := localGitCloneOrPull(project, localRepoDir); err != nil {
		return fmt.Errorf("local git: %w", err)
	}
	log.Printf("scheduler: syncRepo: git done (%s)", time.Since(start).Round(time.Millisecond))

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
		log.Printf("scheduler: syncRepo: no output paths for %s, done", project.Slug)
		return nil
	}

	machine, proxy, err := s.findMachineForRsync()
	if err != nil {
		return fmt.Errorf("no machine for sync: %w", err)
	}
	log.Printf("scheduler: syncRepo: using machine=%s for sync (%d output dirs)", machine.Name, len(outputDirs))

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
			log.Printf("scheduler: syncRepo: %s not under %s, skipping", outputDir, project.RemotePath)
			continue
		}
		rel, _ := filepath.Rel(project.RemotePath, outputDir)
		localDir := filepath.Join(localRepoDir, rel)

		syncStart := time.Now()
		if err := client.SyncDirToLocal(outputDir, localDir); err != nil {
			log.Printf("scheduler: syncRepo: sync failed %s: %v", outputDir, err)
			continue
		}
		log.Printf("scheduler: syncRepo: synced %s (%s)", outputDir, time.Since(syncStart).Round(time.Millisecond))
	}

	log.Printf("scheduler: syncRepo: done project=%s (total %s)", project.Slug, time.Since(start).Round(time.Millisecond))
	return nil
}

// SyncRepoNow est appelé depuis l'UI (handler HTTP). Lance le sync dans une
// goroutine pour ne pas bloquer la requête HTTP.
func (s *Scheduler) SyncRepoNow(projectID string) error {
	project, err := LoadProject(s.cachePath, projectID)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	go func() {
		if err := s.syncRepo(project); err != nil {
			log.Printf("scheduler: SyncRepoNow: %v", err)
		}
	}()
	return nil
}

// finalizeJobInline finalise un job directement depuis monitorLoop, avec le
// watcher déjà ouvert. Pas de goroutine, pas de nouvelle connexion SSH.
// status="" signifie stall — la décision pending/stalled est prise ici.
func (s *Scheduler) finalizeJobInline(watcher *Client, job *Job, localJobDir string, status JobStatus) {
	// Copier les logs finaux puis nettoyer le dossier NFS.
	if err := watcher.FinalizeLogsToLocal(job.NfsJobDir, localJobDir); err != nil {
		log.Printf("scheduler: finalize: logs copy failed for job %s: %v", job.ID, err)
	}

	s.mu.Lock()

	// Libérer la machine.
	for _, state := range s.machineStates {
		if state.CurrentJobID == job.ID {
			state.CurrentJobID = ""
			break
		}
	}

	now := time.Now()

	if status == "" {
		// Stall — retry ou abandon.
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
		// Marquer le projet pour sync au prochain syncTick.
		s.syncDirty[job.ProjectID] = true
		log.Printf("scheduler: finalize: job=%s → %s (project %s marked dirty)", job.ID, status, job.ProjectID)
	}

	if err := SaveJob(s.cachePath, job); err != nil {
		log.Printf("scheduler: finalize: save job %s failed: %v", job.ID, err)
	}
	s.emit(EventJobStatus, job)

	s.mu.Unlock()
}

// findMachineForRsync retourne une machine et son proxy à partir de
// machineStates — sans ouvrir de connexion SSH.
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
				log.Printf("scheduler: findMachineForRsync: found running machine=%s", m.Name)
				return m, p, nil
			}
		}
	}

	// Fallback : machine non deprecated et non en cours de launch.
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
		log.Printf("scheduler: findMachineForRsync: fallback machine=%s", m.Name)
		return m, p, nil
	}

	return nil, nil, fmt.Errorf("no available machine")
}
