package internal

import (
	"fmt"
	"strings"
	"text/template"
)

type SlurmInput struct {
	NamePrefix     string
	BaseCommand    string
	SeedFlag       string
	StartSeed      int
	NumSeeds       int
	MaxHours       int
	PreferredGPU   string
	RetrySuffix    string
	LogArgument    string
	LogPath        string
	OutputArgument string
	OutputPath     string
	OutputFiles    string
	Axes           []SlurmAxis
}

type SlurmAxis struct {
	Name   string
	Values []string
}

type slurmAxisDecl struct {
	VarName   string
	NamedVar  string
	Values    []string
	IndexExpr string
}

type slurmTemplateData struct {
	Project         Project
	Input           SlurmInput
	JobName         string
	ISIRProjectPath string
	Partition       string
	GresSpec        string
	MaxTimeStr      string
	ArrayEnd        int
	Seeds           []int
	AxisDecls       []slurmAxisDecl
	AblationParts   string
	OutputPathBash  string
	OutputFiles     []string
	HasResume       bool
	HasLog          bool
	HasOutput       bool
}

func GenerateSlurmScript(project Project, input SlurmInput) string {
	partition := slurmPartition(input.MaxHours)
	gres := slurmGres(partition, input.PreferredGPU)

	axes := make([]SlurmAxis, 0, len(input.Axes))
	for _, ax := range input.Axes {
		if len(ax.Values) > 0 {
			axes = append(axes, ax)
		}
	}

	total := input.NumSeeds
	for _, ax := range axes {
		total *= len(ax.Values)
	}

	seeds := make([]int, input.NumSeeds)
	for i := range seeds {
		seeds[i] = input.StartSeed + i
	}

	axisDecls := make([]slurmAxisDecl, len(axes))
	divisorTerms := []string{"N_SEEDS"}
	for i := len(axes) - 1; i >= 0; i-- {
		ax := axes[i]
		varName := fmt.Sprintf("AXIS_%d", i)
		namedVar := ""
		if ax.Name != "" {
			namedVar = strings.ToUpper(strings.ReplaceAll(ax.Name, "-", "_")) + "_VAL"
		}
		axisDecls[i] = slurmAxisDecl{
			VarName:  varName,
			NamedVar: namedVar,
			Values:   ax.Values,
			IndexExpr: fmt.Sprintf(
				"$(( (SLURM_ARRAY_TASK_ID / (%s)) %% N_%s ))",
				strings.Join(divisorTerms, " * "), varName,
			),
		}
		divisorTerms = append(divisorTerms, "N_"+varName)
	}

	ablationParts := make([]string, len(axisDecls))
	for i, d := range axisDecls {
		ablationParts[i] = "${" + d.VarName + "}"
	}

	outputPathBash := resolveSlurmPlaceholders(input.OutputPath, axes)

	var outputFiles []string
	for _, line := range strings.Split(input.OutputFiles, "\n") {
		if f := strings.TrimSpace(line); f != "" {
			outputFiles = append(outputFiles, f)
		}
	}

	jobName := strings.ReplaceAll(input.NamePrefix, " ", "_")
	// TODO: replace with project.ISIRPath once that field is added to Project
	isirProjectPath := "/home/chambaz/isir"

	hasOutput := input.OutputArgument != "" && input.OutputPath != ""

	data := slurmTemplateData{
		Project:         project,
		Input:           input,
		JobName:         jobName,
		ISIRProjectPath: isirProjectPath,
		Partition:       partition,
		GresSpec:        gres,
		MaxTimeStr:      fmt.Sprintf("%02d:00:00", input.MaxHours),
		ArrayEnd:        total - 1,
		Seeds:           seeds,
		AxisDecls:       axisDecls,
		AblationParts:   strings.Join(ablationParts, " "),
		OutputPathBash:  outputPathBash,
		OutputFiles:     outputFiles,
		HasResume:       input.RetrySuffix != "" && hasOutput,
		HasLog:          input.LogArgument != "" && input.LogPath != "",
		HasOutput:       hasOutput,
	}

	tmpl := template.Must(template.New("slurm").Delims("[[", "]]").Parse(slurmTemplate))
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "# Error generating script: " + err.Error() + "\n"
	}
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
		return "gpu-infini"
	}
}

