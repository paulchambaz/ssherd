# Design Document — ssherd

**Version 0.9**

---

## 1. Vue d'ensemble

### 1.1 Objectif

Un système de gestion de jobs pour machines distantes, déployé comme container Docker sur un serveur personnel. Le système orchestre l'exécution parallèle de runs d'entraînement RL sur des machines GPU accessibles via SSH, expose une UI web simple, et repose sur le filesystem NFS partagé comme source de vérité pour la coordination — sans démon déployé sur les machines distantes.

### 1.2 Contraintes fondamentales

- **Aucun logiciel déployé sur les machines distantes** — uniquement des commandes shell standard (zsh, python via uv, nvidia-smi)
- **NFS partagé** uniquement pour les fichiers de coordination (`run.sh`, `status`, `heartbeat`, `pid`, `exit_code`) — les outputs et logs des jobs sont écrits dans le disque local `/temporary` de chaque machine
- **SSH avec gateway** en ProxyJump pour toutes les machines
- **Agnosticisme vis-à-vis des projets** — le scheduler ne connaît pas le contenu des projets, seulement des chemins et des commandes

### 1.3 Cas d'application cible

126 runs (6 ablations × 7 environnements × 3 seeds), chaque run durant 2-3h, sur ~50 machines disponibles simultanément. Objectif : complétion en une nuit (~7h). Le dispatch est limité à une plage horaire configurable (par défaut 20h–8h) pour ne pas surcharger la PPTI pendant les heures de travail des étudiants.

---

## 2. Architecture générale

```
┌──────────────────────────────────────────────────┐
│                  Serveur personnel               │
│                                                  │
│  ┌─────────────┐    ┌─────────────────────────┐  │
│  │  Go HTTP    │    │     Scheduler Daemon    │  │
│  │  + templ    │◄──►│  (orchestration loop)   │  │
│  │  + htmx-ws  │    │                         │  │
│  └─────────────┘    └──────────┬──────────────┘  │
│          │                     │                 │
│  ┌───────▼─────────────────────▼──────────────┐  │
│  │           Cache (~/.cache/ssherd/)         │  │
│  │  machines.json                             │  │
│  │  scheduler.json                            │  │
│  │  <project_id>/project.json                 │  │
│  │  <project_id>/jobs/<job_id>/job.json       │  │
│  │  <project_id>/jobs/<job_id>/stdout.log     │  │
│  │  <project_id>/jobs/<job_id>/stderr.log     │  │
│  │  <project_id>/jobs/<job_id>/progress.json  │  │
│  │  <project_id>/repo/                        │  │
│  │    └── <project.DataPath>/                 │  │
│  │  <project_id>/visualizations/<viz_id>/     │  │
│  └────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────┘
                          │ SSH (ProxyJump via gateway)
                          ▼
┌──────────────────────────────────────────────────────┐
│             Machines distantes                       │
│                                                      │
│  /users/nfs/Vrac/paulchambaz/                        │
│  └── .ssherd/jobs/<job_id>/    ← coordination NFS    │
│      ├── run.sh                                      │
│      ├── status                                      │
│      ├── heartbeat                                   │
│      ├── pid                                         │
│      └── exit_code                                   │
│                                                      │
│  {machine.TemporaryPath}/      ← stockage local      │
│  └── <project.DataPath>/                             │
│      └── <job.OutputPath>/                           │
│          ├── stdout.log   (redirigé depuis NFS)      │
│          ├── stderr.log   (redirigé depuis NFS)      │
│          ├── progress.json                           │
│          └── checkpoints/                            │
└──────────────────────────────────────────────────────┘
```

**Remarque sur stdout/stderr** : les logs du process sont redirigés vers le NFS (`{NfsJobDir}/stdout.log` et `stderr.log`) pour que le watcher puisse les lire depuis n'importe quelle machine sans connexion supplémentaire. Les données volumineuses (checkpoints, métriques, progress.json) vont dans le temporary local. Il faut donc éviter de printer des volumes importants depuis le script d'entraînement.

### 2.1 Principe de fonctionnement

Le scheduler tourne sur le serveur personnel. Il maintient une **connexion SSH persistante vers une machine "watcher"** via laquelle il accède au NFS en lecture et écriture. Pour lancer des jobs, il ouvre des connexions SSH courtes vers les machines cibles, écrit un script `run.sh` sur NFS, le lance via `nohup zsh`, et referme la connexion.

