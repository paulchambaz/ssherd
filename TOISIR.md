# Design Document — Export SLURM Script (ssherd)

**Version 1.1 — Cluster ISIR**

---

## 1. Contexte et objectif

ssherd orchestre aujourd'hui des jobs sur des machines accessibles en SSH direct (PPTI), en gérant lui-même le placement, le monitoring via heartbeat NFS, et le transfert de fichiers. L'objectif de cette fonctionnalité est de permettre à l'utilisateur de réutiliser exactement le même formulaire "New Batch" pour générer un script `sbatch` valide, prêt à être soumis sur le cluster ISIR, en un clic.

Le cluster ISIR tourne sous SLURM. La correspondance mentale entre les deux systèmes est la suivante : là où ssherd lance un `nohup zsh run.sh` sur une machine choisie dynamiquement et surveille un heartbeat NFS, SLURM prend en charge le placement, la durée de vie du process, et la gestion des ressources. Le script généré encode tout ce que ssherd ferait au runtime, mais de façon statique et déclarative.

La fonctionnalité se matérialise par un bouton **"To ISIR cluster"** sur la page New Batch, qui soumet le formulaire à un endpoint dédié. Cet endpoint génère le script bash et le retourne en téléchargement direct, sans créer de jobs dans ssherd.

---

## 2. Infrastructure du cluster ISIR

### 2.1 Matériel

Le cluster dispose de 29 nœuds, 1224 CPUs, 2 To de RAM, et 12 GPUs répartis en deux modèles : 2× Nvidia Tesla V100-32G et 10× Nvidia RTX A6000-48Go. Il tourne sous Ubuntu Server 20.04 LTS avec SLURM comme ordonnanceur.

### 2.2 Partitions disponibles

La commande `sinfo -o "%P %G" | grep gpu` révèle la configuration réelle des partitions. Il y a deux familles : les partitions préfixées `cpu-*` et celles préfixées `gpu-*`. Contrairement à ce que le nommage suggère, les deux familles ont des GPUs déclarés (a6000 ou v100, toujours par groupe de 2 par nœud). La convention est d'utiliser les partitions `gpu-*` pour les jobs GPU car c'est là que `$SLURM_GPUTMPDIR` (le scratch NVMe ultra-rapide) est disponible.

Les partitions `gpu-*` existantes sont les suivantes, chacune disponible avec A6000 ou V100 sauf la dernière :

```
gpu-1heure    gpu:a6000:2  /  gpu:v100:2
gpu-1jour     gpu:a6000:2  /  gpu:v100:2
gpu-1semaine  gpu:a6000:2  /  gpu:v100:2
gpu-infini    gpu:v100:2   (V100 uniquement)
```

La partition `gpu-infini` est un cas particulier : elle ne propose que des V100, ce qui signifie qu'un job demandant explicitement un A6000 ne peut pas y être soumis.

La partition marquée d'un `*` dans `sinfo` (`cpu-1heure*`) est la partition par défaut — si on omet `--partition`, c'est elle qui est utilisée. Il est donc impératif de spécifier explicitement la partition GPU dans le script généré.

### 2.3 Spécification des GPUs

Pour demander un GPU spécifique, on utilise la directive `--gres` avec le nom SLURM du modèle :

```bash
--gres=gpu:a6000:1   # un RTX A6000 48Go
--gres=gpu:v100:1    # un Tesla V100 32Go
--gres=gpu:1         # n'importe quel GPU disponible
```

Ces noms (`a6000`, `v100`) sont les identifiants tels que déclarés dans la configuration SLURM du cluster ISIR — ils correspondent à ce que `sinfo` retourne dans la colonne GRES.

### 2.4 Variable de scratch GPU

Pour les partitions `gpu-*`, la variable d'environnement de scratch n'est **pas** `$SLURM_TMPDIR` (scratch CPU) mais `$SLURM_GPUTMPDIR`, qui pointe vers `/gpu-scratch` — un volume NVMe. C'est là que doivent aller tous les fichiers écrits intensivement pendant l'entraînement (checkpoints, outputs). Le script doit utiliser `$SLURM_GPUTMPDIR` exclusivement pour les partitions `gpu-*`.

### 2.5 Stockage NFS

