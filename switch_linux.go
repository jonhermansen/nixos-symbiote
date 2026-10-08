//go:build linux

package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const traceDir = "/var/log/symbiote"

var traceFile *os.File
var traceStart = time.Now()

func switchSystem(nixBin, system string) error {
	ensureProtectedScope()
	initTrace()
	defer closeTrace()

	trace("phase", "start pid=%d ppid=%d uid=%d", os.Getpid(), os.Getppid(), os.Getuid())

	protected := protectedUnits()
	trace("protect", "units=%v", protected)

	newRoot := constructRoot(system)
	trace("construct", "newRoot=%s", newRoot)

	activate(nixBin, system, newRoot)
	trace("activate", "done")

	protect(protected)
	trace("protect", "drop-ins written")

	diagnose(newRoot, protected)
	trace("phase", "diagnose complete — triggering soft-reboot")

	return softReboot(newRoot)
}

// --- tracing ---

func initTrace() {
	os.MkdirAll(traceDir, 0755)
	f, err := os.OpenFile(filepath.Join(traceDir, "trace.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("trace file open failed: %v", err)
		return
	}
	traceFile = f
	trace("init", "trace file opened")
}

func closeTrace() {
	if traceFile != nil {
		traceFile.Sync()
		traceFile.Close()
	}
}

func trace(phase, format string, args ...interface{}) {
	elapsed := time.Since(traceStart).Seconds()
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("SYMBIOTE [%.3fs] %-10s %s", elapsed, phase, msg)
	fmt.Fprintln(os.Stdout, line)
	os.Stdout.Sync()
	if traceFile != nil {
		fmt.Fprintln(traceFile, line)
		traceFile.Sync()
	}
}

func traceBlock(phase, title string, body func() string) {
	trace(phase, "=== BEGIN %s ===", title)
	for _, line := range strings.Split(strings.TrimRight(body(), "\n"), "\n") {
		trace(phase, "  %s", line)
	}
	trace(phase, "=== END %s ===", title)
}

func readAll(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("<read error: %v>", err)
	}
	return string(data)
}

func runCapture(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%s\n<exec error: %v>", string(out), err)
	}
	return string(out)
}

// --- pre-reboot diagnostics ---

func diagnose(newRoot string, protected []string) {
	trace("diag", "collecting pre-reboot state")

	traceBlock("diag", "self /proc/self/status", func() string {
		return readAll("/proc/self/status")
	})
	traceBlock("diag", "self /proc/self/cgroup", func() string {
		return readAll("/proc/self/cgroup")
	})

	traceBlock("diag", "runner processes", func() string {
		return dumpRunnerProcesses()
	})

	traceBlock("diag", "systemctl status of protected units", func() string {
		var sb strings.Builder
		for _, u := range protected {
			sb.WriteString(fmt.Sprintf("--- %s ---\n", u))
			sb.WriteString(runCapture("systemctl", "show", u,
				"--property=Id,ActiveState,SubState,DefaultDependencies,IgnoreOnIsolate,SurviveFinalKillSignal,FragmentPath"))
		}
		return sb.String()
	})

	traceBlock("diag", "mountinfo", func() string {
		return readAll("/proc/self/mountinfo")
	})

	traceBlock("diag", "nextroot layout", func() string {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("newRoot=%s\n", newRoot))
		sb.WriteString(fmt.Sprintf("/run/nextroot -> %s\n", resolveSymlink("/run/nextroot")))
		for _, p := range []string{
			"sbin/init", "usr/lib/systemd/systemd", "usr/lib/systemd/systemd-executor",
			"etc/os-release", "etc/machine-id", "nix", "home",
		} {
			full := filepath.Join(newRoot, p)
			if fi, err := os.Lstat(full); err == nil {
				tgt := ""
				if fi.Mode()&os.ModeSymlink != 0 {
					tgt, _ = os.Readlink(full)
					tgt = " -> " + tgt
				}
				sb.WriteString(fmt.Sprintf("  %s: %s (%s)%s\n", p, fi.Mode(), humanSize(fi.Size()), tgt))
			} else {
				sb.WriteString(fmt.Sprintf("  %s: MISSING (%v)\n", p, err))
			}
		}
		return sb.String()
	})

	traceBlock("diag", "systemctl is-system-running (pre-reboot)", func() string {
		return runCapture("systemctl", "is-system-running")
	})
	traceBlock("diag", "systemctl --failed (pre-reboot)", func() string {
		return runCapture("systemctl", "--failed", "--no-pager")
	})
	traceBlock("diag", "systemd-analyze blame (top 20)", func() string {
		return runCapture("sh", "-c", "systemd-analyze blame --no-pager 2>&1 | head -20")
	})
	traceBlock("diag", "systemd version", func() string {
		return runCapture("systemctl", "--version")
	})
	traceBlock("diag", "kernel", func() string {
		return runCapture("uname", "-a")
	})
	traceBlock("diag", "soft-reboot availability", func() string {
		return runCapture("systemctl", "list-unit-files", "systemd-soft-reboot.service")
	})
}

