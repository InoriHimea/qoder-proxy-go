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
	"sync"
)

//go:embed write_token.mjs
var writeTokenScript []byte

//go:embed cli_wrapper.mjs
var cliWrapperScript []byte

// maxInlineSystemPromptBytes caps how large a --system-prompt value can be
// passed directly as a command-line argument. Linux enforces a per-argument
// limit (MAX_ARG_STRLEN, 128KiB) independent of overall ARG_MAX; a prompt
// past this size makes fork/exec fail with "argument list too long" (E2BIG)
// no matter how much memory is available. Past this threshold we route
// through cli_wrapper.mjs instead, which passes the prompt via a temp file
// and injects it into the CLI's argv in-process (dynamic import, no exec).
const maxInlineSystemPromptBytes = 100 * 1024

// deviceTokenWriteMu serializes writes to the CLI's on-disk credential file
// and lets concurrent spawns skip the rewrite when the token hasn't changed
// since the last write. Without this, concurrent requests each shell out to
// write_token.mjs and race on the same credential file — the CLI process can
// then read a half-written/corrupted file and exit 1 (observed in prod as a
// burst of request 500s under concurrent load).
var (
	deviceTokenWriteMu  sync.Mutex
	lastWrittenTokenSig string
)

func writeDeviceTokenToKeychain(token, userID, refreshToken string, expireTime int64, backendName string) error {
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

	AddSystemLog(fmt.Sprintf("Writing device token via embedded script (backend=%s, user=%s)...", backendName, userID), "info", "spawn")
	args := []string{scriptPath, backendName, token}
	if userID != "" {
		args = append(args, userID)
	}
	if refreshToken != "" {
		args = append(args, refreshToken)
	}
	if expireTime != 0 {
		args = append(args, fmt.Sprintf("%d", expireTime))
	}
	cmd := exec.Command("node", args...)
	output, err := cmd.CombinedOutput()

	os.RemoveAll(scriptDir)

	if err != nil {
		return fmt.Errorf("node exec failed (%w): %s", err, string(output))
	}
	return nil
}

// resolveCliJSPath follows the npm global bin shim (a symlink) for cmdPath
// to the actual CLI bundle .js file, which is what cli_wrapper.mjs needs to
// dynamically import in-process.
func resolveCliJSPath(cmdPath string) (string, error) {
	binPath, err := exec.LookPath(cmdPath)
	if err != nil {
		return "", fmt.Errorf("lookup %s: %w", cmdPath, err)
	}
	resolved, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		return "", fmt.Errorf("resolve symlink for %s: %w", binPath, err)
	}
	return resolved, nil
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
		sig := backendName + "|" + config.Token + "|" + config.UserID + "|" + config.RefreshToken + "|" + fmt.Sprintf("%d", config.ExpireTime)
		deviceTokenWriteMu.Lock()
		if sig == lastWrittenTokenSig {
			deviceTokenWriteMu.Unlock()
		} else {
			if err := writeDeviceTokenToKeychain(config.Token, config.UserID, config.RefreshToken, config.ExpireTime, backendName); err != nil {
				deviceTokenWriteMu.Unlock()
				AddSystemLog(fmt.Sprintf("Failed to write device token to local storage: %v", err), "error", "spawn")
			} else {
				lastWrittenTokenSig = sig
				deviceTokenWriteMu.Unlock()
				AddSystemLog("Device token successfully written to local storage", "info", "spawn")
			}
		}
	}

	args := []string{"-p", "-", "-o", "stream-json", "--dangerously-skip-permissions", "--permission-mode", "bypass_permissions"}
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

	var cmd *exec.Cmd
	var wrapperTempDir string
	if runtime.GOOS != "windows" && len(sysPrompt) > maxInlineSystemPromptBytes {
		cliJSPath, resolveErr := resolveCliJSPath(cmdPath)
		if resolveErr != nil {
			AddSystemLog(fmt.Sprintf("Cannot resolve CLI entry for large system prompt (%d bytes), passing inline anyway (may fail with E2BIG): %v", len(sysPrompt), resolveErr), "warn", "spawn")
			cmd = exec.CommandContext(ctx, cmdPath, append(args, "--system-prompt", sysPrompt)...)
		} else {
			dir, err := os.MkdirTemp("", "qoder-proxy-wrapper-*")
			if err != nil {
				return nil, fmt.Errorf("create wrapper temp dir: %w", err)
			}
			wrapperPath := filepath.Join(dir, "cli_wrapper.mjs")
			spPath := filepath.Join(dir, "system_prompt.txt")
			if err := os.WriteFile(wrapperPath, cliWrapperScript, 0600); err != nil {
				os.RemoveAll(dir)
				return nil, fmt.Errorf("write wrapper script: %w", err)
			}
			if err := os.WriteFile(spPath, []byte(sysPrompt), 0600); err != nil {
				os.RemoveAll(dir)
				return nil, fmt.Errorf("write system prompt file: %w", err)
			}
			wrapperTempDir = dir
			AddSystemLog(fmt.Sprintf("System prompt (%d bytes) exceeds inline arg limit, routing through cli_wrapper.mjs", len(sysPrompt)), "info", "spawn")
			cmd = exec.CommandContext(ctx, "node", append([]string{wrapperPath, cliJSPath, spPath}, args...)...)
		}
	} else {
		cmd = exec.CommandContext(ctx, cmdPath, append(args, "--system-prompt", sysPrompt)...)
	}
	cmd.Dir = os.TempDir() // Prevent locking into /app directory

	// Set environment
	cmd.Env = os.Environ()
	if config.Token != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("QODER_API_KEY=%s", config.Token))
	}
	cmd.Env = append(cmd.Env, "NO_BROWSER=1", "CI=1")
	cmd.Env = append(cmd.Env, "NODE_OPTIONS=--max-old-space-size=16384")
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
		if wrapperTempDir != "" {
			os.RemoveAll(wrapperTempDir)
		}
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
		if wrapperTempDir != "" {
			os.RemoveAll(wrapperTempDir)
		}
		if err != nil {
			AddSystemLog(fmt.Sprintf("CLI %s exited with error: %v", cmdName, err), "error", "process")
		} else {
			AddSystemLog(fmt.Sprintf("CLI %s exited successfully", cmdName), "info", "process")
		}
	}(binaryName)

	return stdout, nil
}