`/home` est monté sur tous les nœuds de calcul via NFS. C'est là que vivent les fichiers personnels (quota 1 To). `/argile` est également monté sur tous les nœuds mais lent — à éviter pour l'I/O de calcul. Il est formellement déconseillé de lancer des calculs directement depuis ces espaces. Tout I/O intensif passe par `$SLURM_GPUTMPDIR`, avec un rsync vers `/home` en fin de job.

---

## 3. Référence des commandes SLURM utiles

Cette section sert de référence intégrée pour toute la chaîne de travail autour de la fonctionnalité.

### 3.1 Soumission et gestion de jobs

`sbatch <script.sh>` soumet un script batch pour exécution différée. `srun` obtient une allocation et exécute une commande de façon interactive (utile pour déboguer). `salloc` obtient une allocation sans l'exécuter immédiatement.

Les directives de soumission les plus pertinentes pour ce projet sont les suivantes. `--array=<indexes>` spécifie un job array (ex. `--array=0-29` ou `--array=0-29%10` pour limiter à 10 tâches simultanées). `--job-name=<name>` donne un nom au job, visible dans `squeue`. `--partition=<name>` choisit la partition. `--gres=<name[:type]:count>` demande des ressources génériques comme les GPUs. `--time=<HH:MM:SS>` fixe la limite de wall clock — SLURM tue le job au-delà. `--mem=<MB>` fixe la mémoire maximale par nœud. `--cpus-per-task=<count>` fixe le nombre de CPUs par tâche. `--output=<filename>` et `--error=<filename>` redirigent stdout et stderr vers des fichiers, avec les patterns `%A` (job array ID) et `%a` (indice de tâche) pour nommer les fichiers par tâche.

Pour annuler des jobs, `scancel <jobid>` annule un job entier, `scancel <jobid>_[0-5]` annule les tâches 0 à 5 d'un array, et `scancel -u <user>` annule tous les jobs d'un utilisateur.

### 3.2 Observation et monitoring

`squeue -u <user>` affiche les jobs en cours et en attente pour un utilisateur. Le format est personnalisable : `squeue -u chambaz --format="%.10i %.9P %.30j %.8T %.10M %R"` donne l'ID, la partition, le nom, l'état, le temps écoulé, et la raison d'attente. Les états possibles sont `PD` (pending), `R` (running), `CG` (completing), `F` (failed), `CA` (cancelled), `TO` (timeout), `CD` (completed).

`sinfo` affiche l'état des nœuds et des partitions. `sinfo -o "%P %G"` montre les partitions avec leurs ressources GRES. `sinfo --long` donne plus de détails. `sinfo --partition=gpu-1jour` filtre sur une partition précise.

`sacct` est la commande post-mortem essentielle — elle interroge la base de données d'accounting SLURM pour voir l'état de jobs terminés. La commande la plus utile pour l'observabilité d'un array est :

```bash
sacct -j <array_job_id> --format=JobID,JobName,State,ExitCode,Elapsed,Start,End -X
```

Le flag `-X` supprime les étapes de job pour ne montrer que les jobs parents. `ExitCode` affiche le code de retour du process Python sous la forme `exitcode:signal`. Pour voir uniquement les tâches échouées : `sacct -j <id> --state=FAILED,TIMEOUT -X`.

`scontrol show job <jobid>` donne l'état détaillé d'un job en cours, incluant les nœuds alloués, le temps restant, et les ressources consommées.

### 3.3 Variables d'environnement disponibles dans le script

SLURM injecte automatiquement des variables dans l'environnement du script en cours d'exécution. Les plus utiles pour ce projet sont `$SLURM_ARRAY_JOB_ID` (ID du job array), `$SLURM_ARRAY_TASK_ID` (indice de la tâche courante, de 0 à N-1), `$SLURM_JOB_ID` (ID du job individuel), `$SLURM_JOB_PARTITION` (partition effectivement allouée), `$SLURM_GPUTMPDIR` (chemin du scratch NVMe GPU), et `$SLURM_CPUS_PER_TASK` (nombre de CPUs alloués).

---

## 4. Informations disponibles dans le formulaire

Tout le contenu nécessaire à la génération du script est déjà présent dans le `FormState` existant, à l'exception d'un seul champ nouveau. Le `FormState` pertinent contient `NamePrefix` (nom du job SLURM), `BaseCommand` (commande Python de base), `Axes` (liste d'axes d'ablation, chacun avec un nom optionnel et une liste de valeurs qui sont des flags shell complets comme `--algo sac`), `SeedFlag` / `StartSeed` / `NumSeeds`, `RetrySuffix` (typiquement `--resume`), `LogArgument` / `LogPath`, `OutputArgument` / `OutputPath` (avec placeholders `{ablation}`, `{seed}`, `{nom_axe}`), et `OutputFiles` (fichiers à surveiller, ex. `meta.json`, `results.pkl`).

