# Design Document — SLURM Backend for ssherd

**Version 1.0**

---

## 1. Context and motivation

ssherd currently manages jobs exclusively through direct SSH connections to individual machines (PPTI model): it picks a machine, opens a connection, writes a `run.sh` on NFS, launches it with `nohup`, and monitors liveness through a heartbeat file. This works well for the PPTI because each machine is an independent unit that ssherd controls entirely.

The ISIR cluster and Jean-Zay operate on a fundamentally different model: you submit a job description to a scheduler (SLURM), which decides where and when to run it. You never manage individual machines — you manage jobs. ssherd needs a second backend that speaks SLURM instead of bare SSH.

The goal of this document is to specify how to extend ssherd with a `SlurmBackend` that supports the ISIR cluster first, then Jean-Zay, while keeping the existing PPTI backend untouched and the user-facing interface (New Batch form, job list, visualizations) identical across all backends.

The user retains responsibility for the initial git clone of each project on each cluster. ssherd takes over from there: it performs a `git pull` before each new batch submission, manages job arrays, monitors progress, and syncs results back.

---

## 2. Data model changes

### 2.1 Machine types

The current data model has two entity types: `Proxy` and `Machine`. This needs to be extended to reflect the real diversity of compute targets. The new taxonomy has four types: `proxy` (a gateway used for SSH jumping, unchanged), `ppti` (the existing bare-SSH machines), `isir` (the ISIR SLURM cluster), and `jeanzay` (the IDRIS SLURM cluster). The type is declared when adding a machine in the UI, and it determines which backend is used when dispatching jobs.

The `machines.json` structure gains a `type` field on each machine entry, and a new top-level `clusters` array that holds cluster-level configuration (as opposed to individual node configuration):

```json
{
  "proxies": [...],
  "machines": [
    {
      "id": "abc123",
      "type": "ppti",
      "name": "ppti-14-408-01",
      "hostname": "ppti-14-408-01.ufr-info-p6.jussieu.fr",
      "user": "21400464",
      "proxy_id": "xyz789",
      "gpu_model": "RTX A4500",
      "temporary_path": "/temporary",
      "status": "available"
    }
  ],
  "clusters": [
    {
      "id": "isir-cluster",
      "type": "isir",
      "name": "ISIR Cluster",
      "hostname": "cluster.isir.upmc.fr",
      "user": "chambaz",
      "proxy_id": "isir-gateway-id",
      "home_path": "/home/chambaz",
      "jobs_path": "/home/chambaz/jobs",
      "logs_path": "/home/chambaz/jobs/logs",
      "partitions": {
        "1h":   "gpu-1heure",
        "1d":   "gpu-1jour",
        "1w":   "gpu-1semaine",
        "inf":  "gpu-infini"
      },
      "gpu_types": ["a6000", "v100"],
      "status": "available"
    },
    {
      "id": "jeanzay",
      "type": "jeanzay",
      "name": "Jean-Zay",
      "hostname": "jean-zay.idris.fr",
      "user": "chambaz",
      "proxy_id": "jeanzay-gateway-id",
      "home_path": "$HOME",
      "work_path": "$WORK",
      "scratch_path": "$SCRATCH",
      "jobs_path": "$WORK/jobs",
      "logs_path": "$WORK/jobs/logs",
      "partitions": {
        "dev":  "gpu_p13",
        "1h":   "gpu_p13",
        "20h":  "gpu_p13",
        "100h": "gpu_p13"
      },
      "accounts": ["your_account@v100", "your_account@a100", "your_account@h100"],
      "status": "available"
    }
  ]
}
```

The key insight behind separating `machines` from `clusters` is that a PPTI machine is a single node ssherd controls directly, while a SLURM cluster is a single SSH endpoint that gives access to hundreds of nodes via job submission. They are structurally different and deserve different representations.

### 2.2 Project paths per cluster

