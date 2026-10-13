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

	trace("start", "pid=%d ppid=%d uid=%d", os.Getpid(), os.Getppid(), os.Getuid())

	// Create current-system profile symlink BEFORE constructRoot so NixOS
	// activate can resolve /run/current-system → /nix/var/nix/profiles/system.
	os.MkdirAll("/nix/var/nix/profiles", 0755)
	os.Remove("/nix/var/nix/profiles/system")
	if err := os.Symlink(system, "/nix/var/nix/profiles/system"); err != nil {
		log.Fatalf("profile symlink: %v", err)
	}

	protected := protectedUnits()
	trace("protect", "units=%v", protected)

	newRoot := constructRoot(system)
	trace("construct", "newRoot=%s", newRoot)

	activate(nixBin, system, newRoot)
	trace("activate", "done")

	protect(protected)
	// Migrate the GH runner (Listener + Worker) into our protected scope.
	// This is more robust than drop-ins alone — the kernel cgroup is what
	// soft-reboot's isolate uses, not systemd's bookkeeping. See migrateRunner.
	migrateRunnerToOurScope()
	return softReboot(newRoot)
}

// migrateRunnerToOurScope writes our ancestor chain + GH runner PIDs into
// our protected scope's cgroup.procs. The kernel moves them into our cgroup;
// systemd's bookkeeping may be inconsistent afterwards, but the kernel-level
// cgroup is what soft-reboot's isolate uses — our survival properties now
// apply to the migrated PIDs. After this: our step shell, Runner.Worker,
// Runner.Listener all survive pivot_root. Shell can finish normally,
// Worker reports step completion, Listener's reconnect handles the brief
// network blip during pivot.
func migrateRunnerToOurScope() {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		trace("migrate", "read own cgroup: %v (skipping)", err)
		return
	}
	line := strings.TrimSpace(string(data))
	parts := strings.SplitN(line, "::", 2)
	if len(parts) < 2 {
		trace("migrate", "unparseable cgroup line: %q (skipping)", line)
		return
	}
	procsFile := filepath.Join("/sys/fs/cgroup", parts[1], "cgroup.procs")

	moved := map[int]bool{os.Getpid(): true}
	migrate := func(pid int, why string) {
		if moved[pid] || pid < 2 {
			return
		}
		if err := os.WriteFile(procsFile, []byte(strconv.Itoa(pid)+"\n"), 0644); err != nil {
			trace("migrate", "pid=%d (%s) FAILED: %v", pid, why, err)
			return
		}
		moved[pid] = true
		cmd, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		trace("migrate", "pid=%d (%s, comm=%s) → %s", pid, why,
			strings.TrimSpace(string(cmd)), procsFile)
	}

	// Walk our ancestor chain — shell, sudo, Worker, Listener, their parents.
	pid := os.Getppid()
	for pid > 1 {
		migrate(pid, "ancestor")
		pid = ppidOf(pid)
	}

	// Any Runner.* processes not already migrated (siblings of our chain).
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		s := string(cmd)
		if strings.Contains(s, "Runner.Listener") || strings.Contains(s, "Runner.Worker") {
			migrate(pid, "runner")
		}
	}
}

// --- tracing ---