Côté projet, on dispose de `RemotePath` (chemin NFS absolu du projet, ex. `/users/nfs/Vrac/paulchambaz/isir`) et `DataPath` (chemin relatif sous le scratch, ex. `paulchambaz/isir`).

Le seul champ nouveau à ajouter au `FormState` est `MaxHours int` — la durée maximale estimée d'un job individuel en heures entières. Il pilote le choix de la partition et la directive `--time`.

---

## 5. Logique de sélection de partition et de GPU

### 5.1 Partition selon la durée

```go
func slurmPartition(maxHours int) string {
    switch {
    case maxHours <= 1:
        return "gpu-1heure"
    case maxHours <= 24:
        return "gpu-1jour"
    case maxHours <= 168:
        return "gpu-1semaine"
    default:
        // gpu-infini est V100 uniquement — à noter dans un commentaire du script
        return "gpu-infini"
    }
}
```

### 5.2 GPU selon le PreferredGPU du formulaire

Le champ `PreferredGPU` du formulaire utilise actuellement les noms des GPUs PPTI ("RTX A4500", "RTX 3080"). Pour le cluster ISIR, il faut mapper vers les identifiants SLURM. Un champ séparé `SlurmGPU` dans les settings, ou une simple UI dans le script exporté avec deux options (a6000 / v100 / any), est plus propre qu'un mapping fragile. La valeur par défaut est `gpu:1` (n'importe quel GPU). Si la partition est `gpu-infini`, forcer `gpu:v100:1` car c'est la seule option disponible.

---

## 6. Structure du script généré

### 6.1 Directives SLURM

```bash
#!/bin/bash
#SBATCH --job-name=<NamePrefix>
#SBATCH --partition=<partition>
#SBATCH --gres=gpu:<type>:1
#SBATCH --time=<MaxHours>:00:00
#SBATCH --cpus-per-task=4
#SBATCH --mem=32G
#SBATCH --output=<RemotePath>/logs/slurm_%A_%a.out
#SBATCH --error=<RemotePath>/logs/slurm_%A_%a.err
#SBATCH --array=0-<N-1>
```

### 6.2 Décomposition de l'indice en paramètres

`$SLURM_ARRAY_TASK_ID` est un entier de 0 à N-1. Le mapping vers les combinaisons (axes × seeds) suit une décomposition en base mixte avec les seeds en poids faible — même ordre que ssherd pour la cohérence des indices. Pour deux axes de tailles `N0` et `N1` et `S` seeds :

```bash
SEEDS=(1 2 3)
AXIS_0_VALUES=("--algo sac" "--algo afu")
AXIS_1_VALUES=("--action-dim 5" "--action-dim 10" "--action-dim 15")

N_SEEDS=${#SEEDS[@]}
N_AXIS_1=${#AXIS_1_VALUES[@]}
N_AXIS_0=${#AXIS_0_VALUES[@]}

SEED_IDX=$(( SLURM_ARRAY_TASK_ID % N_SEEDS ))
AXIS_1_IDX=$(( (SLURM_ARRAY_TASK_ID / N_SEEDS) % N_AXIS_1 ))
AXIS_0_IDX=$(( (SLURM_ARRAY_TASK_ID / (N_SEEDS * N_AXIS_1)) % N_AXIS_0 ))

SEED=${SEEDS[$SEED_IDX]}
AXIS_0=${AXIS_0_VALUES[$AXIS_0_IDX]}
AXIS_1=${AXIS_1_VALUES[$AXIS_1_IDX]}

# Construction du slug d'ablation (même logique que ssherd)
ABLATION=$(echo "${AXIS_0} ${AXIS_1}" | sed 's/ /_/g; s/-/_/g; s/__*/_/g')
```

### 6.3 Résolution des chemins

L'`OutputPath` du formulaire contient des placeholders textuels (`{ablation}`, `{seed}`) que la fonction Go `resolveSlurmPlaceholders` convertit en références bash (`${ABLATION}`, `${SEED}`) avant d'écrire le template. Le chemin local scratch et le chemin NFS sont alors :

