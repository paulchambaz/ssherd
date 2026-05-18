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
	Project        Project
	Input          SlurmInput
	Partition      string
	GresSpec       string
	MaxTimeStr     string
	ArrayEnd       int
	Seeds          []int
	AxisDecls      []slurmAxisDecl
	AblationParts  string
	OutputPathBash string
	OutputFiles    []string
	HasResume      bool
	HasLog         bool
	HasOutput      bool
}

func GenerateSlurmScript(project Project, input SlurmInput) string {
	partition := slurmPartition(input.MaxHours)
	gres := slurmGres(partition, input.PreferredGPU)

	total := input.NumSeeds
	for _, ax := range input.Axes {
		if len(ax.Values) > 0 {
			total *= len(ax.Values)
		}
	}

	seeds := make([]int, input.NumSeeds)
	for i := range seeds {
		seeds[i] = input.StartSeed + i
	}

	// Build axis declarations with index expressions (seeds innermost, axis[0] outermost)
	axisDecls := make([]slurmAxisDecl, len(input.Axes))
	divisor := input.NumSeeds
	for i := len(input.Axes) - 1; i >= 0; i-- {
		ax := input.Axes[i]
		n := len(ax.Values)
		if n == 0 {
			n = 1
		}
		varName := fmt.Sprintf("AXIS_%d", i)
		namedVar := ""
		if ax.Name != "" {
			namedVar = strings.ToUpper(strings.ReplaceAll(ax.Name, "-", "_")) + "_VAL"
		}
		axisDecls[i] = slurmAxisDecl{
			VarName:   varName,
			NamedVar:  namedVar,
			Values:    ax.Values,
			IndexExpr: fmt.Sprintf("$(( (SLURM_ARRAY_TASK_ID / %d) %% %d ))", divisor, n),
		}
		divisor *= n
	}

	// Build ablation expression: "${AXIS_0} ${AXIS_1} ..."
	ablationParts := make([]string, len(axisDecls))
	for i, d := range axisDecls {
		ablationParts[i] = "${" + d.VarName + "}"
	}

	// Resolve placeholders in OutputPath
	outputPathBash := resolveSlurmPlaceholders(input.OutputPath, input.Axes)

	// Parse OutputFiles
	var outputFiles []string
	for _, line := range strings.Split(input.OutputFiles, "\n") {
		if f := strings.TrimSpace(line); f != "" {
			outputFiles = append(outputFiles, f)
		}
	}

	data := slurmTemplateData{
		Project:        project,
		Input:          input,
		Partition:      partition,
		GresSpec:       gres,
		MaxTimeStr:     fmt.Sprintf("%02d:00:00", input.MaxHours),
		ArrayEnd:       total - 1,
		Seeds:          seeds,
		AxisDecls:      axisDecls,
		AblationParts:  strings.Join(ablationParts, " "),
		OutputPathBash: outputPathBash,
		OutputFiles:    outputFiles,
		HasResume:      input.RetrySuffix != "",
		HasLog:         input.LogArgument != "" && input.LogPath != "",
		HasOutput:      input.OutputArgument != "" && input.OutputPath != "",
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

var slurmTemplate = `#!/bin/bash
#
# Prerequisites:
#   1. uv must be available in $PATH on compute nodes
#   2. Project cloned and up to date at: [[.Project.RemotePath]]
#
#SBATCH --job-name=[[.Input.NamePrefix]]
#SBATCH --partition=[[.Partition]]
#SBATCH --gres=[[.GresSpec]]
#SBATCH --time=[[.MaxTimeStr]]
#SBATCH --cpus-per-task=4
#SBATCH --mem=32G
#SBATCH --output=[[.Project.RemotePath]]/logs/slurm_%A_%a.out
#SBATCH --error=[[.Project.RemotePath]]/logs/slurm_%A_%a.err
#SBATCH --array=0-[[.ArrayEnd]]

# --- Parameters ---
SEEDS=([[range .Seeds]][[.]] [[end]])
[[-  range .AxisDecls]]
[[.VarName]]_VALUES=([[range .Values]]"[[.]]" [[end]])
[[-  end]]

N_SEEDS=${#SEEDS[@]}
[[- range .AxisDecls]]
N_[[.VarName]]=${#[[.VarName]]_VALUES[@]}
[[- end]]

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
ABLATION=$(echo "[[.AblationParts]]" | sed 's/ /_/g; s/-/_/g; s/__*/_/g; s/^_*//; s/_*$//')
[ -z "$ABLATION" ] && ABLATION="run"
[[end]]
# --- Paths ---
[[- if .HasOutput]]
OUTPUT_LOCAL="$SLURM_GPUTMPDIR/[[.Project.DataPath]]/[[.OutputPathBash]]"
OUTPUT_NFS="[[.Project.RemotePath]]/[[.OutputPathBash]]"

mkdir -p "$OUTPUT_LOCAL" "$OUTPUT_NFS" "[[.Project.RemotePath]]/logs"
[[- else]]
mkdir -p "[[.Project.RemotePath]]/logs"
[[- end]]

# --- Sync ---
sync_watch() {
    rsync -a \
[[- range .OutputFiles]]
        --include='[[.]]' \
[[- end]]
        --exclude='*' \
        "$OUTPUT_LOCAL/" "$OUTPUT_NFS/" 2>/dev/null || true
}

sync_full() {
    rsync -a \
[[- range .OutputFiles]]
        --include='[[.]]' \
[[- end]]
        --include='*.ckpt' \
        --exclude='*' \
        "$OUTPUT_LOCAL/" "$OUTPUT_NFS/" 2>/dev/null || true
}

trap sync_full EXIT

background_sync() {
    while true; do sleep 600; sync_watch; done
}
background_sync &
SYNC_BG_PID=$!
[[if .HasResume]]
# --- Resume ---
RESUME_FLAG=""
if [ -f "$OUTPUT_NFS/checkpoint.ckpt" ]; then
    cp "$OUTPUT_NFS/checkpoint.ckpt" "$OUTPUT_LOCAL/checkpoint.ckpt"
    RESUME_FLAG="[[.Input.RetrySuffix]]"
fi
[[end]]
# --- Run ---
cd [[.Project.RemotePath]]

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
"${CMD[@]}"

kill $SYNC_BG_PID 2>/dev/null
wait $SYNC_BG_PID 2>/dev/null
`