A project currently has a single `RemotePath` which was designed for the PPTI NFS. With multiple backends, each project needs to know where it lives on each cluster. This is stored as a map in `project.json`:

```json
{
  "id": "a1b2c3d4",
  "slug": "isir",
  "name": "ISIR Internship",
  "remote_path": "/users/nfs/Vrac/paulchambaz/isir",
  "data_path": "paulchambaz/isir",
  "cluster_paths": {
    "isir-cluster": "/home/chambaz/isir",
    "jeanzay":      "$WORK/isir"
  },
  "git_repo": "https://github.com/paulchambaz/isir",
  "branch": "master",
  "git_token": "ghp_xxxxxxxxxxxx"
}
```

When generating a SLURM script for a given cluster, `GenerateSlurmScript` looks up `project.ClusterPaths[cluster.ID]` to get the correct project root on that cluster. If the path is not configured, the UI shows an error asking the user to set it up before submitting.

### 2.3 Job extensions for SLURM

The `job.json` gains a `backend` field (`"ppti"`, `"isir"`, or `"jeanzay"`) and a `slurm_job_id` field that holds the array job ID returned by `sbatch`. The `machine` field, previously a machine name, becomes optional and is empty for SLURM jobs since ssherd does not control node placement. A new `slurm_task_states` map holds the per-task state as ssherd understands it, derived from polling `sacct`:

```json
{
  "id": "e5f6a7b8",
  "backend": "isir",
  "slurm_job_id": "943076",
  "slurm_array_size": 30,
  "slurm_task_states": {
    "0": "done",
    "1": "running",
    "2": "pending",
    "5": "failed"
  },
  "cluster_id": "isir-cluster",
  "script_path": "/home/chambaz/jobs/job_20260518_155358.sh"
}
```

---

## 3. The SLURM backend architecture

### 3.1 Interface definition

The existing scheduler logic is tightly coupled to the SSH/PPTI model. The cleanest refactoring introduces a `Backend` interface in `internal/backend.go` that both the existing and new backends implement:

```go
type Backend interface {
    // Submit generates and submits a batch of jobs, returns an opaque job ID
    Submit(project Project, form FormState, cluster Cluster) (string, error)

    // Poll returns the current state of each task in the batch
    Poll(job Job, cluster Cluster) (map[string]TaskState, error)

    // SyncOutput copies results from the cluster to the local cache
    SyncOutput(job Job, cluster Cluster, outputFiles []string) error

    // Cancel stops all running and pending tasks
    Cancel(job Job, cluster Cluster) error
}
```

`PPTIBackend` implements this interface using the existing SSH/heartbeat/NFS logic. `SlurmBackend` implements it using `sbatch`, `sacct`, and rsync over SSH. Jean-Zay is a `SlurmBackend` with a different `Cluster` config — no new backend type needed, just a different configuration.

### 3.2 Hardcoded constants for ISIR

The following values are currently hardcoded in the generated script and should be promoted to named constants or cluster config fields in Go before this feature ships, so they can be changed without editing templates:

```go
const (
    ISIRScratchVar      = "SLURM_GPUTMPDIR"    // GPU NVMe scratch on ISIR
    ISIRDefaultCPUs     = 4
    ISIRDefaultMemGB    = 32
    SyncWatchInterval   = 600                   // seconds between background syncs
    SlurmPollInterval   = 60                    // seconds between sacct polls
    CheckpointPattern   = "*.ckpt"
)

// Partition selection from requested hours
func slurmPartition(cluster Cluster, maxHours int) string {
    switch {
    case maxHours <= 1:
        return cluster.Partitions["1h"]
    case maxHours <= 24:
        return cluster.Partitions["1d"]
    case maxHours <= 168:
        return cluster.Partitions["1w"]
    default:
        return cluster.Partitions["inf"]
    }
}
```

---

## 4. Submission flow

### 4.1 Git pull before submission

