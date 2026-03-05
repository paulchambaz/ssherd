package internal

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type MachineState struct {
	MachineID    string
	CurrentJobID string
	IsWatcher    bool
	LastChecked  time.Time
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

type Scheduler struct {
	cfgMu     sync.RWMutex
	cfg       SchedulerConfig
	cachePath string

	mu            sync.RWMutex
	jobs          []*Job
	machineStates map[string]*MachineState

	watcherMu        sync.Mutex
	watcherClient    *Client
	watcherMachineID string

	// Events reçoit une copie snapshot du job à chaque changement d'état ou de
	// progression. Le serveur HTTP lit ce canal pour diffuser via WebSocket.
	Events chan JobEvent
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

// emit envoie un snapshot du job dans le canal Events.
// On copie le job pour éviter toute race avec les mutations du scheduler.
func (s *Scheduler) emit(kind EventKind, job *Job) {
	snapshot := *job
	if job.Progress != nil {
		p := *job.Progress
		snapshot.Progress = &p
	}
	select {
	case s.Events <- JobEvent{Kind: kind, Job: &snapshot}:
	default:
		// canal plein : on laisse tomber plutôt que de bloquer
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
	defer s.mu.Unlock()

	store, err := LoadMachinesStore(s.cachePath)
	if err != nil {
		log.Printf("scheduler: failed to reload machines store: %v", err)
		return
	}

	maxRunning := int(math.Floor(float64(len(store.Machines)) * s.getConfig().UseRatio))
	if maxRunning == 0 {
		return
	}

	running := s.countRunning()
	if running >= maxRunning {
		return
	}

	job := s.nextPending()
	if job == nil {
		return
	}

	machine := s.findAvailableMachine(store, job.GPURequirements)
	if machine == nil {
		log.Printf("scheduler: no available machine for job %s", job.ID)
		return
	}

	project, err := LoadProject(s.cachePath, job.ProjectID)
	if err != nil {
		log.Printf("scheduler: project %s not found for job %s: %v", job.ProjectID, job.ID, err)
		return
	}

	if _, ok := s.machineStates[machine.ID]; !ok {
		s.machineStates[machine.ID] = &MachineState{MachineID: machine.ID}
	}
	s.machineStates[machine.ID].CurrentJobID = job.ID

	job.Status = JobRunning
	now := time.Now()
	job.StartedAt = &now
	job.Machine = machine.Name

	if err := SaveJob(s.cachePath, job); err != nil {
		log.Printf("scheduler: failed to save job %s: %v", job.ID, err)
	}

	s.emit(EventJobStatus, job)
	log.Printf("scheduler: dispatching job %s → machine %s", job.ID, machine.Name)
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

// findAvailableMachine retourne la première machine libre et compatible.
// Préfère les machines avec le GPU demandé (tri soft).
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
	proxy := store.FindProxy(machine.ProxyID)
	if proxy == nil {
		log.Printf("scheduler: proxy not found for machine %s", machine.Name)
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

	client, err := Connect(cfg)
	if err != nil {
		log.Printf("scheduler: connect to %s failed: %v", machine.Name, err)
		s.revertJob(job, machine)
		return
	}
	defer client.Close()

	// Après :
	if _, _, code, err := client.Run("nvidia-smi -L"); err != nil || code != 0 {
		log.Printf("scheduler: nvidia-smi failed on %s, marking deprecated", machine.Name)
		s.markMachineDeprecated(store, machine)
		s.revertJob(job, machine)
		return
	}

	if job.GPURequirements.MinVRAMMB > 0 {
		freeRaw, _, code, err := client.Run(
			"nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits 2>/dev/null | head -1",
		)
		if err != nil || code != 0 {
			log.Printf("scheduler: cannot query free VRAM on %s, skipping", machine.Name)
			s.revertJob(job, machine)
			return
		}
		freeMiB, err := strconv.Atoi(strings.TrimSpace(freeRaw))
		if err != nil {
			log.Printf("scheduler: cannot parse free VRAM %q on %s, skipping", strings.TrimSpace(freeRaw), machine.Name)
			s.revertJob(job, machine)
			return
		}
		// MinVRAMMB est en MB, freeMiB est en MiB — approximation acceptable (1 MiB ≈ 1.048 MB)
		if freeMiB < job.GPURequirements.MinVRAMMB {
			log.Printf("scheduler: not enough free VRAM on %s (free: %d MiB, required: %d MB), requeueing",
				machine.Name, freeMiB, job.GPURequirements.MinVRAMMB)
			s.revertJob(job, machine)
			return
		}
		log.Printf("scheduler: VRAM ok on %s (%d MiB free)", machine.Name, freeMiB)
	}

	if err := client.GitPull(project); err != nil {
		log.Printf("scheduler: git pull failed for job %s on %s: %v", job.ID, machine.Name, err)
		s.revertJob(job, machine)
		return
	}

	if err := client.RunBackground(LaunchParams{Job: job, Project: project}); err != nil {
		log.Printf("scheduler: launch job %s on %s failed: %v", job.ID, machine.Name, err)
		s.revertJob(job, machine)
		return
	}

	log.Printf("scheduler: job %s running on %s", job.ID, machine.Name)

	s.watcherMu.Lock()
	if s.watcherClient == nil {
		watcherClient, err := Connect(cfg)
		if err == nil {
			s.watcherClient = watcherClient
			s.watcherMachineID = machine.ID
			s.mu.Lock()
			s.machineStates[machine.ID].IsWatcher = true
			s.mu.Unlock()
			log.Printf("scheduler: watcher set to %s", machine.Name)
		}
	}
	s.watcherMu.Unlock()
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
	for {
		time.Sleep(s.getConfig().MonitorInterval)
		s.monitorTick()
	}
}

func (s *Scheduler) monitorTick() {
	s.mu.RLock()
	var running []*Job
	for _, j := range s.jobs {
		if j.Status == JobRunning {
			running = append(running, j)
		}
	}
	s.mu.RUnlock()

	if len(running) == 0 {
		return
	}

	watcher, err := s.ensureWatcher()
	if err != nil {
		log.Printf("scheduler: no watcher available: %v", err)
		return
	}

	for _, job := range running {
		s.checkJob(watcher, job)
	}
}

func (s *Scheduler) checkJob(watcher *Client, job *Job) {
	statusOut, _, _, err := watcher.Run(fmt.Sprintf("cat %s/status 2>/dev/null", job.NfsJobDir))
	if err != nil {
		log.Printf("scheduler: cannot read status for job %s: %v", job.ID, err)
		return
	}

	localJobDir := jobDir(s.cachePath, job.ProjectID, job.ID)

	switch strings.TrimSpace(statusOut) {
	case "done":
		if err := watcher.FinalizeLogsToLocal(job.NfsJobDir, localJobDir); err != nil {
			log.Printf("scheduler: finalize logs failed for job %s: %v", job.ID, err)
		}
		s.finalizeJob(job, JobDone)
		return
	case "failed":
		if err := watcher.FinalizeLogsToLocal(job.NfsJobDir, localJobDir); err != nil {
			log.Printf("scheduler: finalize logs failed for job %s: %v", job.ID, err)
		}
		s.finalizeJob(job, JobFailed)
		return
	}

	// Vérifier le heartbeat pour détecter les stalls
	hbOut, _, _, err := watcher.Run(fmt.Sprintf("cat %s/heartbeat 2>/dev/null", job.NfsJobDir))
	if err == nil && strings.TrimSpace(hbOut) != "" {
		hbTime, err := time.Parse(time.RFC3339, strings.TrimSpace(hbOut))
		if err == nil && time.Since(hbTime) > s.getConfig().StallTimeout {
			log.Printf("scheduler: job %s stalled (last heartbeat %v ago)", job.ID, time.Since(hbTime).Round(time.Second))
			if err := watcher.FinalizeLogsToLocal(job.NfsJobDir, localJobDir); err != nil {
				log.Printf("scheduler: finalize logs failed for stalled job %s: %v", job.ID, err)
			}
			s.handleStall(job)
			return
		}
	}

	if err := watcher.SyncLogsToLocal(job.NfsJobDir, localJobDir); err != nil {
		log.Printf("scheduler: sync logs failed for job %s: %v", job.ID, err)
	}

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

	s.syncProgress(watcher, job)
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

func (s *Scheduler) finalizeJob(job *Job, status JobStatus) {
	s.mu.Lock()

	job.Status = status
	now := time.Now()
	job.FinishedAt = &now

	// Si le job est done, on force progress à 100%
	if status == JobDone && job.Progress != nil {
		job.Progress.Percent = 100
		job.Progress.CurrentStep = job.Progress.TotalSteps
		job.Progress.ETASeconds = 0
		job.Progress.LastUpdated = now
	}

	for _, state := range s.machineStates {
		if state.CurrentJobID == job.ID {
			state.CurrentJobID = ""
			if state.IsWatcher {
				state.IsWatcher = false
				s.watcherMu.Lock()
				if s.watcherClient != nil {
					s.watcherClient.Close()
					s.watcherClient = nil
				}
				s.watcherMachineID = ""
				s.watcherMu.Unlock()
			}
			break
		}
	}

	if err := SaveJob(s.cachePath, job); err != nil {
		log.Printf("scheduler: failed to save job %s: %v", job.ID, err)
	}

	s.emit(EventJobStatus, job)
	log.Printf("scheduler: job %s → %s", job.ID, status)

	s.mu.Unlock()

	go func() {
		project, err := LoadProject(s.cachePath, job.ProjectID)
		if err != nil {
			log.Printf("scheduler: finalize sync: load project %s: %v", job.ProjectID, err)
			return
		}
		if err := s.syncRepo(project); err != nil {
			log.Printf("scheduler: finalize sync: %v", err)
			return
		}
		if len(job.OutputFiles) == 0 {
			return
		}
		watcher, err := s.ensureWatcher()
		if err != nil {
			log.Printf("scheduler: finalize sync: no watcher for cleanup: %v", err)
			return
		}
		if err := watcher.DeleteOutputFiles(job.OutputFiles); err != nil {
			log.Printf("scheduler: finalize sync: cleanup failed: %v", err)
		}
	}()
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

func (s *Scheduler) ensureWatcher() (*Client, error) {
	s.watcherMu.Lock()
	defer s.watcherMu.Unlock()

	if s.watcherClient != nil {
		if s.watcherClient.IsAlive() {
			return s.watcherClient, nil
		}
		log.Printf("scheduler: watcher connection lost, reconnecting...")
		s.watcherClient.Close()
		s.watcherClient = nil
		s.watcherMachineID = ""
	}

	store, err := LoadMachinesStore(s.cachePath)
	if err != nil {
		return nil, fmt.Errorf("reload store: %w", err)
	}

	s.mu.RLock()
	var candidate *Machine
	for _, j := range s.jobs {
		if j.Status != JobRunning || j.Machine == "" {
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
	s.mu.RUnlock()

	if candidate == nil {
		return nil, fmt.Errorf("no running machine available for watcher")
	}

	proxy := store.FindProxy(candidate.ProxyID)
	if proxy == nil {
		return nil, fmt.Errorf("proxy not found for machine %s", candidate.Name)
	}

	cfg := SSHConfig{
		GatewayHost:     proxy.Hostname,
		GatewayPort:     proxy.Port,
		GatewayUser:     proxy.User,
		GatewayPassword: proxy.Password,
		TargetHost:      candidate.Hostname,
		TargetPort:      22,
		TargetUser:      candidate.User,
		TargetPassword:  proxy.Password,
		ConnectTimeout:  30 * time.Second,
	}

	client, err := Connect(cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to watcher %s: %w", candidate.Name, err)
	}

	s.watcherClient = client
	s.watcherMachineID = candidate.ID

	s.mu.Lock()
	if state, ok := s.machineStates[candidate.ID]; ok {
		state.IsWatcher = true
	}
	s.mu.Unlock()

	log.Printf("scheduler: watcher reconnected to %s", candidate.Name)
	return client, nil
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

// CancelJob annule un job en mémoire et, si le job est running, tue le process
// distant et écrit "cancelled" dans NfsJobDir/status pour court-circuiter le monitor.
// Retourne une erreur si le job est introuvable ou si le kill SSH échoue.
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
		return fmt.Errorf("job %s not found in scheduler", jobID)
	}

	wasRunning := job.Status == JobRunning
	machineName := job.Machine
	nfsJobDir := job.NfsJobDir

	job.Status = JobCancelled
	now := time.Now()
	job.FinishedAt = &now

	// Libérer la machine dans machineStates
	for _, state := range s.machineStates {
		if state.CurrentJobID == jobID {
			state.CurrentJobID = ""
			break
		}
	}
	s.mu.Unlock()

	if err := SaveJob(s.cachePath, job); err != nil {
		log.Printf("scheduler: cancel: failed to save job %s: %v", jobID, err)
	}
	s.emit(EventJobStatus, job)

	if !wasRunning || machineName == "" {
		return nil
	}

	// Ouvrir une connexion SSH courte vers la machine qui héberge le job
	store, err := LoadMachinesStore(s.cachePath)
	if err != nil {
		return fmt.Errorf("cancel: load machines store: %w", err)
	}
	var machine *Machine
	for _, m := range store.Machines {
		if m.Name == machineName {
			machine = m
			break
		}
	}
	if machine == nil {
		return fmt.Errorf("cancel: machine %s not found in store", machineName)
	}
	proxy := store.FindProxy(machine.ProxyID)
	if proxy == nil {
		return fmt.Errorf("cancel: proxy not found for machine %s", machineName)
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
		return fmt.Errorf("cancel: connect to %s: %w", machineName, err)
	}
	defer client.Close()

	// Écrire "cancelled" EN PREMIER pour court-circuiter le monitor
	// avant même de tuer le process, pour éviter la race condition avec run.sh
	if _, _, _, err := client.Run(fmt.Sprintf("echo cancelled > %s/status", nfsJobDir)); err != nil {
		log.Printf("scheduler: cancel: failed to write status on NFS: %v", err)
	}

	// Lire le pid et tuer le process
	pidRaw, err := client.ReadRemoteFile(nfsJobDir + "/pid")
	if err == nil && strings.TrimSpace(pidRaw) != "" {
		pid := strings.TrimSpace(pidRaw)
		if _, _, code, err := client.Run("kill " + pid + " 2>/dev/null"); err != nil || code != 0 {
			log.Printf("scheduler: cancel: kill %s on %s failed (may already be dead)", pid, machineName)
		} else {
			log.Printf("scheduler: cancel: killed pid %s on %s", pid, machineName)
		}
	}

	return nil
}

// RequeueJob remet un job en pending dans le scheduler en mémoire.
// Si le job est déjà présent dans la slice (par ID), il est mis à jour sur place
// plutôt qu'ajouté en double.
func (s *Scheduler) RequeueJob(job *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == job.ID {
			// Mise à jour sur place — évite les doublons
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

func (s *Scheduler) syncLoop() {
	for {
		time.Sleep(s.getConfig().SyncInterval)
		s.syncTick()
	}
}

func (s *Scheduler) syncTick() {
	s.mu.RLock()
	var running []*Job
	for _, j := range s.jobs {
		if j.Status == JobRunning {
			running = append(running, j)
		}
	}
	s.mu.RUnlock()

	if len(running) == 0 {
		return
	}

	// Collecter les projets uniques qui ont des jobs running
	seen := make(map[string]bool)
	for _, j := range running {
		if seen[j.ProjectID] {
			continue
		}
		seen[j.ProjectID] = true
		project, err := LoadProject(s.cachePath, j.ProjectID)
		if err != nil {
			log.Printf("scheduler: sync: load project %s: %v", j.ProjectID, err)
			continue
		}
		if err := s.syncRepo(project); err != nil {
			log.Printf("scheduler: sync: project %s: %v", project.Slug, err)
		}
	}
}

func (s *Scheduler) syncRepo(project *Project) error {
	localRepoDir := filepath.Join(s.cachePath, project.ID, "repo")
	if err := os.MkdirAll(localRepoDir, 0755); err != nil {
		return fmt.Errorf("mkdir repo: %w", err)
	}

	// Clone ou pull local en premier, toujours
	if err := localGitCloneOrPull(project, localRepoDir); err != nil {
		return fmt.Errorf("local git: %w", err)
	}

	// Collecter les output_path uniques des jobs du projet
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
		log.Printf("scheduler: sync: no output paths declared for project %s, skipping rsync", project.Slug)
		return nil
	}

	// Trouver une machine pour rsync
	store, err := LoadMachinesStore(s.cachePath)
	if err != nil {
		return fmt.Errorf("load store: %w", err)
	}

	var machine *Machine
	var proxy *Proxy

	watcher, watcherErr := s.ensureWatcher()
	if watcherErr == nil {
		s.watcherMu.Lock()
		watcherMachineID := s.watcherMachineID
		s.watcherMu.Unlock()
		machine = store.FindMachine(watcherMachineID)
	}

	if machine == nil {
		for _, m := range store.Machines {
			if m.Status == MachineStatusDeprecated {
				continue
			}
			p := store.FindProxy(m.ProxyID)
			if p == nil {
				continue
			}
			c, err := Connect(SSHConfig{
				GatewayHost:     p.Hostname,
				GatewayPort:     p.Port,
				GatewayUser:     p.User,
				GatewayPassword: p.Password,
				TargetHost:      m.Hostname,
				TargetPort:      22,
				TargetUser:      m.User,
				TargetPassword:  p.Password,
				ConnectTimeout:  15 * time.Second,
			})
			if err != nil {
				log.Printf("scheduler: sync: connect to %s failed: %v", m.Name, err)
				continue
			}
			machine = m
			proxy = p
			c.Close()
			break
		}
	} else {
		_ = watcher
		proxy = store.FindProxy(machine.ProxyID)
	}

	if machine == nil {
		return fmt.Errorf("no reachable machine available for rsync")
	}
	if proxy == nil {
		return fmt.Errorf("proxy not found for machine %s", machine.Name)
	}

	sshCmd := fmt.Sprintf(
		"ssh -o StrictHostKeyChecking=no -o ProxyJump=%s@%s:%d",
		proxy.User, proxy.Hostname, proxy.Port,
	)

	for outputDir := range outputDirs {
		if !strings.HasPrefix(outputDir, project.RemotePath) {
			log.Printf("scheduler: sync: output path %s not under remote path %s, skipping", outputDir, project.RemotePath)
			continue
		}
		rel, _ := filepath.Rel(project.RemotePath, outputDir)
		localDir := filepath.Join(localRepoDir, rel)
		if err := os.MkdirAll(localDir, 0755); err != nil {
			log.Printf("scheduler: sync: mkdir %s: %v", localDir, err)
			continue
		}
		src := fmt.Sprintf("%s@%s:%s/", machine.User, machine.Hostname, outputDir)
		dst := localDir + "/"
		cmd := exec.Command("rsync", "-az", "--checksum", "-e", sshCmd, src, dst)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("scheduler: sync: rsync %s failed: %v\n%s", outputDir, err, out)
			continue
		}
		log.Printf("scheduler: sync: synced %s → %s", outputDir, localDir)
	}

	log.Printf("scheduler: sync: done for project %s", project.Slug)
	return nil
}

func (s *Scheduler) SyncRepoNow(projectID string) error {
	project, err := LoadProject(s.cachePath, projectID)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	return s.syncRepo(project)
}
