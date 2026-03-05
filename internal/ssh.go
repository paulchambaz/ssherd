package internal

import (
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHConfig struct {
	GatewayHost     string
	GatewayPort     int
	GatewayUser     string
	GatewayPassword string
	TargetHost      string
	TargetPort      int
	TargetUser      string
	TargetPassword  string
	ConnectTimeout  time.Duration
}

type Client struct {
	mu      sync.Mutex
	name    string // identifiant lisible pour les logs, ex: "ppti-14-509-12"
	gateway *ssh.Client
	target  *ssh.Client
}

type LaunchParams struct {
	Job     *Job
	Project *Project
}

func Connect(cfg SSHConfig) (*Client, error) {
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}
	name := cfg.TargetHost

	log.Printf("ssh: connect → %s (via %s, timeout=%s)", name, cfg.GatewayHost, cfg.ConnectTimeout)
	start := time.Now()

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
		log.Printf("ssh: connect → %s FAILED (gateway): %v (elapsed %s)", name, err, time.Since(start).Round(time.Millisecond))
		return nil, fmt.Errorf("gateway connection failed: %w", err)
	}

	targetAddr := fmt.Sprintf("%s:%d", cfg.TargetHost, cfg.TargetPort)
	tunnelConn, err := gatewayClient.Dial("tcp", targetAddr)
	if err != nil {
		gatewayClient.Close()
		log.Printf("ssh: connect → %s FAILED (tunnel): %v (elapsed %s)", name, err, time.Since(start).Round(time.Millisecond))
		return nil, fmt.Errorf("tunnel to target failed: %w", err)
	}

	ncc, chans, reqs, err := ssh.NewClientConn(tunnelConn, targetAddr, targetCfg)
	if err != nil {
		tunnelConn.Close()
		gatewayClient.Close()
		log.Printf("ssh: connect → %s FAILED (handshake): %v (elapsed %s)", name, err, time.Since(start).Round(time.Millisecond))
		return nil, fmt.Errorf("target SSH handshake failed: %w", err)
	}

	log.Printf("ssh: connect → %s ok (%s)", name, time.Since(start).Round(time.Millisecond))
	return &Client{
		name:    name,
		gateway: gatewayClient,
		target:  ssh.NewClient(ncc, chans, reqs),
	}, nil
}

func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// truncateCmd tronque une commande pour les logs.
func truncateCmd(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// Run sérialise toutes les opérations SSH via c.mu.
// Logue le temps d'attente sur le mutex (contention) et la durée d'exécution.
func (c *Client) Run(command string) (stdout string, stderr string, exitCode int, err error) {
	waitStart := time.Now()
	c.mu.Lock()
	waitDur := time.Since(waitStart)
	// On loggue la contention si l'attente dépasse 100ms — signe que
	// le client est partagé et qu'une autre opération bloquait.
	if waitDur > 100*time.Millisecond {
		log.Printf("ssh: [%s] Run waited %s on mutex for: %s", c.name, waitDur.Round(time.Millisecond), truncateCmd(command))
	}

	execStart := time.Now()
	defer func() {
		c.mu.Unlock()
		elapsed := time.Since(execStart)
		// On loggue toujours les commandes longues (>3s) pour identifier les blocages réseau.
		if elapsed > 3*time.Second {
			log.Printf("ssh: [%s] Run took %s (exit=%d) for: %s", c.name, elapsed.Round(time.Millisecond), exitCode, truncateCmd(command))
		}
	}()

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
		log.Printf("ssh: [%s] SyncLogsToLocal: %s → %d bytes", c.name, filename, len(content))
	}
	return nil
}

func (c *Client) FinalizeLogsToLocal(nfsJobDir, localJobDir string) error {
	if err := os.MkdirAll(localJobDir, 0755); err != nil {
		return fmt.Errorf("cannot create local job dir: %w", err)
	}
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
	_, stderr, code, err := c.Run(fmt.Sprintf("rm -rf %s", nfsJobDir))
	if err != nil || code != 0 {
		return fmt.Errorf("rm -rf %s failed (code %d): %s", nfsJobDir, code, stderr)
	}
	log.Printf("ssh: finalize: cleaned up NFS dir %s", nfsJobDir)
	return nil
}

func (c *Client) RunBackground(params LaunchParams) error {
	job := params.Job
	script := fmt.Sprintf(`#!/usr/bin/env zsh
JOB_DIR=%s
mkdir -p "$JOB_DIR"
echo $$ > "$JOB_DIR/pid"
echo "running" > "$JOB_DIR/status"
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

func (c *Client) GitPull(project *Project) error {
	branch := project.Branch
	if branch == "" {
		branch = "main"
	}

	proxy := "http://proxy.ufr-info-p6.jussieu.fr:3128"

	log.Printf("ssh: [%s] GitPull: git reset on %s", c.name, project.RemotePath)
	resetCmd := fmt.Sprintf(
		"env http_proxy=%s https_proxy=%s git -C %s reset --hard HEAD",
		proxy, proxy, project.RemotePath,
	)
	if _, stderr, code, err := c.Run(resetCmd); err != nil || code != 0 {
		return fmt.Errorf("git reset failed (code %d): %s", code, stderr)
	}
	log.Printf("ssh: [%s] GitPull: git reset ok, starting pull", c.name)

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

func localGitCloneOrPull(project *Project, localRepoDir string) error {
	gitDir := filepath.Join(localRepoDir, ".git")
	authenticatedURL := project.GitRepo
	if project.GitToken != "" && strings.HasPrefix(authenticatedURL, "https://") {
		authenticatedURL = "https://" + project.GitToken + "@" + strings.TrimPrefix(authenticatedURL, "https://")
	}
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		log.Printf("scheduler: sync: cloning %s into %s", project.GitRepo, localRepoDir)
		cmd := exec.Command("git", "clone", authenticatedURL, localRepoDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git clone failed: %w\n%s", err, out)
		}
		log.Printf("scheduler: sync: clone done for project %s", project.Slug)
	} else {
		branch := project.Branch
		if branch == "" {
			branch = "main"
		}
		log.Printf("scheduler: sync: local git reset for project %s", project.Slug)
		resetCmd := exec.Command("git", "-C", localRepoDir, "reset", "--hard", "HEAD")
		if out, err := resetCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git reset failed: %w\n%s", err, out)
		}
		log.Printf("scheduler: sync: local git pull for project %s (branch=%s)", project.Slug, branch)
		pullStart := time.Now()
		pullCmd := exec.Command("git", "-C", localRepoDir, "pull", authenticatedURL, branch)
		if out, err := pullCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git pull failed: %w\n%s", err, out)
		}
		log.Printf("scheduler: sync: pull done for project %s (%s)", project.Slug, time.Since(pullStart).Round(time.Millisecond))
	}
	return nil
}

func (c *Client) IsAlive() bool {
	_, _, code, err := c.Run("echo ok")
	return err == nil && code == 0
}

// Close ne prend pas c.mu intentionnellement.
func (c *Client) Close() error {
	log.Printf("ssh: [%s] Close", c.name)
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

func (c *Client) ReadRemoteFileBinary(remotePath string) ([]byte, error) {
	stdout, _, code, err := c.Run(fmt.Sprintf("base64 -w0 %s 2>/dev/null", shellEscape(remotePath)))
	if err != nil {
		return nil, fmt.Errorf("base64 read failed: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("file not found or not readable: %s", remotePath)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stdout))
	if err != nil {
		return nil, fmt.Errorf("base64 decode failed: %w", err)
	}
	return data, nil
}