func dumpRunnerProcesses() string {
	var sb strings.Builder
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		cmdline := readAll(filepath.Join("/proc", e.Name(), "cmdline"))
		cmdline = strings.ReplaceAll(cmdline, "\x00", " ")
		lower := strings.ToLower(cmdline)
		if !strings.Contains(lower, "runner") && !strings.Contains(lower, "actions") {
			continue
		}
		stat := readAll(filepath.Join("/proc", e.Name(), "stat"))
		ppid := 0
		if i := strings.LastIndex(stat, ")"); i >= 0 {
			fields := strings.Fields(stat[i+1:])
			if len(fields) >= 2 {
				ppid, _ = strconv.Atoi(fields[1])
			}
		}
		cgroup := strings.TrimSpace(readAll(filepath.Join("/proc", e.Name(), "cgroup")))
		sb.WriteString(fmt.Sprintf("pid=%s ppid=%d\n  cmdline: %s\n  cgroup: %s\n",
			e.Name(), ppid, strings.TrimSpace(cmdline), cgroup))
	}
	if sb.Len() == 0 {
		return "<no runner-like processes found>"
	}
	return sb.String()
}

func resolveSymlink(path string) string {
	tgt, err := os.Readlink(path)
	if err != nil {
		return fmt.Sprintf("<readlink error: %v>", err)
	}
	return tgt
}

func humanSize(n int64) string {
	switch {
	case n > 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	case n > 1<<10:
		return fmt.Sprintf("%dK", n>>10)
	}
	return fmt.Sprintf("%dB", n)
}

// --- scope protection ---

func ensureProtectedScope() {
	if os.Getenv("NIXOS_SYMBIOTE_PROTECTED") == "1" {
		return
	}
	sdrun, err := exec.LookPath("systemd-run")
	if err != nil {
		log.Printf("systemd-run not found, skipping scope protection")
		return
	}
	log.Printf("re-execing inside protected systemd scope")
	args := append([]string{
		"systemd-run", "--scope",
		"--property=DefaultDependencies=no",
		"--property=IgnoreOnIsolate=yes",
		"--property=SurviveFinalKillSignal=yes",
		"--setenv=NIXOS_SYMBIOTE_PROTECTED=1",
	}, os.Args...)
	syscall.Exec(sdrun, args, os.Environ())
}

// --- cgroup walk ---

var unitRe = regexp.MustCompile(`([^/]+\.(service|scope|slice))\s*$`)

func protectedUnits() []string {
	seen := map[string]bool{}
	var units []string

	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			units = append(units, u)
		}
	}

	// walk our own ancestor chain
	pid := os.Getpid()
	for pid > 1 {
		for _, u := range unitsForPid(pid) {
			add(u)
		}
		pid = ppidOf(pid)
	}

	// GH runner processes: Runner.Listener, Runner.Worker — these are our siblings,
	// not ancestors. If we don't protect them, soft-reboot kills them and the step
	// shell dies in the middle of its "wait for NixOS" poll.
	for _, u := range runnerUnits() {
		add(u)
	}

	for _, u := range []string{"nix-daemon.service", "walinuxagent.service"} {
		add(u)
	}
	return units
}

