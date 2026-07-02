package main

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed write_token.mjs
var writeTokenScript []byte

func writeDeviceTokenToKeychain(token, backendName string) error {
	if token == "" {
		return fmt.Errorf("token empty")
	}

	scriptDir, err := os.MkdirTemp("", "qoder-proxy-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	scriptPath := filepath.Join(scriptDir, "write_token.mjs")
	if err := os.WriteFile(scriptPath, writeTokenScript, 0600); err != nil {
		return fmt.Errorf("write embed script: %w", err)
	}

	AddSystemLog(fmt.Sprintf("Writing device token via embedded script (backend=%s)...", backendName), "info", "spawn")
	cmd := exec.Command("node", scriptPath, backendName, token)
	output, err := cmd.CombinedOutput()

	os.RemoveAll(scriptDir)

	if err != nil {
		return fmt.Errorf("node exec failed (%w): %s", err, string(output))
	}
	return nil
}

type SpawnOptions struct {
	Model           string
	ReasoningEffort string
	MaxTokens       int
	SystemPrompt    string
	DisableTools    bool
}

func spawnQoderCli(ctx context.Context, prompt string, opts SpawnOptions, cm *ConfigManager) (io.ReadCloser, error) {
	config := cm.Get()

	binaryName := "qodercli"
	backendName := "global"
	if strings.ToLower(config.Backend) == "cn" {
		binaryName = "qoderclicn"
		backendName = "cn"
	}

	if config.Token != "" {
		if err := writeDeviceTokenToKeychain(config.Token, backendName); err != nil {
			AddSystemLog(fmt.Sprintf("Failed to write device token to local storage: %v", err), "error", "spawn")
		} else {
			AddSystemLog("Device token successfully written to local storage", "info", "spawn")
		}
	}

	// Prepare arguments. Note: we must include `--` so qodercli knows the prompt comes from stdin.
	args := []string{"-p", "-", "-f", "stream-json", "--dangerously-skip-permissions", "--permission-mode", "bypassPermissions"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ReasoningEffort != "" {
		args = append(args, "--reasoning-effort", opts.ReasoningEffort)
	}
	if opts.MaxTokens > 0 {
		args = append(args, "--max-output-tokens", fmt.Sprintf("%d", opts.MaxTokens))
	}
	sysPrompt := opts.SystemPrompt
	if sysPrompt == "" {
		sysPrompt = "You are a helpful AI assistant." // Overrides Qoder's massive default system prompt
	}
	args = append(args, "--system-prompt", sysPrompt)
	if opts.DisableTools {
		args = append(args, "--tools", "")
	}

	cmdPath := binaryName
	if runtime.GOOS == "windows" {
		cmdPath = binaryName + ".cmd"
	}

	AddSystemLog(fmt.Sprintf("Spawning %s (model: %s)", cmdPath, opts.Model), "info", "spawn")
	if config.Token == "" {
		AddSystemLog("Warning: Personal Access Token is empty in config", "warn", "config")
	}

	cmd := exec.CommandContext(ctx, cmdPath, args...)
	cmd.Dir = os.TempDir() // Prevent locking into /app directory

	// Set environment
	cmd.Env = os.Environ()
	if config.Token != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("QODER_API_KEY=%s", config.Token))
	}
	cmd.Env = append(cmd.Env, "NO_BROWSER=1", "CI=1")
	cmd.Env = append(cmd.Env, "NODE_OPTIONS=--max-old-space-size=8192")
	// Some Node.js optimizations for large heap
	cmd.Env = append(cmd.Env, "V8_FORCE_GC=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// Asynchronously read stderr to prevent the process from blocking
	go func() {
		defer stderr.Close()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) != "" {
				// Special handling for common warnings to reduce noise
				if !strings.Contains(line, "MaxListenersExceededWarning") && !strings.Contains(line, "--trace-warnings") {
					AddSystemLog(fmt.Sprintf("CLI stderr: %s", line), "warn", "cli")
				}
			}
		}
	}()

	// Write prompt to stdin asynchronously
	go func() {
		defer stdin.Close()
		_, err := io.WriteString(stdin, prompt+"\n")
		if err != nil {
			AddSystemLog(fmt.Sprintf("Failed to write to CLI stdin: %v", err), "error", "spawn")
		}
	}()

	// Wait for process to exit to log its status
	go func(cmdName string) {
		err := cmd.Wait()
		if err != nil {
			AddSystemLog(fmt.Sprintf("CLI %s exited with error: %v", cmdName, err), "error", "process")
		} else {
			AddSystemLog(fmt.Sprintf("CLI %s exited successfully", cmdName), "info", "process")
		}
	}(binaryName)

	return stdout, nil
}