Le job tourne de façon autonome, écrit ses outputs dans `{machine.TemporaryPath}/{project.DataPath}/{job.OutputPath}/` local à la machine, et met à jour un fichier `heartbeat` sur NFS toutes les π/2 minutes exactement (94.247779607693797 secondes). Le scheduler lit via le watcher le fichier `status` pour détecter la fin. Quand un job se termine, le scheduler rsync le contenu du temporary local vers le cache local avant de nettoyer le remote.

---

## 3. Structure des données

### 3.1 Cache local

```
~/.cache/ssherd/
├── machines.json
├── scheduler.json
├── <project_id>/
│   ├── project.json
│   ├── jobs/
│   │   └── <job_id>/
│   │       ├── job.json
│   │       ├── stdout.log
│   │       ├── stderr.log
│   │       └── progress.json
│   ├── repo/
│   │   └── <project.DataPath>/   ← outputs rsync depuis les machines
│   └── visualizations/
│       └── <viz_id>/
│           ├── viz.json
│           └── outputs/
│               ├── <version>.svg
│               └── <version>.png
```

### 3.2 `machines.json`

```json
{
  "proxies": [...],
  "machines": [
    {
      "id": "def456",
      "name": "ppti-14-408-01",
      "hostname": "ppti-14-408-01.ufr-info-p6.jussieu.fr",
      "user": "21400464",
      "protocol": 2,
      "proxy_id": "abc123",
      "gpu_model": "RTX A4500",
      "temporary_path": "/temporary",
      "status": "available"
    }
  ]
}
```

`temporary_path` est la racine du disque local sur la machine (ex : `/temporary`). Le chemin complet d'un output est `{machine.TemporaryPath}/{project.DataPath}/{job.OutputPath}`.

### 3.3 `scheduler.json`

```json
{
  "use_ratio": 0.5,
  "dispatch_interval": "1m0s",
  "monitor_interval": "2m0s",
  "stall_timeout": "10m0s",
  "sync_interval": "30m0s",
  "viz_interval": "10m0s",
  "local_prefix": "",
  "dispatch_start": 20,
  "dispatch_end": 8,
  "ntfy_url": "https://ntfy.chambaz.xyz",
  "ntfy_channel": "ssherd",
  "ntfy_user": "paulchambaz",
  "ntfy_password": "..."
}
```

### 3.4 `project.json`

```json
{
  "id": "a1b2c3d4",
  "slug": "isir",
  "name": "ISIR",
  "remote_path": "/users/nfs/Vrac/paulchambaz/isir",
  "data_path": "paulchambaz/isir",
  "git_repo": "https://github.com/paulchambaz/isir",
  "branch": "master",
  "git_token": "ghp_xxxxxxxxxxxx",
  "created_at": "...",
  "updated_at": "..."
}
```

`data_path` est le chemin relatif sous `machine.TemporaryPath` où ce projet écrit ses données (ex : `paulchambaz/isir`). Le chemin complet sur une machine distante est `{machine.TemporaryPath}/{project.DataPath}/{job.OutputPath}`, soit par exemple `/temporary/paulchambaz/isir/results/antmaze-large-play-v2_1/`.

### 3.5 `job.json`

```json
{
  "id": "e5f6a7b8",
  "project_id": "a1b2c3d4",
  "project_slug": "isir",
  "display_name": "RLPD ablation - antmaze-large-play-v2 - seed 1",
  "status": "running",
  "machine": "ppti-14-408-01",
  "created_at": "...",
  "started_at": "...",
  "finished_at": null,
  "retry_count": 0,
  "max_retries": 3,
  "run_command": "cd /users/nfs/.../isir && uv run python train.py --env antmaze-large-play-v2 --seed 1 --log {temporary_path}/paulchambaz/isir/results/antmaze-large-play-v2_1/progress.json --output {temporary_path}/paulchambaz/isir/results/antmaze-large-play-v2_1/",
  "retry_command": "cd /users/nfs/.../isir && uv run python train.py --env antmaze-large-play-v2 --seed 1 --log {temporary_path}/paulchambaz/isir/results/antmaze-large-play-v2_1/progress.json --output {temporary_path}/paulchambaz/isir/results/antmaze-large-play-v2_1/ --resume",
  "log_path": "{temporary_path}/paulchambaz/isir/results/antmaze-large-play-v2_1/progress.json",
  "output_path": "results/antmaze-large-play-v2_1/",
  "nfs_job_dir": "/users/nfs/Vrac/paulchambaz/.ssherd/jobs/e5f6a7b8",
  "progress": {...},
  "gpu_requirements": {
    "min_vram_mb": 2048,
    "preferred_gpu": "RTX A4500"
  },
  "form_state": { ... }
}
```