```bash
OUTPUT_LOCAL="$SLURM_GPUTMPDIR/<DataPath>/<OutputPath résolu>"
OUTPUT_NFS="<RemotePath>/<OutputPath résolu>"
```

### 6.4 Synchronisation et robustesse

Le script embarque deux fonctions de sync. `sync_watch` transfère uniquement les fichiers listés dans `OutputFiles` (les fichiers légers de suivi) via `rsync` avec des filtres `--include` / `--exclude`. `sync_full` y ajoute le pattern `*.ckpt` pour le checkpoint. Un `trap sync_full EXIT` garantit la synchronisation complète à toute terminaison — fin normale, timeout SLURM (SIGTERM), ou `scancel`. Un loop en arrière-plan appelle `sync_watch` toutes les 10 minutes pour rendre les résultats accessibles en cours de run.

```bash
sync_watch() {
    rsync -a \
        --include='meta.json' \
        --include='results.pkl' \
        --include='progress.json' \
        --exclude='*' \
        "$OUTPUT_LOCAL/" "$OUTPUT_NFS/"
}

sync_full() {
    rsync -a \
        --include='meta.json' \
        --include='results.pkl' \
        --include='progress.json' \
        --include='*.ckpt' \
        --exclude='*' \
        "$OUTPUT_LOCAL/" "$OUTPUT_NFS/"
}

trap sync_full EXIT

background_sync() {
    while true; do sleep 600; sync_watch; done
}
background_sync &
SYNC_BG_PID=$!
```

### 6.5 Resume

Si `RetrySuffix` est non-vide, le script vérifie la présence d'un checkpoint dans `$OUTPUT_NFS` et le copie dans le scratch avant de lancer Python :

```bash
RESUME_FLAG=""
if [ -f "$OUTPUT_NFS/checkpoint.ckpt" ]; then
    cp "$OUTPUT_NFS/checkpoint.ckpt" "$OUTPUT_LOCAL/checkpoint.ckpt"
    RESUME_FLAG="<RetrySuffix>"
fi
```

### 6.6 Commande finale et nettoyage

```bash
mkdir -p "$OUTPUT_LOCAL" "$OUTPUT_NFS/../../logs"

cd <RemotePath>

uv run <BaseCommand> \
    $AXIS_0 $AXIS_1 \
    <SeedFlag> $SEED \
    <LogArgument> "$OUTPUT_LOCAL/<LogPath>" \
    <OutputArgument> "$OUTPUT_LOCAL" \
    $RESUME_FLAG

kill $SYNC_BG_PID 2>/dev/null
wait $SYNC_BG_PID 2>/dev/null
# trap EXIT déclenche sync_full ici automatiquement
```

---

## 7. Implémentation Go

### 7.1 Modification du FormState

Ajouter `MaxHours int` dans la struct `FormState` (fichier `internal/job.go` ou `internal/args.go`). Ce champ est persisté dans `job.json` comme les autres même s'il ne sert qu'à la génération SLURM.

### 7.2 Nouvelle route

Dans `daemon/routes.go`, ajouter `POST /projects/{slug}/jobs/export-slurm`. Cette route ne crée pas de jobs dans ssherd — elle génère et retourne un fichier.

### 7.3 Handler

Le handler parse le formulaire exactement comme le handler de création de batch (réutiliser la fonction de parsing existante), appelle `GenerateSlurmScript`, et retourne la réponse avec les headers de téléchargement :

```go
func (s *Server) handleExportSlurm(w http.ResponseWriter, r *http.Request) {
    slug := r.PathValue("slug")
    project := s.store.GetProjectBySlug(slug)
    form := parseJobForm(r) // même parsing que la création

    script := internal.GenerateSlurmScript(project, form)

    filename := fmt.Sprintf("slurm_%s.sh", internal.Slugify(form.NamePrefix))
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    w.Header().Set("Content-Disposition",
        fmt.Sprintf(`attachment; filename="%s"`, filename))
    fmt.Fprint(w, script)
}
```

Comme c'est un POST standard (pas htmx) qui retourne un fichier, le navigateur déclenche le téléchargement nativement sans JavaScript supplémentaire.

### 7.4 Fonction de génération

Dans `internal/slurm.go` :