When the user clicks "Start ISIR" (or "Start Jean-Zay"), ssherd opens an SSH connection to the cluster frontale and runs a `git pull` in the project's cluster path before generating the script. This ensures the code on the cluster is up to date with the remote repo:

```go
func (b *SlurmBackend) gitPull(conn *ssh.Client, projectPath string) error {
    cmd := fmt.Sprintf("cd %s && git pull origin %s", projectPath, project.Branch)
    return conn.Run(cmd)
}
```

If `git pull` fails (conflict, detached HEAD, network issue), the submission is aborted and the error is shown in the UI. The user must resolve it manually — ssherd never force-pushes or resolves conflicts automatically.

### 4.2 Script generation and storage

After the git pull, `GenerateSlurmScript` produces the bash script exactly as described in the export SLURM design doc. ssherd then copies the script to `cluster.JobsPath` on the cluster via SSH, naming it with a timestamp:

```
/home/chambaz/jobs/isir_<project_slug>_<timestamp>.sh
```

This mirrors exactly what the standalone `submit` tool does manually today. The script is stored permanently in `~/jobs/` so the user can inspect it, resubmit it manually, or pass it to `retry` at any time.

### 4.3 sbatch invocation

ssherd runs `sbatch` over SSH and captures the output to extract the job array ID:

```go
output, err := conn.RunOutput(fmt.Sprintf("sbatch %s", scriptPath))
// output is "Submitted batch job 943076"
jobID := strings.TrimPrefix(strings.TrimSpace(output), "Submitted batch job ")
```

The job ID is stored in `job.SlurmJobID` and persisted in `job.json`. From this point, all monitoring uses this ID.

---

## 5. Monitoring and observability

### 5.1 Polling with sacct

The PPTI backend uses a heartbeat file updated every 94 seconds. The SLURM backend replaces this with periodic `sacct` polling every `SlurmPollInterval` seconds. The poll runs over the persistent watcher SSH connection:

```go
func (b *SlurmBackend) Poll(job Job, cluster Cluster) (map[string]TaskState, error) {
    cmd := fmt.Sprintf(
        "sacct -j %s --format=JobID,State,ExitCode --noheader -X",
        job.SlurmJobID,
    )
    output, err := b.watcherConn.RunOutput(cmd)
    // parse each line: "943076_0   COMPLETED   0:0"
    // map task index → TaskState
}
```

`TaskState` is an enum: `Pending`, `Running`, `Done`, `Failed`, `Timeout`, `Cancelled`. ssherd maps SLURM states to these: `COMPLETED` → `Done`, `FAILED` → `Failed`, `TIMEOUT` → `Timeout`, `CANCELLED` → `Cancelled`, `RUNNING` → `Running`, `PENDING` → `Pending`.

A task is considered definitively finished (for the purpose of triggering `SyncOutput`) when it transitions to any terminal state: `Done`, `Failed`, or `Timeout`.

### 5.2 Progress reading via progress.json

For each running task, ssherd reads `progress.json` from the NFS output path every `SlurmPollInterval` seconds via the watcher connection:

```go
progressPath := fmt.Sprintf("%s/%s/progress.json",
    cluster.HomePath + "/" + project.ClusterPaths[cluster.ID] + "/results/...",
    taskOutputPath,
)
content, err := b.watcherConn.ReadFile(progressPath)
```

The `progress.json` format is already defined by the `Ssherd` Python class: `current_step`, `total_steps`, `start_time`, `current_time`. ssherd uses this to compute completion percentage and estimated time remaining, exactly as it does today for PPTI jobs. The UI displays per-task progress bars in the job detail view.

### 5.3 Determining when a task is done

A SLURM task is marked `done` in ssherd when `sacct` reports `COMPLETED` for that task index. ssherd does not rely on any file written by the job itself to signal completion — `sacct` is the authoritative source of truth. This is cleaner than the PPTI model where a `status` file written by the job script was the signal.