`{temporary_path}` est un placeholder résolu au moment du lancement dans `launchJob`, quand la machine cible est connue. `output_path` est relatif au `DataPath` du projet. Le chemin absolu remote est `{machine.TemporaryPath}/{project.DataPath}/{job.OutputPath}`. Le chemin local correspondant est `{cachePath}/{projectID}/repo/{project.DataPath}/{job.OutputPath}`.

### 3.6 `viz.json`

Stocké dans `{cachePath}/{projectID}/visualizations/{vizID}/viz.json`. Le champ `data_path` pointe vers le répertoire local `{cachePath}/{projectID}/repo/{project.DataPath}/` — c'est là que `vizNeedsRegen` cherche les fichiers de données pour comparer leurs mtimes avec celles du SVG de sortie. Le champ `output_file_template` est un chemin relatif au repo local avec des placeholders `{version}` ou `{<axis_name>}`. Le champ `build_remote` est conservé dans la structure JSON pour ne pas casser les fichiers existants mais n'est plus consulté — toutes les générations passent par `generateVizLocal`.

---

## 4. Composants

### 4.1 SSH Client (`internal/ssh.go` + `internal/sync.go`)

Implémenté avec `golang.org/x/crypto/ssh`. Toutes les opérations SSH sont sérialisées via `c.mu`. Le logging de contention (attente > 100ms) et de durée (exécution > 3s) est actif. Chaque `launchJob` ouvre sa propre connexion courte, le watcher du `monitorLoop` est local à sa goroutine, et `CancelJob` ouvre une connexion dédiée dans une goroutine séparée.

**`LaunchParams`** contient désormais `Job`, `Project` et `Machine`.

**`RunBackground`** : génère un script `run.sh` qui :
- Crée `{NfsJobDir}` sur NFS pour les fichiers de coordination
- Crée `{machine.TemporaryPath}/{project.DataPath}/{job.OutputPath}/` sur le disque local via `mkdir -p`
- Résout `{temporary_path}` dans `RunCommand` par `machine.TemporaryPath`
- Redirige stdout/stderr vers `{NfsJobDir}/stdout.log` et `stderr.log` (NFS, lisibles par le watcher)
- Lance le heartbeat toutes les **94.247779607693797 secondes** exactement (= π/2 minutes = 30π secondes) via `sleep` directement, sans `bc`
- Écrit `status`, `pid`, `exit_code` dans `{NfsJobDir}`

**`SyncDirToLocal(remoteDir, localDir)`** — transfère remote → local via tar+base64, compare les mtimes pour ne transférer que ce qui a changé.

**`CopyLocalToRemote(localDir, remoteDir)`** — symétrique : tar local → base64 → SSH → décode + extrait sur le remote. Utilisé pour copier les checkpoints avant un retry sur une nouvelle machine. Compare également les mtimes.

### 4.2 Scheduler (`internal/scheduler.go`)

Quatre goroutines indépendantes :

**`dispatchLoop`** — vérifie la plage horaire (`isDispatchAllowed`) avant chaque tick. Si hors plage, skip silencieusement. Requeue automatiquement les jobs `failed` avec `retry_count < max_retries` en début de tick via `requeueFailedJobs` — ces jobs sont placés en tête de liste. Lance ensuite le premier job `pending`.

Dans **`launchJob`**, avant `RunBackground` :
1. Si des données locales existent pour ce job (`{cachePath}/{projectID}/repo/{project.DataPath}/{job.OutputPath}`), les copier vers la machine cible via `CopyLocalToRemote` — non fatal
2. Résoudre `{temporary_path}` dans `job.LogPath`
3. Lancer `RunBackground`