func slurmGres(partition, preferredGPU string) string {
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

func GenerateJeanZaySlurmScript(project Project, input SlurmInput) string {
	qos := jeanZayQoS(input.MaxHours)

	axes := make([]SlurmAxis, 0, len(input.Axes))
	for _, ax := range input.Axes {
		if len(ax.Values) > 0 {
			axes = append(axes, ax)
		}
	}

	total := input.NumSeeds
	for _, ax := range axes {
		total *= len(ax.Values)
	}

	seeds := make([]int, input.NumSeeds)
	for i := range seeds {
		seeds[i] = input.StartSeed + i
	}

	axisDecls := make([]slurmAxisDecl, len(axes))
	divisorTerms := []string{"N_SEEDS"}
	for i := len(axes) - 1; i >= 0; i-- {
		ax := axes[i]
		varName := fmt.Sprintf("AXIS_%d", i)
		namedVar := ""
		if ax.Name != "" {
			namedVar = strings.ToUpper(strings.ReplaceAll(ax.Name, "-", "_")) + "_VAL"
		}
		axisDecls[i] = slurmAxisDecl{
			VarName:  varName,
			NamedVar: namedVar,
			Values:   ax.Values,
			IndexExpr: fmt.Sprintf(
				"$(( (SLURM_ARRAY_TASK_ID / (%s)) %% N_%s ))",
				strings.Join(divisorTerms, " * "), varName,
			),
		}
		divisorTerms = append(divisorTerms, "N_"+varName)
	}

	ablationParts := make([]string, len(axisDecls))
	for i, d := range axisDecls {
		ablationParts[i] = "${" + d.VarName + "}"
	}

	outputPathBash := resolveSlurmPlaceholders(input.OutputPath, axes)

	jobName := strings.ReplaceAll(input.NamePrefix, " ", "_")
	jeanZayPath := "/linkhome/rech/genisi01/utu98yp/isir"

	hasOutput := input.OutputArgument != "" && input.OutputPath != ""

	data := slurmJeanZayTemplateData{
		Project:        project,
		Input:          input,
		JobName:        jobName,
		JeanZayPath:    jeanZayPath,
		QoS:            qos,
		MaxTimeStr:     fmt.Sprintf("%02d:00:00", input.MaxHours),
		ArrayEnd:       total - 1,
		Seeds:          seeds,
		AxisDecls:      axisDecls,
		AblationParts:  strings.Join(ablationParts, " "),
		OutputPathBash: outputPathBash,
		HasResume:      input.RetrySuffix != "" && hasOutput,
		HasLog:         input.LogArgument != "" && input.LogPath != "",
		HasOutput:      hasOutput,
	}

	tmpl := template.Must(template.New("slurm-jz").Delims("[[", "]]").Parse(slurmJeanZayTemplate))
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "# Error generating script: " + err.Error() + "\n"
	}
	return buf.String()
}

func jeanZayQoS(maxHours int) string {
	switch {
	case maxHours <= 2:
		return "qos_gpu-dev"
	case maxHours <= 20:
		return "qos_gpu-t3"
	default:
		return "qos_gpu-t4"
	}
}

type slurmJeanZayTemplateData struct {
	Project        Project
	Input          SlurmInput
	JobName        string
	JeanZayPath    string
	QoS            string
	MaxTimeStr     string
	ArrayEnd       int
	Seeds          []int
	AxisDecls      []slurmAxisDecl
	AblationParts  string
	OutputPathBash string
	HasResume      bool
	HasLog         bool
	HasOutput      bool
}

func resolveSlurmPlaceholders(outputPath string, axes []SlurmAxis) string {
	result := outputPath
	result = strings.ReplaceAll(result, "{ablation}", "${ABLATION}")
	result = strings.ReplaceAll(result, "{seed}", "${SEED}")
	for _, axis := range axes {
		if axis.Name != "" {
			varName := strings.ToUpper(strings.ReplaceAll(axis.Name, "-", "_")) + "_VAL"
			result = strings.ReplaceAll(result, "{"+axis.Name+"}", "${"+varName+"}")
		}
	}
	return result
}