The flow is: `sacct` returns `COMPLETED` for task N → ssherd calls `SyncOutput` for that task → files are copied from `cluster.HomePath/.../results/...` to local cache → task is marked `done` in `job.json` → UI updates via WebSocket.

---

## 6. Output synchronization

### 6.1 During execution

The generated bash script handles in-job sync autonomously via `sync_watch` (every 10 minutes) and `sync_full` (on EXIT trap). This means `meta.json`, `results.pkl`, and `progress.json` appear on NFS without ssherd doing anything. ssherd reads them passively via the watcher connection.

### 6.2 After task completion

When a task reaches a terminal state, `SyncOutput` copies the output files from the cluster NFS to the local ssherd cache. It uses rsync over SSH with the same include/exclude patterns as the bash script, but from the local machine to the cluster rather than within the cluster:

```go
func (b *SlurmBackend) SyncOutput(job Job, cluster Cluster, outputFiles []string) error {
    remoteDir := fmt.Sprintf("%s:%s",
        cluster.User + "@" + cluster.Hostname,
        remoteOutputPath,
    )
    // rsync with ProxyJump via cluster.ProxyID
    // include only outputFiles patterns + *.ckpt if resume is possible
}
```

Checkpoints are included in the sync only if `job.RetryCount < job.MaxRetries` — once a job has exhausted its retries, checkpoints are excluded to save bandwidth and local disk space.

---

## 7. UI changes

### 7.1 Machine management

The `/machines` page gains a type selector when adding a new machine: `ppti`, `isir`, or `jeanzay`. For PPTI machines, the form is unchanged. For `isir` and `jeanzay`, the form shows cluster-specific fields: `home_path`, `jobs_path`, `logs_path`, and the partitions map. The GPU model selector is replaced by a GPU type selector (`a6000`, `v100` for ISIR; `v100`, `a100`, `h100` for Jean-Zay).

### 7.2 Project settings

The project settings page gains a "Cluster paths" section where the user declares where the project is cloned on each configured cluster. This is a simple key-value map: cluster name → absolute path. ssherd does not clone the repo — the user does this once manually and registers the path here.

### 7.3 New Batch form

The form gains a "Target" selector at the top: `PPTI`, `ISIR Cluster`, `Jean-Zay`. Selecting a SLURM target replaces the GPU/VRAM/machine fields with cluster-specific fields: partition (derived from `MaxHours`), GPU type preference, and account (for Jean-Zay). The "Start batch" button label changes to match the selected target. The "To ISIR cluster" export button remains available as a standalone option for users who want to manage submission manually.

### 7.4 Job list and detail

The job list shows a `backend` badge per job (`PPTI`, `ISIR`, `JZ`). The job detail view shows per-task progress for SLURM jobs: a table with task index, SLURM state, progress percentage from `progress.json`, and elapsed time. The existing single-job progress bar is replaced by an aggregated view (mean completion across all tasks) with an expandable per-task breakdown.

---

## 8. Implementation order

The work naturally splits into four sequential phases. The first phase is the data model refactoring: adding `type` to machines, introducing the `clusters` array, adding `cluster_paths` to projects, and updating the UI forms. This is purely structural and does not change any scheduling behavior.

The second phase is the `Backend` interface extraction: wrapping the existing PPTI logic in a `PPTIBackend` struct that implements the interface, without changing its behavior. This is a refactoring phase — all existing tests should pass unchanged.

The third phase is the `SlurmBackend` implementation for ISIR: `Submit` (git pull + script generation + sbatch), `Poll` (sacct), `SyncOutput` (rsync), and `Cancel` (scancel). The monitoring loop in `scheduler.go` calls the backend interface methods rather than the PPTI-specific functions.

The fourth phase is Jean-Zay: registering it as a second `SlurmBackend` instance with a different `Cluster` config. At this point the only Jean-Zay-specific code is the partition/account configuration — everything else is inherited from the SLURM backend.