**`monitorLoop`** — inchangé dans sa structure. `checkJob` déclenche `syncOutputToLocal` puis `finalizeJobInline` quand un job passe `done` ou `failed`.

**`syncOutputToLocal`** : trouve la machine du job, construit `remoteOutputDir = {machine.TemporaryPath}/{project.DataPath}/{job.OutputPath}` et `localOutputDir = {cachePath}/{projectID}/repo/{project.DataPath}/{job.OutputPath}`, appelle `SyncDirToLocal` puis `rm -rf` le remote. Non fatal.

**`syncLoop`** — toutes les `sync_interval`. Pour chaque machine ayant des jobs `running`, ouvre une connexion SSH et appelle `syncMachine`. Une connexion par machine, un rsync par DataPath de projet distinct : `{machine.TemporaryPath}/{project.DataPath}/` → `{cachePath}/{projectID}/repo/{project.DataPath}/`.

**`vizLoop`** — toutes les `viz_interval`. Toutes les visualisations sont générées en local uniquement via `generateVizLocal`. `generateVizRemote` est supprimé. Le paramètre `mode` de `GenerateVizNow` est ignoré.

**Plage horaire** : `isDispatchAllowed()` compare l'heure locale avec `DispatchStart` et `DispatchEnd`. Gère correctement les plages traversant minuit (ex: 20h→8h : actif si `hour >= 20 || hour < 8`).

**Guard de génération concurrente** : inchangé.

### 4.3 Ntfy

Les notifications push sont envoyées via une instance ntfy auto-hébergée. `notify(title, msg)` construit un POST HTTP vers `{NtfyURL}/{NtfyChannel}` avec authentification Basic Auth (`NtfyUser`/`NtfyPassword`) et un timeout de 10s. L'appel est non bloquant — lancé dans une goroutine séparée. Deux événements déclenchent une notification : un job qui passe en `failed`, et la transition où tous les jobs passent de running à terminés.

### 4.4 WebSocket Hub

Le hub maintient un ensemble de canaux abonnés (`map[chan string]struct{}`), un par client WebSocket connecté. `Broadcast(html)` envoie un fragment HTML à tous les clients via un select non bloquant — les clients lents sont ignorés plutôt que bloqués. À la connexion d'un nouveau client, un snapshot complet est envoyé : état de tous les jobs actifs et état de toutes les visualisations connues. À la déconnexion, le canal est retiré et fermé.

### 4.5 Serveur HTTP (`daemon/`)

Routes :
- `POST /projects/{slug}/jobs/cancel-all`
- `POST /projects/{slug}/jobs/delete-finished`
- `POST /projects/{slug}/jobs/{id}/delete`

`relayEvents()` gère quatre types d'événements :
- `EventJobStatus` / `EventJobProgress` → `JobRowFragment` + `JobProgressFragment` + `JobLogsFragment`
- `EventVizDone` → `VizResultFragment`
- `EventJobDeleted` → `JobDeleteFragment` (retire la ligne du tableau via `hx-swap-oob="delete"`)

### 4.6 GPU Registry

`KnownGPUs` est une liste statique de modèles GPU avec leur VRAM en MiB et leurs TFLOPS FP32 approximatifs. Elle sert à deux choses : valider le champ `gpu_model` d'une machine dans le formulaire UI, et alimenter le filtre `SatisfiesRequirements` qui vérifie qu'une machine a suffisamment de VRAM libre avant dispatch. Le champ `PreferredGPU` d'un job est traité comme une préférence soft — `findAvailableMachine` favorise les machines correspondantes mais retombe sur n'importe quelle machine compatible si aucune n'est disponible.

---

## 5. API HTTP

### 5.1 Routes