var slurmTemplate = `#!/bin/bash -l
#SBATCH --job-name=[[.JobName]]
#SBATCH --partition=[[.Partition]]
#SBATCH --gres=[[.GresSpec]]
#SBATCH --time=[[.MaxTimeStr]]
#SBATCH --cpus-per-task=4
#SBATCH --mem=32G
#SBATCH --output=[[.ISIRProjectPath]]/logs/slurm_%A_%a.out
#SBATCH --error=[[.ISIRProjectPath]]/logs/slurm_%A_%a.err
#SBATCH --array=0-[[.ArrayEnd]]
#SBATCH --signal=B:TERM@300

SEEDS=([[range .Seeds]][[.]] [[end]])
[[- range .AxisDecls]]
[[.VarName]]_VALUES=([[range .Values]]"[[.]]" [[end]])
[[- end]]
SYNC_INTERVAL=600

N_SEEDS=${#SEEDS[@]}
[[- range .AxisDecls]]
N_[[.VarName]]=${#[[.VarName]]_VALUES[@]}
[[- end]]
N_TOTAL=$(( N_SEEDS[[range .AxisDecls]] * N_[[.VarName]][[end]] ))
[ "$SLURM_ARRAY_TASK_ID" -ge "$N_TOTAL" ] && exit 0

SEED_IDX=$(( SLURM_ARRAY_TASK_ID % N_SEEDS ))
[[- range .AxisDecls]]
[[.VarName]]_IDX=[[.IndexExpr]]
[[- end]]

SEED=${SEEDS[$SEED_IDX]}
[[- range .AxisDecls]]
[[.VarName]]=${[[.VarName]]_VALUES[$[[.VarName]]_IDX]}
[[- end]]
[[- range .AxisDecls]][[if .NamedVar]]
[[.NamedVar]]="${[[.VarName]]}"
[[- end]][[end]]
[[if .AxisDecls]]
ABLATION=$(echo "[[.AblationParts]]" | sed 's/ /_/g; s/__*/_/g; s/^_*//; s/_*$//')
[ -z "$ABLATION" ] && ABLATION="run"
[[end]]
[[- if .HasOutput]]
OUTPUT_LOCAL="$SLURM_GPUTMPDIR/[[.Project.DataPath]]/[[.OutputPathBash]]"
OUTPUT_NFS="[[.ISIRProjectPath]]/[[.OutputPathBash]]"
mkdir -p "$OUTPUT_LOCAL" "$OUTPUT_NFS" "[[.ISIRProjectPath]]/logs"

sync_all() {
    rsync -a \
        --include='progress.json' \
[[- range .OutputFiles]]
        --include='[[.]]' \
[[- end]]
        --include='*.ckpt' \
        --exclude='*' \
        "$OUTPUT_LOCAL/" "$OUTPUT_NFS/" \
        || echo "[ssherd] WARNING: sync failed at $(date -u)" >&2
}
[[- else]]
mkdir -p "[[.ISIRProjectPath]]/logs"

sync_all() { :; }
[[- end]]

CLEANED_UP=0
cleanup() {
    [ "$CLEANED_UP" -eq 1 ] && return
    CLEANED_UP=1
    kill $SYNC_BG_PID 2>/dev/null
    kill $PYTHON_PID 2>/dev/null
    wait $PYTHON_PID 2>/dev/null
    wait $SYNC_BG_PID 2>/dev/null
    sync_all
}
trap cleanup EXIT TERM INT
[[if .HasOutput]]
background_sync() {
    while true; do sleep "$SYNC_INTERVAL"; sync_all; done
}
background_sync &
SYNC_BG_PID=$!
[[end]]
[[- if .HasResume]]
RESUME_FLAG=""
if [ -f "$OUTPUT_NFS/checkpoint.ckpt" ]; then
    rsync -a \
        --include='progress.json' \
[[- range .OutputFiles]]
        --include='[[.]]' \
[[- end]]
        --include='*.ckpt' \
        --exclude='*' \
        "$OUTPUT_NFS/" "$OUTPUT_LOCAL/"
    RESUME_FLAG="[[.Input.RetrySuffix]]"
fi
[[end]]
cd [[.ISIRProjectPath]]

CMD=(uv run [[.Input.BaseCommand]])
[[- range .AxisDecls]]
CMD+=( ${[[.VarName]]} )
[[- end]]
CMD+=([[.Input.SeedFlag]] $SEED)
[[- if .HasLog]]
CMD+=([[.Input.LogArgument]] "$OUTPUT_LOCAL/[[.Input.LogPath]]")
[[- end]]
[[- if .HasOutput]]
CMD+=([[.Input.OutputArgument]] "$OUTPUT_LOCAL")
[[- end]]
[[- if .HasResume]]
CMD+=($RESUME_FLAG)
[[- end]]
"${CMD[@]}" &
PYTHON_PID=$!
wait $PYTHON_PID
`