```go
type slurmData struct {
    Project        Project
    Form           FormState
    Total          int       // taille du job array
    Partition      string
    GresSpec       string    // ex. "gpu:a6000:1"
    Seeds          []int
    AxisArrays     [][]string // valeurs de chaque axe
    OutputPathBash string    // OutputPath avec placeholders → vars bash
    MaxTimeStr     string    // ex. "08:00:00"
}

func GenerateSlurmScript(project Project, form FormState) string {
    total := form.NumSeeds
    for _, axis := range form.Axes {
        total *= len(axis.Values)
    }

    partition := slurmPartition(form.MaxHours)
    gres := slurmGres(partition, form.PreferredGPU)
    outputPathBash := resolveSlurmPlaceholders(form.OutputPath, form.Axes)

    seeds := make([]int, form.NumSeeds)
    for i := range seeds {
        seeds[i] = form.StartSeed + i
    }

    axisArrays := make([][]string, len(form.Axes))
    for i, axis := range form.Axes {
        axisArrays[i] = axis.Values
    }

    data := slurmData{
        Project:        project,
        Form:           form,
        Total:          total,
        Partition:      partition,
        GresSpec:       gres,
        Seeds:          seeds,
        AxisArrays:     axisArrays,
        OutputPathBash: outputPathBash,
        MaxTimeStr:     fmt.Sprintf("%02d:00:00", form.MaxHours),
    }

    tmpl := template.Must(template.New("slurm").Parse(slurmTemplate))
    var buf strings.Builder
    tmpl.Execute(&buf, data)
    return buf.String()
}

func slurmPartition(maxHours int) string {
    switch {
    case maxHours <= 1:
        return "gpu-1heure"
    case maxHours <= 24:
        return "gpu-1jour"
    case maxHours <= 168:
        return "gpu-1semaine"
    default:
        return "gpu-infini" // V100 uniquement
    }
}

func slurmGres(partition, preferredGPU string) string {
    // gpu-infini ne propose que des V100
    if partition == "gpu-infini" {
        return "gpu:v100:1"
    }
    switch preferredGPU {
    case "a6000", "RTX A6000":
        return "gpu:a6000:1"
    case "v100", "V100":
        return "gpu:v100:1"
    default:
        return "gpu:1"
    }
}

// resolveSlurmPlaceholders convertit {ablation} → ${ABLATION},
// {seed} → ${SEED}, et {nom_axe} → ${NOM_AXE_VAL} pour usage dans le template bash.
func resolveSlurmPlaceholders(outputPath string, axes []Axis) string {
    result := outputPath
    result = strings.ReplaceAll(result, "{ablation}", "${ABLATION}")
    result = strings.ReplaceAll(result, "{seed}", "${SEED}")
    for _, axis := range axes {
        if axis.Name != "" {
            varName := strings.ToUpper(strings.ReplaceAll(axis.Name, "-", "_"))
            result = strings.ReplaceAll(result,
                "{"+axis.Name+"}", "${"+varName+"_VAL}")
        }
    }
    return result
}
```

### 7.5 Modification de l'UI

Dans `views/jobs.templ`, ajouter dans la section Infrastructure du formulaire un champ `MaxHours`, et dans la barre de boutons le bouton d'export via `formaction` HTML standard :

```html
<!-- Champ MaxHours dans la section Infrastructure -->
<div class="flex flex-col gap-1.5 px-4 py-3 w-full md:w-32">
    <label class="text-xs font-semibold text-base-500 uppercase tracking-wide">
        Max hours / job
    </label>
    <input type="number" name="max_hours" value="4" min="1"
        class="bg-transparent text-sm font-mono focus:outline-none">
</div>

<!-- Bouton dans la barre d'actions -->
<button type="submit"
    formaction="/projects/{slug}/jobs/export-slurm"
    formmethod="POST"
    class="px-4 py-2 text-sm font-medium border border-base-400 rounded-md hover:bg-base-200">
    To ISIR cluster
</button>
```

---

## 8. Ce que le script généré suppose

Le script généré fait deux hypothèses qui doivent être satisfaites manuellement avant soumission. Premièrement, `uv` doit être disponible dans le `$PATH` sur les nœuds de calcul du cluster ISIR — à vérifier avec le service informatique ou via une session `srun` interactive. Deuxièmement, le projet doit être cloné et à jour dans `RemotePath` sur le NFS du cluster avant soumission. Ces deux prérequis sont documentés dans un commentaire en tête du script généré.