```
GET  /{$}
GET  /health
GET  /ws

GET  /projects
GET  /projects/new
POST /projects
GET  /projects/{slug}
GET  /projects/{slug}/edit
POST /projects/{slug}
POST /projects/{slug}/delete

GET  /projects/{slug}/jobs
GET  /projects/{slug}/jobs/new
POST /projects/{slug}/jobs
POST /projects/{slug}/jobs/cancel-all
POST /projects/{slug}/jobs/delete-finished
GET  /projects/{slug}/jobs/{id}
POST /projects/{slug}/jobs/{id}/cancel
POST /projects/{slug}/jobs/{id}/retry
POST /projects/{slug}/jobs/{id}/edit
POST /projects/{slug}/jobs/{id}/delete
GET  /projects/{slug}/jobs/{id}/logs/stdout
GET  /projects/{slug}/jobs/{id}/logs/stderr

GET  /projects/{slug}/visualizations
GET  /projects/{slug}/visualizations/new
POST /projects/{slug}/visualizations
GET  /projects/{slug}/visualizations/{id}
POST /projects/{slug}/visualizations/{id}
GET  /projects/{slug}/visualizations/{id}/file?format=svg|png
POST /projects/{slug}/visualizations/{id}/generate
POST /projects/{slug}/visualizations/{id}/delete

GET  /projects/{slug}/files
POST /projects/{slug}/files/sync
GET  /projects/{slug}/files/download
GET  /projects/{slug}/settings

GET  /machines
GET  /machines/new
POST /machines
POST /machines/{id}/delete
POST /machines/{id}/reset

GET  /proxies/new
POST /proxies
POST /proxies/{id}/delete

GET  /settings
POST /settings
GET  /static/
```

---

## 6. Interface Web

### 6.1 Stack

templ, htmx, htmx-ext-ws, Tailwind CSS v4, Vanilla JS minimal.

### 6.2 Pages

**`/projects/{slug}/jobs` — Liste des jobs**

- Bouton **Cancel all** — annule tous les jobs `pending` et `running`
- Bouton **Delete all** — supprime tous les jobs `done`, `failed`, `cancelled`
- Chaque ligne est entièrement cliquable via `onclick` + `cursor-pointer`

**`/projects/{slug}/jobs/{id}` — Détail job**

- `retry_command` éditable inline
- Bouton **Redo this job** si `job.FormState != nil`
- Bouton **Delete job** visible uniquement si état terminal (`done`, `failed`, `cancelled`, `stalled`)

**`/projects/{slug}/jobs/new` — New Batch**

- Restauration du `FormState` via `data-form-state` si `?from={id}`

**`/projects/{slug}/visualizations/{id}` — Détail visualisation**

- Erreurs de génération affichées inline via MutationObserver sur `resultEl.parentNode` avec `{ childList: true }`
- Bouton **Generate** sans sélecteur de mode — toujours local
- Spinner immédiat si `?generating=true`

**`/settings` — Settings**

- Champs `Dispatch start` et `Dispatch end` (0–23) pour la plage horaire

### 6.3 Formulaire "New Batch"

Variables disponibles dans les templates :
- `{seed}` — numéro de seed
- `{ablation}` — slug de toutes les valeurs du combo
- `{<axis_name>}` — valeur de l'axe si nommé

Deux champs de routage vers le temporary :
- `Log argument` (défaut : `--log`) + `Log path` (relatif au DataPath) — appende `--log {temporary_path}/{DataPath}/{log_path}` à la commande
- `Output argument` (défaut : `--output`) + `Output path` (relatif au DataPath) — appende `--output {temporary_path}/{DataPath}/{output_path}`

Le placeholder `{temporary_path}` reste littéral dans `run_command` et `retry_command` stockés en JSON — il est résolu au moment du lancement quand la machine est connue.

La preview affiche les commandes avec `{temporary_path}` non résolu, ce qui est correct et attendu.

### 6.4 Boutons Copy

Trois boutons copy identiques visuellement : texte et icône en bleu accent, souligné au hover. Feedback "Copied!" pendant 2s.

---

## 7. Visualisations

### 7.1 Configuration

`name` et `description` sont éditables après création.

### 7.2 Génération double SVG + PNG

SVG fatal. PNG non fatal — l'erreur est remontée dans `VizErr` et affichée dans l'UI (préfixée `"SVG ok, PNG failed: "`).

### 7.3 Résolution des chemins de sortie

`data_path` dans `viz.json` pointe vers le chemin local `{cachePath}/{projectID}/repo/{project.DataPath}/`. Toutes les visualisations sont générées en local à partir des données rapatriées par le `syncLoop` et `syncOutputToLocal`.

### 7.4 Exécution asynchrone des visualisations