var slurmJeanZayTemplate = `#!/bin/bash -l
#SBATCH --job-name=[[.JobName]]
#SBATCH --partition=gpu_p13
#SBATCH --account=gyw@v100
#SBATCH --gres=gpu:1
#SBATCH --qos=[[.QoS]]
#SBATCH --time=[[.MaxTimeStr]]
#SBATCH --cpus-per-task=10
#SBATCH --output=[[.JeanZayPath]]/logs/slurm_%A_%a.out
#SBATCH --error=[[.JeanZayPath]]/logs/slurm_%A_%a.err
#SBATCH --array=0-[[.ArrayEnd]]
#SBATCH --signal=B:TERM@300

SEEDS=([[range .Seeds]][[.]] [[end]])
[[- range .AxisDecls]]
[[.VarName]]_VALUES=([[range .Values]]"[[.]]" [[end]])
[[- end]]

N_SEEDS=${#SEEDS[@]}
[[- range .AxisDecls]]
N_[[.VarName]]=${#[[.VarName]]_VALUES[@]}
[[- end]]
N_TOTAL=$(( N_SEEDS[[range .AxisDecls]] * N_[[.VarName]][[end]] ))
[ "$SLURM_ARRAY_TASK_ID" -ge "$N_TOTAL" ] && exit 0

SEED_IDX=$(( SLURM_ARRAY_TASK_ID % N_SEEDS ))
[[- range .AxisDecls]]
[[.VarName]]_IDX=[[.IndexExpr]]
[[- end]]

SEED=${SEEDS[$SEED_IDX]}
[[- range .AxisDecls]]
[[.VarName]]=${[[.VarName]]_VALUES[$[[.VarName]]_IDX]}
[[- end]]
[[- range .AxisDecls]][[if .NamedVar]]
[[.NamedVar]]="${[[.VarName]]}"
[[- end]][[end]]
[[if .AxisDecls]]
ABLATION=$(echo "[[.AblationParts]]" | sed 's/ /_/g; s/__*/_/g; s/^_*//; s/_*$//')
[ -z "$ABLATION" ] && ABLATION="run"
[[end]]
[[- if .HasOutput]]
OUTPUT="$SCRATCH/isir/[[.OutputPathBash]]"
mkdir -p "$OUTPUT" "[[.JeanZayPath]]/logs"
[[- else]]
mkdir -p "[[.JeanZayPath]]/logs"
[[- end]]

CLEANED_UP=0
cleanup() {
    [ "$CLEANED_UP" -eq 1 ] && return
    CLEANED_UP=1
    kill $PYTHON_PID 2>/dev/null
    wait $PYTHON_PID 2>/dev/null
}
trap cleanup EXIT TERM INT
[[- if .HasResume]]

RESUME_FLAG=""
if [ -f "$OUTPUT/checkpoint.ckpt" ]; then
    RESUME_FLAG="[[.Input.RetrySuffix]]"
fi
[[end]]
cd [[.JeanZayPath]]

CMD=(uv run [[.Input.BaseCommand]])
[[- range .AxisDecls]]
CMD+=( ${[[.VarName]]} )
[[- end]]
CMD+=([[.Input.SeedFlag]] $SEED)
[[- if .HasLog]]
CMD+=([[.Input.LogArgument]] "$OUTPUT/[[.Input.LogPath]]")
[[- end]]
[[- if .HasOutput]]
CMD+=([[.Input.OutputArgument]] "$OUTPUT")
[[- end]]
[[- if .HasResume]]
CMD+=($RESUME_FLAG)
[[- end]]
"${CMD[@]}" &
PYTHON_PID=$!
wait $PYTHON_PID
`