func unitsForPid(pid int) []string {
	var out []string
	f, err := os.Open(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return out
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if m := unitRe.FindStringSubmatch(s.Text()); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

func runnerUnits() []string {
	var out []string
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmd := readAll(filepath.Join("/proc", e.Name(), "cmdline"))
		if !strings.Contains(cmd, "Runner.Listener") && !strings.Contains(cmd, "Runner.Worker") {
			continue
		}
		units := unitsForPid(pid)
		trace("runner", "pid=%d units=%v cmd=%s", pid, units, strings.ReplaceAll(cmd, "\x00", " "))
		out = append(out, units...)
	}
	return out
}

func ppidOf(pid int) int {
	data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

// --- construct root ---

func constructRoot(system string) string {
	newRoot := filepath.Join("/nixos-roots", time.Now().Format("20060102-150405"))

	for _, d := range []string{
		"etc", "var", "var/lib", "var/log", "tmp", "root",
		"usr/lib/systemd", "bin", "sbin", "nix",
		"run", "dev", "proc", "sys", "home",
		"lib/modules", "lib/firmware",
	} {
		os.MkdirAll(filepath.Join(newRoot, d), 0755)
	}

	// Bind-mount our trace dir into the new root so post-reboot logs land there too.
	os.MkdirAll(filepath.Join(newRoot, "var/log/symbiote"), 0755)
	syscall.Mount(traceDir, filepath.Join(newRoot, "var/log/symbiote"), "", syscall.MS_BIND, "")

	for _, b := range [][2]string{
		{"/nix", "nix"},
		{"/home", "home"},
		{"/lib/modules", "lib/modules"},
	} {
		syscall.Mount(b[0], filepath.Join(newRoot, b[1]), "", syscall.MS_BIND, "")
	}
	if _, err := os.Stat("/lib/firmware"); err == nil {
		syscall.Mount("/lib/firmware", filepath.Join(newRoot, "lib/firmware"), "", syscall.MS_BIND, "")
	}

	for _, d := range []string{"dev", "proc", "sys"} {
		syscall.Mount("/"+d, filepath.Join(newRoot, d), "", syscall.MS_BIND, "")
	}
	syscall.Mount("tmpfs", filepath.Join(newRoot, "run"), "tmpfs", 0, "")

	systemdLib := filepath.Join(system, "systemd/lib/systemd")
	os.Symlink(filepath.Join(systemdLib, "systemd"), filepath.Join(newRoot, "usr/lib/systemd/systemd"))
	os.Symlink(filepath.Join(systemdLib, "systemd-executor"), filepath.Join(newRoot, "usr/lib/systemd/systemd-executor"))
	os.Symlink(filepath.Join(systemdLib, "systemd"), filepath.Join(newRoot, "sbin/init"))

	return newRoot
}

// --- activate ---

func activate(nixBin, system, newRoot string) {
	os.MkdirAll("/nix/var/nix/profiles", 0755)
	os.Remove("/nix/var/nix/profiles/system")
	os.Symlink(system, "/nix/var/nix/profiles/system")

	os.Symlink("/nix/var/nix/profiles/system", filepath.Join(newRoot, "run/current-system"))

	cmd := exec.Command("chroot", newRoot, "/nix/var/nix/profiles/system/activate")
	cmd.Env = []string{"NIXOS_ACTION=boot", "NIXOS_INSTALL_BOOTLOADER=0", "PATH=/nix/var/nix/profiles/system/sw/bin"}
	cmd.Stdout = io.MultiWriter(os.Stdout, prefixWriter{"activate"})
	cmd.Stderr = io.MultiWriter(os.Stderr, prefixWriter{"activate!"})
	cmd.Run()

	os.Remove(filepath.Join(newRoot, "etc/os-release"))
	os.WriteFile(filepath.Join(newRoot, "etc/os-release"), []byte("ID=nixos\n"), 0644)

	for _, d := range []string{"run", "sys", "proc", "dev"} {
		syscall.Unmount(filepath.Join(newRoot, d), syscall.MNT_DETACH)
	}
}

type prefixWriter struct{ phase string }

func (p prefixWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		trace(p.phase, "  %s", line)
	}
	return len(b), nil
}

// --- protect units ---

func protect(units []string) {
	dropin := []byte("[Unit]\nDefaultDependencies=no\nIgnoreOnIsolate=yes\nSurviveFinalKillSignal=yes\n")
	for _, unit := range units {
		dir := filepath.Join("/run/systemd/system", unit+".d")
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, "survive.conf"), dropin, 0644)
		trace("protect", "wrote drop-in for %s", unit)
	}
	mustRun(exec.Command("systemctl", "daemon-reload"))
}

// --- soft-reboot ---

func softReboot(newRoot string) error {
	syscall.Mount(newRoot, newRoot, "", syscall.MS_BIND, "")
	os.Remove("/run/nextroot")
	os.Symlink(newRoot, "/run/nextroot")

	trace("reboot", "triggering soft-reboot into %s", newRoot)
	cmd := exec.Command("systemctl", "soft-reboot")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	trace("reboot", "systemctl soft-reboot returned: %v", err)
	return err
}

// --- mountinfo ---

func mountSource(path string) (string, string) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 10 {
			continue
		}
		if fields[4] != path {
			continue
		}
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" && i+2 < len(fields) {
				return fields[i+2], fields[i+1]
			}
		}
	}
	return "", ""
}