Chaque combo d'une visualisation est généré dans une goroutine indépendante. `tryMarkVizGenerating` pose un verrou sur la paire `(vizID, comboKey)` avant de lancer la goroutine — si un combo est déjà en cours de génération, il est ignoré plutôt que dupliqué. À la fin de chaque goroutine, `emitViz` envoie un `EventVizDone` avec le comboKey et l'erreur éventuelle, ce qui déclenche un fragment WebSocket qui met à jour le viewer sans rechargement de page.

### 7.5 Git pull avant génération

`localGitCloneOrPull` est appelé au début de `generateVizLocal`. `generateVizRemote` est supprimé.

### 7.6 Affichage des erreurs

Le MutationObserver observe `resultEl.parentNode` avec `{ childList: true }` — cela corrige le bug où htmx-ws remplaçait l'élément entier (OOB swap), détachant le nœud observé.

---

## 8. Structure du repository

```
.
├── main.go
├── daemon/
│   ├── server.go
│   ├── routes.go
│   ├── home.go
│   ├── projects.go
│   ├── jobs.go
│   ├── visualizations.go
│   ├── machines.go
│   └── ws.go
├── internal/
│   ├── args.go
│   ├── config.go
│   ├── event.go
│   ├── hub.go
│   ├── job.go
│   ├── machine.go
│   ├── page.go
│   ├── project.go
│   ├── scheduler.go
│   ├── ssh.go
│   ├── sync.go
│   ├── tab.go
│   ├── utils.go
│   ├── version.go
│   ├── visualization.go
│   └── viz.go
├── views/
│   ├── base.templ
│   ├── header.templ
│   ├── home.templ
│   ├── jobs.templ
│   ├── machines.templ
│   ├── notfound.templ
│   ├── project.templ
│   ├── projects.templ
│   ├── settings.templ
│   ├── visualization.templ
│   ├── fragments.templ
│   └── helpers.go
└── static/
    ├── css/styles.css
    └── js/main.js
```

---

## 9. Protocole de robustesse

### 9.1 Machines qui s'éteignent

1. Le heartbeat NFS n'est plus mis à jour (toutes les 94.247779607693797s)
2. Après `stall_timeout` (10min), le scheduler détecte le stall via `finalizeJobInline(status="")`
3. Le job est requeued avec `retry_command` si `retry_count < max_retries`, sinon passe à `stalled`
4. Les données partielles dans le temporary de la machine morte sont perdues — le retry repart des dernières données disponibles dans le cache local (dernier sync périodique)

### 9.2 GPU absent ou non fonctionnel

Au moment du lancement, `nvidia-smi -L` est exécuté. En cas d'échec, la machine passe à `deprecated` et n'est plus sélectionnée. `maxRunning` est calculé sur les machines non deprecated uniquement.

### 9.3 Perte du watcher

`openWatcher` est appelé si la connexion est morte ou absente. Si aucune machine n'est disponible parmi les jobs running, le tick est skippé silencieusement.

### 9.4 Déconnexion WebSocket client

Le hub retire le client en erreur d'écriture. À la reconnexion, le client reçoit un snapshot complet de tous les jobs actifs et de l'état des visualisations.

### 9.5 Timeouts SSH

Toutes les connexions SSH ont un `ConnectTimeout` de 30s (15s pour `CancelJob`).

### 9.6 Saturation SSH

Chaque connexion SSH (`launchJob`, `CancelJob`) est ouverte, utilisée et fermée immédiatement. `Client.Run()` sérialise les opérations via mutex — la contention est loggée si elle dépasse 100ms.

### 9.7 Race condition finalisation

`finalizingJobs map[string]bool` protégé par `finalizingMu` garantit qu'un job ne peut être finalisé que par une seule goroutine à la fois. `syncProgress` et `syncOutputToLocal` sont appelés juste avant `finalizeJobInline` pour les statuts `done` et `failed`.

### 9.8 Notifications

`notify(title, msg)` envoie un POST HTTP à l'instance ntfy configurée avec Basic Auth. Non bloquant (goroutine). Déclenché sur `JobFailed` et sur la transition "tous les jobs terminés".

### 9.9 Plage horaire de dispatch

Si l'heure courante est hors de la fenêtre `[DispatchStart, DispatchEnd[`, `dispatchTick` retourne immédiatement sans logguer. Les jobs déjà en cours continuent jusqu'à leur fin naturelle. La plage traverse correctement minuit : actif si `hour >= start || hour < end` quand `start > end`.

