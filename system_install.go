//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	systemInstallTargetBinary = "/usr/local/sbin/spinherd"
	systemInstallServicePath  = "/etc/systemd/system/spinherd.service"
)

func runSystemInstall(ctx context.Context, logger *log.Logger, _ SystemInstallConfig) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}

	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolve current executable symlinks: %w", err)
	}

	targetPath := systemInstallTargetBinary
	if execPath != targetPath {
		logger.Printf("installing binary from %s to %s", execPath, targetPath)
		if err := copyExecutable(execPath, targetPath); err != nil {
			return err
		}
	} else {
		logger.Printf("binary already running from %s, skipping copy", targetPath)
	}

	serviceData := []byte(systemdServiceText(targetPath))
	logger.Printf("writing systemd unit to %s", systemInstallServicePath)
	if err := writeFileAtomic(systemInstallServicePath, serviceData, 0o644); err != nil {
		return err
	}

	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", "spinherd.service"},
		{"restart", "spinherd.service"},
	} {
		logger.Printf("running systemctl %s", joinArgs(args))
		if err := runSystemctl(ctx, args...); err != nil {
			return err
		}
	}

	logger.Printf("system install completed")
	return nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source executable %s: %w", src, err)
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create target directory %s: %w", filepath.Dir(dst), err)
	}

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat source executable %s: %w", src, err)
	}

	tmpPath := dst + ".tmp"
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create target executable %s: %w", tmpPath, err)
	}

	copyErr := func() error {
		if _, err := io.Copy(out, in); err != nil {
			return fmt.Errorf("copy executable to %s: %w", tmpPath, err)
		}
		if err := out.Chmod(info.Mode().Perm() | 0o111); err != nil {
			return fmt.Errorf("chmod target executable %s: %w", tmpPath, err)
		}
		if err := out.Sync(); err != nil {
			return fmt.Errorf("sync target executable %s: %w", tmpPath, err)
		}
		return nil
	}()
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close target executable %s: %w", tmpPath, closeErr)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace target executable %s: %w", dst, err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create target directory %s: %w", filepath.Dir(path), err)
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func runSystemctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %s: %w", joinArgs(args), err)
	}
	return nil
}

func joinArgs(args []string) string {
	return strings.Join(args, " ")
}

func systemdServiceText(binaryPath string) string {
	return fmt.Sprintf(`[Unit]
Description=spinherd storage spindown daemon
After=local-fs.target

[Service]
Type=simple
ExecStart=%s daemon
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
`, binaryPath)
}
