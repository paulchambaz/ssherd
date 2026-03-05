package internal

import (
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHConfig struct {
	GatewayHost     string
	GatewayPort     int
	GatewayUser     string
	GatewayPassword string

	TargetHost     string
	TargetPort     int
	TargetUser     string
	TargetPassword string

	ConnectTimeout time.Duration
}

type Client struct {
	gateway *ssh.Client
	target  *ssh.Client
}

func Connect(cfg SSHConfig) (*Client, error) {
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}

	gatewayCfg := &ssh.ClientConfig{
		User:            cfg.GatewayUser,
		Auth:            []ssh.AuthMethod{ssh.Password(cfg.GatewayPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         cfg.ConnectTimeout,
	}

	targetCfg := &ssh.ClientConfig{
		User:            cfg.TargetUser,
		Auth:            []ssh.AuthMethod{ssh.Password(cfg.TargetPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         cfg.ConnectTimeout,
	}

	gatewayAddr := fmt.Sprintf("%s:%d", cfg.GatewayHost, cfg.GatewayPort)
	gatewayClient, err := ssh.Dial("tcp", gatewayAddr, gatewayCfg)
	if err != nil {
		return nil, fmt.Errorf("gateway connection failed: %w", err)
	}

	targetAddr := fmt.Sprintf("%s:%d", cfg.TargetHost, cfg.TargetPort)
	tunnelConn, err := gatewayClient.Dial("tcp", targetAddr)
	if err != nil {
		gatewayClient.Close()
		return nil, fmt.Errorf("tunnel to target failed: %w", err)
	}

	ncc, chans, reqs, err := ssh.NewClientConn(tunnelConn, targetAddr, targetCfg)
	if err != nil {
		tunnelConn.Close()
		gatewayClient.Close()
		return nil, fmt.Errorf("target SSH handshake failed: %w", err)
	}

	return &Client{
		gateway: gatewayClient,
		target:  ssh.NewClient(ncc, chans, reqs),
	}, nil
}

func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Run exécute une commande via zsh -i -c sur la machine cible.
func (c *Client) Run(command string) (stdout string, stderr string, exitCode int, err error) {
	session, err := c.target.NewSession()
	if err != nil {
		return "", "", -1, fmt.Errorf("new session failed: %w", err)
	}
	defer session.Close()

	var stdoutBuf, stderrBuf strings.Builder
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	wrapped := "zsh -i -c " + shellEscape(command)
	runErr := session.Run(wrapped)

	stdout = stdoutBuf.String()
	stderr = stderrBuf.String()

	if runErr != nil {
		if exitErr, ok := runErr.(*ssh.ExitError); ok {
			return stdout, stderr, exitErr.ExitStatus(), nil
		}
		return stdout, stderr, -1, runErr
	}

	return stdout, stderr, 0, nil
}

// ReadRemoteFile lit un fichier distant et retourne son contenu.
// Retourne une chaîne vide sans erreur si le fichier n'existe pas encore.
func (c *Client) ReadRemoteFile(remotePath string) (string, error) {
	stdout, _, code, err := c.Run(fmt.Sprintf("cat %s 2>/dev/null", remotePath))
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", nil
	}
	return stdout, nil
}

// SyncLogsToLocal copie stdout.log et stderr.log depuis NFS vers le cache local.
// Appelé à chaque tick du monitor pour garder les logs locaux à jour.
func (c *Client) SyncLogsToLocal(nfsJobDir, localJobDir string) error {
	if err := os.MkdirAll(localJobDir, 0755); err != nil {
		return fmt.Errorf("cannot create local job dir: %w", err)
	}

	for _, filename := range []string{"stdout.log", "stderr.log"} {
		content, err := c.ReadRemoteFile(filepath.Join(nfsJobDir, filename))
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", filename, err)
		}
		if content == "" {
			continue
		}
		localPath := filepath.Join(localJobDir, filename)
		if err := os.WriteFile(localPath, []byte(content), 0644); err != nil {
			return fmt.Errorf("cannot write %s locally: %w", filename, err)
		}
	}

	return nil
}

// FinalizeLogsToLocal fait une dernière copie complète de tous les fichiers
// importants depuis NFS vers le cache local, puis supprime le dossier NFS.
// Appelé une seule fois quand le job se termine (done, failed, stalled).
func (c *Client) FinalizeLogsToLocal(nfsJobDir, localJobDir string) error {
	if err := os.MkdirAll(localJobDir, 0755); err != nil {
		return fmt.Errorf("cannot create local job dir: %w", err)
	}

	// Copier tous les fichiers utiles
	for _, filename := range []string{"stdout.log", "stderr.log", "exit_code", "pid"} {
		content, err := c.ReadRemoteFile(filepath.Join(nfsJobDir, filename))
		if err != nil {
			log.Printf("ssh: finalize: cannot read %s: %v", filename, err)
			continue
		}
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(localJobDir, filename), []byte(content), 0644); err != nil {
			log.Printf("ssh: finalize: cannot write %s locally: %v", filename, err)
		}
	}

	// Supprimer le dossier NFS seulement après copie réussie
	_, stderr, code, err := c.Run(fmt.Sprintf("rm -rf %s", nfsJobDir))
	if err != nil || code != 0 {
		return fmt.Errorf("rm -rf %s failed (code %d): %s", nfsJobDir, code, stderr)
	}

	log.Printf("ssh: finalize: cleaned up NFS dir %s", nfsJobDir)
	return nil
}

type LaunchParams struct {
	Job     *Job
	Project *Project
}

// RunBackground écrit un script sur NFS puis le lance via nohup zsh.
// Le git pull est fait en amont via GitPull.
func (c *Client) RunBackground(params LaunchParams) error {
	job := params.Job

	script := fmt.Sprintf(`#!/usr/bin/env zsh
JOB_DIR=%s
mkdir -p "$JOB_DIR"
echo $$ > "$JOB_DIR/pid"
echo "running" > "$JOB_DIR/status"

# reset local uniquement — le pull réseau a déjà été fait par le scheduler
git -C %s reset --hard HEAD >> "$JOB_DIR/stdout.log" 2>> "$JOB_DIR/stderr.log"

echo "=== job start ===" >> "$JOB_DIR/stdout.log"

(while true; do date -Iseconds > "$JOB_DIR/heartbeat"; sleep 120; done) &
HEARTBEAT_PID=$!

%s >> "$JOB_DIR/stdout.log" 2>> "$JOB_DIR/stderr.log"
EXIT=$?

kill $HEARTBEAT_PID 2>/dev/null
echo $EXIT > "$JOB_DIR/exit_code"
if [ $EXIT -eq 0 ]; then
    echo "done" > "$JOB_DIR/status"
else
    echo "failed" > "$JOB_DIR/status"
fi
`, job.NfsJobDir, params.Project.RemotePath, job.RunCommand)

	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	cmd := fmt.Sprintf(
		`mkdir -p %s && printf '%%s' %s | base64 -d > %s/run.sh && nohup zsh -i %s/run.sh > /dev/null 2>&1 &`,
		job.NfsJobDir, encoded, job.NfsJobDir, job.NfsJobDir,
	)

	_, stderr, exitCode, err := c.Run(cmd)
	if err != nil {
		return fmt.Errorf("launch failed: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("launch exited with code %d: %s", exitCode, stderr)
	}

	return nil
}

// GitPull fait un reset --hard puis pull sur le repo du projet.
func (c *Client) GitPull(project *Project) error {
	branch := project.Branch
	if branch == "" {
		branch = "main"
	}

	proxy := "http://proxy.ufr-info-p6.jussieu.fr:3128"

	resetCmd := fmt.Sprintf(
		"env http_proxy=%s https_proxy=%s git -C %s reset --hard HEAD",
		proxy, proxy, project.RemotePath,
	)
	if _, stderr, code, err := c.Run(resetCmd); err != nil || code != 0 {
		return fmt.Errorf("git reset failed (code %d): %s", code, stderr)
	}

	var pullCmd string
	if project.GitToken != "" {
		authenticatedURL := project.GitRepo
		if strings.HasPrefix(authenticatedURL, "https://") {
			authenticatedURL = "https://" + project.GitToken + "@" + strings.TrimPrefix(authenticatedURL, "https://")
		}
		pullCmd = fmt.Sprintf(
			"env http_proxy=%s https_proxy=%s git -C %s pull %s %s",
			proxy, proxy, project.RemotePath, authenticatedURL, branch,
		)
	} else {
		pullCmd = fmt.Sprintf(
			"env http_proxy=%s https_proxy=%s git -C %s pull origin %s",
			proxy, proxy, project.RemotePath, branch,
		)
	}

	stdout, stderr, code, err := c.Run(pullCmd)
	if err != nil {
		return fmt.Errorf("git pull error: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("git pull failed (code %d): %s", code, stderr)
	}

	log.Printf("ssh: git pull ok: %s", strings.TrimSpace(stdout))
	return nil
}

// localGitCloneOrPull fait un clone initial si le repo local n'existe pas,
// ou un pull si le clone est déjà là. Tourne localement sur le serveur.
func localGitCloneOrPull(project *Project, localRepoDir string) error {
	gitDir := filepath.Join(localRepoDir, ".git")

	authenticatedURL := project.GitRepo
	if project.GitToken != "" && strings.HasPrefix(authenticatedURL, "https://") {
		authenticatedURL = "https://" + project.GitToken + "@" + strings.TrimPrefix(authenticatedURL, "https://")
	}

	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		// Clone initial
		log.Printf("scheduler: sync: cloning %s into %s", project.GitRepo, localRepoDir)
		cmd := exec.Command("git", "clone", authenticatedURL, localRepoDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git clone failed: %w\n%s", err, out)
		}
		log.Printf("scheduler: sync: clone done for project %s", project.Slug)
	} else {
		// Pull
		branch := project.Branch
		if branch == "" {
			branch = "main"
		}
		resetCmd := exec.Command("git", "-C", localRepoDir, "reset", "--hard", "HEAD")
		if out, err := resetCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git reset failed: %w\n%s", err, out)
		}
		pullCmd := exec.Command("git", "-C", localRepoDir, "pull", authenticatedURL, branch)
		if out, err := pullCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git pull failed: %w\n%s", err, out)
		}
		log.Printf("scheduler: sync: pull done for project %s", project.Slug)
	}
	return nil
}

// IsAlive vérifie que la connexion est toujours active.
func (c *Client) IsAlive() bool {
	_, _, code, err := c.Run("echo ok")
	return err == nil && code == 0
}

func (c *Client) Close() error {
	var errs []string
	if err := c.target.Close(); err != nil {
		errs = append(errs, err.Error())
	}
	if err := c.gateway.Close(); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("close errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (c *Client) DeleteOutputFiles(files []string) error {
	if len(files) == 0 {
		return nil
	}
	var errs []string
	for _, f := range files {
		if _, _, code, err := c.Run(fmt.Sprintf("rm -f %s 2>/dev/null", f)); err != nil || code != 0 {
			errs = append(errs, f)
			log.Printf("ssh: delete output file %s failed", f)
		} else {
			log.Printf("ssh: deleted output file %s", f)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed to delete: %s", strings.Join(errs, ", "))
	}
	return nil
}