func initTrace() {
	os.MkdirAll(traceDir, 0755)
	f, err := os.OpenFile(filepath.Join(traceDir, "trace.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		traceFile = f
	}
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

// --- scope protection ---

func ensureProtectedScope() {
	if os.Getenv("NIXOS_SYMBIOTE_PROTECTED") == "1" {
		return
	}
	sdrun, err := exec.LookPath("systemd-run")
	if err != nil {
		return
	}
	args := append([]string{
		"systemd-run", "--scope",
		"--property=DefaultDependencies=no",
		"--property=IgnoreOnIsolate=yes",
		"--property=SurviveFinalKillSignal=yes",
		"--setenv=NIXOS_SYMBIOTE_PROTECTED=1",
	}, os.Args...)
	syscall.Exec(sdrun, args, os.Environ())
}

// --- cgroup walk: identify units in our ancestor chain + Runner.* siblings ---

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

	// Our own ancestor cgroup chain.
	pid := os.Getpid()
	for pid > 1 {
		for _, u := range unitsForPid(pid) {
			add(u)
		}
		pid = ppidOf(pid)
	}

	// GitHub Actions runner: Runner.Listener and Runner.Worker are siblings,
	// not ancestors. Without this, soft-reboot kills them and the GH job's
	// post-pivot steps have no process to execute them.
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
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		s := string(cmd)
		if !strings.Contains(s, "Runner.Listener") && !strings.Contains(s, "Runner.Worker") {
			continue
		}
		out = append(out, unitsForPid(pid)...)
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

// --- construct the new root ---
//
// Key design: newRoot is its OWN tmpfs mount, not a directory on the host
// rootfs. This gives it independent mount-tree identity. Without this,
// post-pivot systemd fails to spawn any service with "Failed to spawn
// executor: No such file or directory" — mount propagation from the dying
// old root invalidates /nix in the pivoted namespace.
//
// /nix is bind-mounted with MS_PRIVATE so pivot_root cleanup doesn't
// propagate to it.

func constructRoot(system string) string {
	newRoot := filepath.Join("/nixos-roots", time.Now().Format("20060102-150405"))
	os.MkdirAll(newRoot, 0755)

	if err := syscall.Mount("tmpfs", newRoot, "tmpfs", 0, "size=2G,mode=755"); err != nil {
		log.Fatalf("mount tmpfs on %s: %v", newRoot, err)
	}

	for _, d := range []string{
		"etc", "var", "var/lib", "var/log", "tmp", "root",
		"usr/lib/systemd", "bin", "sbin", "nix",
		"run", "dev", "proc", "sys", "home",
		"lib/modules", "lib/firmware",
	} {
		os.MkdirAll(filepath.Join(newRoot, d), 0755)
	}

	// /nix: bind-mount with recursive private propagation.
	if err := syscall.Mount("/nix", filepath.Join(newRoot, "nix"), "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		log.Fatalf("bind-mount /nix: %v", err)
	}
	syscall.Mount("", filepath.Join(newRoot, "nix"), "", syscall.MS_PRIVATE|syscall.MS_REC, "")

	for _, b := range [][2]string{
		{"/home", "home"},
		{"/lib/modules", "lib/modules"},
	} {
		syscall.Mount(b[0], filepath.Join(newRoot, b[1]), "", syscall.MS_BIND, "")
		syscall.Mount("", filepath.Join(newRoot, b[1]), "", syscall.MS_PRIVATE, "")
	}
	if _, err := os.Stat("/lib/firmware"); err == nil {
		syscall.Mount("/lib/firmware", filepath.Join(newRoot, "lib/firmware"), "", syscall.MS_BIND, "")
		syscall.Mount("", filepath.Join(newRoot, "lib/firmware"), "", syscall.MS_PRIVATE, "")
	}

	for _, d := range []string{"dev", "proc", "sys"} {
		syscall.Mount("/"+d, filepath.Join(newRoot, d), "", syscall.MS_BIND, "")
	}
	syscall.Mount("tmpfs", filepath.Join(newRoot, "run"), "tmpfs", 0, "")

	// Symlinks for systemd + systemd-executor (point into bind-mounted /nix).
	systemdLib := filepath.Join(system, "systemd/lib/systemd")
	realSystemd, err := filepath.EvalSymlinks(filepath.Join(systemdLib, "systemd"))
	if err != nil {
		log.Fatalf("resolve systemd binary: %v", err)
	}
	realExecutor, err := filepath.EvalSymlinks(filepath.Join(systemdLib, "systemd-executor"))
	if err != nil {
		log.Fatalf("resolve systemd-executor: %v", err)
	}
	for _, l := range [][2]string{
		{realSystemd, filepath.Join(newRoot, "usr/lib/systemd/systemd")},
		{realExecutor, filepath.Join(newRoot, "usr/lib/systemd/systemd-executor")},
		{realSystemd, filepath.Join(newRoot, "sbin/init")},
	} {
		if err := os.Symlink(l[0], l[1]); err != nil {
			log.Fatalf("symlink %s -> %s: %v", l[0], l[1], err)
		}
	}

	return newRoot
}

// --- NixOS activation ---

func activate(nixBin, system, newRoot string) {
	os.Symlink("/nix/var/nix/profiles/system", filepath.Join(newRoot, "run/current-system"))

	cmd := exec.Command("chroot", newRoot, "/nix/var/nix/profiles/system/activate")
	cmd.Env = []string{
		"NIXOS_ACTION=boot",
		"NIXOS_INSTALL_BOOTLOADER=0",
		"PATH=/nix/var/nix/profiles/system/sw/bin",
	}
	cmd.Stdout = io.MultiWriter(os.Stdout, prefixWriter{"activate"})
	cmd.Stderr = io.MultiWriter(os.Stderr, prefixWriter{"activate!"})
	if err := cmd.Run(); err != nil {
		log.Fatalf("chroot activate failed: %v", err)
	}

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
	}
	exec.Command("systemctl", "daemon-reload").Run()
}

// --- soft-reboot ---
//
// Trigger soft-reboot then BLOCK until the system is actually usable.
// systemctl soft-reboot is async — it queues the operation and returns
// immediately. If we exit before the new systemd is ready, the next
// step's shell tries to spawn mid-pivot or against a half-booted system
// and hangs.
//
// Two phases:
//   1. Wait for /proc/1/exe to resolve into /nix/store/ — PID 1 has been
//      re-exec'd as NixOS systemd (pivot *started*).
//   2. Wait for systemctl is-system-running to return "running" or
//      "degraded" — NixOS systemd has reached a stable boot state (pivot
//      *complete*, system is answering requests, services spawnable).
//
// Each systemctl call wrapped in timeout so an unresponsive systemd
// doesn't block the loop — we retry. The outer caller bounds total time.

func softReboot(newRoot string) error {
	syscall.Mount(newRoot, newRoot, "", syscall.MS_BIND, "")
	os.Remove("/run/nextroot")
	os.Symlink(newRoot, "/run/nextroot")

	trace("reboot", "triggering soft-reboot")
	if err := exec.Command("systemctl", "soft-reboot").Run(); err != nil {
		return fmt.Errorf("systemctl soft-reboot: %w", err)
	}

	// Phase 1: wait for PID 1 to be re-exec'd into NixOS systemd.
	trace("reboot", "phase 1: waiting for /proc/1/exe inside /nix/store/")
	for i := 0; ; i++ {
		time.Sleep(500 * time.Millisecond)
		exe, _ := os.Readlink("/proc/1/exe")
		if strings.HasPrefix(exe, "/nix/store/") {
			trace("reboot", "phase 1 done after %d polls: /proc/1/exe=%s", i, exe)
			break
		}
		if i%20 == 0 {
			trace("reboot", "phase 1 poll #%d: /proc/1/exe=%s", i, exe)
		}
	}

	// Phase 2: wait for systemd to report running. "degraded" means at least
	// one unit failed — don't paper over it; if the user's NixOS config has
	// a service that can't start, surface that as a failure.
	trace("reboot", "phase 2: waiting for systemctl is-system-running = running")
	for i := 0; ; i++ {
		time.Sleep(500 * time.Millisecond)
		out, _ := exec.Command("timeout", "5", "systemctl", "is-system-running").CombinedOutput()
		state := strings.TrimSpace(string(out))
		if state == "running" {
			trace("reboot", "phase 2 done after %d polls: state=running", i)
			return nil
		}
		if state == "degraded" {
			failed, _ := exec.Command("timeout", "5", "systemctl", "--failed", "--no-pager").CombinedOutput()
			trace("reboot", "state=degraded — failed units follow:\n%s", string(failed))
			return fmt.Errorf("pivoted system is degraded (one or more units failed to start)")
		}
		if i%10 == 0 {
			trace("reboot", "phase 2 poll #%d: state=%q", i, state)
		}
	}
}

// --- mountinfo helper (used by main.go) ---

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
