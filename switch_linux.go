//go:build linux

package main

import (
	"bufio"
	"context"
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
	return softReboot(newRoot, system)
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

	// systemd's switch_root() iterates a transfer_table (src/shared/switch-root.c)
	// and for each entry calls chase(path, new_root, CHASE_PREFIX_ROOT) WITHOUT
	// CHASE_NONEXISTENT — meaning every path the host has MUST already exist
	// inside new_root or chase returns -ENOENT and switch_root fails. On
	// failure, systemd silently falls back to re-execing itself at the OLD
	// binary path (main.c: "Failed to switch root, trying to continue") and
	// the pivot never happens — /proc/1/exe stays unchanged.
	//
	// For soft-reboot (SWITCH_ROOT_RECURSIVE_RUN not set), transfer_table is:
	//   /dev, /sys, /proc, /run,
	//   /run/credentials/@system, /run/credentials/@encrypted, /run/host.
	//
	// GHA runners inject per-job credentials into /run/credentials/@system;
	// bare Ubuntu usually has none. That's why pivot works locally but hangs
	// on GHA. Pre-create empty stubs so chase() succeeds; systemd will then
	// bind-mount the host paths onto them non-destructively.
	for _, p := range []string{
		"run/credentials/@system",
		"run/credentials/@encrypted",
		"run/host",
	} {
		os.MkdirAll(filepath.Join(newRoot, p), 0755)
	}

	// Preserve the GitHub Actions runner's native runtime across the pivot.
	// The runner (Runner.Listener/Worker) is a .NET app plus a bundled node,
	// dynamically linked against the host's glibc/ICU/OpenSSL at Ubuntu FHS
	// paths. Its processes survive the pivot (open fds keep already-loaded
	// code mapped), but any fresh dlopen/execve resolves against the NEW
	// root — where NixOS leaves these paths empty — so the agent breaks the
	// moment it needs a not-yet-loaded library or helper binary, and the job
	// can never report back. Bind the Ubuntu dynamic linker + library dirs
	// into newRoot at identical paths. NixOS keeps everything in /nix/store
	// and does not populate these FHS dirs, so there is no collision; the
	// agent finds its libs while the user's NixOS userland is untouched.
	// (The runner install tree itself lives under /home, already bound.)
	for _, p := range []string{
		"lib64",                      // x86_64 ld-linux-x86-64.so.2
		"lib/x86_64-linux-gnu",       // x86_64 shared libs
		"usr/lib/x86_64-linux-gnu",   // x86_64 shared libs
		"lib/aarch64-linux-gnu",      // aarch64 shared libs + ld-linux-aarch64
		"usr/lib/aarch64-linux-gnu",  // aarch64 shared libs
	} {
		hostPath := "/" + p
		if _, err := os.Stat(hostPath); err != nil {
			continue // not present on this arch/host
		}
		dst := filepath.Join(newRoot, p)
		os.MkdirAll(dst, 0755)
		if err := syscall.Mount(hostPath, dst, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			trace("construct", "agent-lib bind %s FAILED: %v", hostPath, err)
			continue
		}
		syscall.Mount("", dst, "", syscall.MS_PRIVATE, "")
		trace("construct", "agent-lib preserved: %s", hostPath)
	}

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

func softReboot(newRoot, system string) error {
	syscall.Mount(newRoot, newRoot, "", syscall.MS_BIND, "")
	os.Remove("/run/nextroot")
	os.Symlink(newRoot, "/run/nextroot")

	// Confirm which switch_root transfer_table paths exist on the host —
	// these are the ones that must be present inside newRoot to prevent the
	// silent pivot-failure described in constructRoot's long comment.
	for _, p := range []string{
		"/run/credentials/@system",
		"/run/credentials/@encrypted",
		"/run/host",
	} {
		if _, err := os.Lstat(p); err == nil {
			trace("reboot", "host has %s (switch_root will transfer it; newRoot stub required)", p)
		} else {
			trace("reboot", "host lacks %s (switch_root will skip)", p)
		}
	}

	// Pre-pivot sanity dump — if switch_root fails, we need to know everything
	// that could have caused it. systemd's path_is_os_tree("/run/nextroot")
	// is the first gate (main.c:1829); it requires {/etc,/usr/lib}/os-release
	// inside newRoot. Dump that check explicitly so a missing os-release
	// shows up in trace immediately instead of needing a journal grep.
	dumpPrePivot(newRoot)

	trace("reboot", "triggering soft-reboot")
	if err := exec.Command("systemctl", "soft-reboot").Run(); err != nil {
		return fmt.Errorf("systemctl soft-reboot: %w", err)
	}

	// Phase 1: wait for PID 1 to be re-exec'd into NixOS systemd.
	// /proc/1/exe will become a path like
	//   /nix/store/<hash>-systemd-<ver>/lib/systemd/systemd
	// which gives us the EXACT systemd package running as init. We use its
	// matching bin/systemctl in phase 2 — this is the only correct choice:
	// any other systemctl in the store might speak a different protocol
	// version to the running systemd.
	//
	// Hard cap at 180 polls (90s). If pivot hasn't happened by then it never
	// will — fail fast with the journal grep so we have the data in trace.
	// systemd's "Failed to switch root" / "/run/nextroot is not a valid OS
	// tree" errors go to the journal via its own log facility, which our
	// journal-logger watchdog captures to /var/log/symbiote/journal.log.
	trace("reboot", "phase 1: waiting for /proc/1/exe inside /nix/store/")
	var pid1Exe string
	const phase1Deadline = 180
	for i := 0; ; i++ {
		time.Sleep(500 * time.Millisecond)
		exe, _ := os.Readlink("/proc/1/exe")
		if strings.HasPrefix(exe, "/nix/store/") {
			trace("reboot", "phase 1 done after %d polls: /proc/1/exe=%s", i, exe)
			pid1Exe = exe
			break
		}
		if i%20 == 0 {
			trace("reboot", "phase 1 poll #%d: /proc/1/exe=%s", i, exe)
			dumpJournalGrep(i)
		}
		if i >= phase1Deadline {
			dumpJournalGrep(i)
			return fmt.Errorf("phase 1 timed out after %d polls — PID 1 never pivoted into /nix/store (/proc/1/exe=%s); check trace above for systemd's switch_root error", i, exe)
		}
	}

	// Derive the matching systemctl from the systemd package root. systemd's
	// store layout is pkg/{bin/systemctl, lib/systemd/systemd, ...} — strip
	// the "/lib/systemd/systemd" tail to recover pkg, then append bin/systemctl.
	systemdPkg := strings.TrimSuffix(pid1Exe, "/lib/systemd/systemd")
	if systemdPkg == pid1Exe {
		return fmt.Errorf("unrecognized /proc/1/exe layout: %s (expected .../lib/systemd/systemd)", pid1Exe)
	}
	systemctl := filepath.Join(systemdPkg, "bin/systemctl")
	if _, err := os.Stat(systemctl); err != nil {
		return fmt.Errorf("expected systemctl at %s: %w", systemctl, err)
	}
	trace("reboot", "using systemctl matched to running init: %s", systemctl)

	// Phase 2: wait for systemd to report running. Use context.WithTimeout
	// (no external `timeout` binary dependency) so each poll bounds itself.
	// "degraded" means at least one unit failed — don't paper over it on
	// first sighting, but DO give one reset-failed retry first: services
	// whose OLD-system PID was SIGKILL'd by soft-reboot's final kill
	// phase carry across pivot as "failed (signal)" via systemd's
	// in-memory serialized unit state, even though they're socket-
	// activated or Restart=on-failure. reset-failed clears those stale
	// failures so dbus.socket (etc.) can spawn a fresh instance on the
	// next client connection, and is-system-running then returns running.
	trace("reboot", "phase 2: waiting for systemctl is-system-running = running")
	triedResetFailed := false
	for i := 0; ; i++ {
		time.Sleep(500 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, systemctl, "is-system-running")
		out, err := cmd.CombinedOutput()
		cancel()
		state := strings.TrimSpace(string(out))
		exit := cmd.ProcessState.ExitCode()
		if state == "running" {
			trace("reboot", "phase 2 done after %d polls: state=running", i)
			return nil
		}
		if state == "degraded" && !triedResetFailed {
			triedResetFailed = true
			ctxR, cancelR := context.WithTimeout(context.Background(), 10*time.Second)
			rfOut, rfErr := exec.CommandContext(ctxR, systemctl, "reset-failed").CombinedOutput()
			cancelR()
			trace("reboot", "degraded at poll #%d — ran reset-failed (err=%v out=%q); will retry", i, rfErr, strings.TrimSpace(string(rfOut)))
			continue
		}
		if state == "degraded" {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			failed, _ := exec.CommandContext(ctx2, systemctl, "list-units", "--failed", "--no-legend", "--no-pager", "--plain").CombinedOutput()
			cancel2()
			trace("reboot", "state=degraded — failed units follow:\n%s", string(failed))
			// For each failed unit, dump status + last 50 journal lines so we
			// don't have to re-run just to see the error.
			for _, line := range strings.Split(string(failed), "\n") {
				unit := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(strings.TrimSpace(line), "●"), " ", 2)[0])
				if unit == "" || !strings.Contains(unit, ".") {
					continue
				}
				ctx3, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
				status, _ := exec.CommandContext(ctx3, systemctl, "status", unit, "--no-pager", "-l", "-n", "50").CombinedOutput()
				cancel3()
				trace("reboot", "status %s:\n%s", unit, string(status))
				journalctl := filepath.Join(systemdPkg, "bin/journalctl")
				if _, err := os.Stat(journalctl); err == nil {
					ctx4, cancel4 := context.WithTimeout(context.Background(), 5*time.Second)
					jlog, _ := exec.CommandContext(ctx4, journalctl, "-u", unit, "--no-pager", "-n", "50", "-b").CombinedOutput()
					cancel4()
					trace("reboot", "journal %s:\n%s", unit, string(jlog))
				}
			}
			return fmt.Errorf("pivoted system is degraded (one or more units failed to start)")
		}
		if i%10 == 0 {
			errStr := ""
			if err != nil {
				errStr = err.Error()
			}
			trace("reboot", "phase 2 poll #%d: state=%q exit=%d err=%q", i, state, exit, errStr)
		}
		if i > 0 && i%60 == 0 {
			dumpStuckState(i)
		}
	}
}

// dumpPrePivot writes everything we might need to understand a pivot
// failure, taken from the HOST view just before soft-reboot fires. The
// gates systemd will apply (main.c:1829 path_is_os_tree + switch_root
// transfer_table) are all directly observable here — if a required path
// is missing from newRoot now, systemd WILL fail silently later.
func dumpPrePivot(newRoot string) {
	trace("prepivot", "===== pre-soft-reboot sanity dump =====")
	nxLink, _ := os.Readlink("/run/nextroot")
	trace("prepivot", "/run/nextroot → %s (want: %s)", nxLink, newRoot)
	// path_is_os_tree gate — newRoot must have /etc/os-release OR
	// /usr/lib/os-release reachable through its real path.
	for _, p := range []string{"etc/os-release", "usr/lib/os-release"} {
		full := filepath.Join(newRoot, p)
		if fi, err := os.Lstat(full); err == nil {
			trace("prepivot", "  %s: present (mode=%v size=%d)", p, fi.Mode(), fi.Size())
		} else {
			trace("prepivot", "  %s: %v", p, err)
		}
	}
	// Boot gates for the re-exec after pivot — these are what systemd's
	// hardcoded SYSTEMD_BINARY_PATH resolves to inside newRoot.
	for _, p := range []string{"usr/lib/systemd/systemd", "usr/lib/systemd/systemd-executor", "sbin/init"} {
		full := filepath.Join(newRoot, p)
		if tgt, err := os.Readlink(full); err == nil {
			if _, serr := os.Stat(full); serr == nil {
				trace("prepivot", "  %s → %s (reachable)", p, tgt)
			} else {
				trace("prepivot", "  %s → %s (DANGLING: %v)", p, tgt, serr)
			}
		} else if fi, err := os.Lstat(full); err == nil {
			trace("prepivot", "  %s: regular, mode=%v", p, fi.Mode())
		} else {
			trace("prepivot", "  %s: MISSING (%v)", p, err)
		}
	}
	// Mount state of key paths — systemd's switch_root path_is_mount_point
	// checks drive whether entries are skipped or bind-mounted.
	trace("prepivot", "----- mountinfo for key paths -----")
	mi, err := os.ReadFile("/proc/self/mountinfo")
	if err == nil {
		for _, line := range strings.Split(string(mi), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				continue
			}
			mp := fields[4]
			if mp == "/" || mp == "/nix" || mp == "/run" || mp == "/run/nextroot" || strings.HasPrefix(mp, newRoot) {
				trace("prepivot", "  mount: %s", line)
			}
		}
	}
	trace("prepivot", "----- newRoot top-level -----")
	if entries, err := os.ReadDir(newRoot); err == nil {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		trace("prepivot", "  %v", names)
	}
	trace("prepivot", "===== end pre-pivot dump =====")
}

// dumpJournalGrep reads the journal-logger output file (populated by
// our systemd-run'd watchdog) and surfaces lines that would explain a
// pivot failure. systemd logs "Failed to switch root" / "not a valid
// OS tree" / "nextroot" through its normal log channel — journal
// receives them, our file receives journal, we grep.
func dumpJournalGrep(poll int) {
	const journalPath = "/var/log/symbiote/journal.log"
	data, err := os.ReadFile(journalPath)
	if err != nil {
		trace("reboot", "journal-grep poll #%d: cannot read %s: %v", poll, journalPath, err)
		return
	}
	lines := strings.Split(string(data), "\n")
	// Last ~200 lines; grep for pivot-relevant tokens.
	start := len(lines) - 200
	if start < 0 {
		start = 0
	}
	keep := []string{}
	for _, l := range lines[start:] {
		low := strings.ToLower(l)
		if strings.Contains(low, "nextroot") ||
			strings.Contains(low, "switch root") ||
			strings.Contains(low, "switch-root") ||
			strings.Contains(low, "soft-reboot") ||
			strings.Contains(low, "soft reboot") ||
			strings.Contains(low, "pivot") ||
			strings.Contains(low, "failed to execute") ||
			strings.Contains(low, "os tree") ||
			strings.Contains(low, "spawn executor") {
			keep = append(keep, l)
		}
	}
	if len(keep) == 0 {
		trace("reboot", "journal-grep poll #%d: no pivot-related messages yet", poll)
		return
	}
	trace("reboot", "journal-grep poll #%d: %d matching line(s):\n  %s", poll, len(keep), strings.Join(keep, "\n  "))
}

// dumpStuckState writes diagnostics useful when phase 2 is hung — reads
// static /proc and /run files that cannot block on D-Bus.
func dumpStuckState(poll int) {
	trace("reboot", "===== stuck diagnostics (poll #%d) =====", poll)
	exe, _ := os.Readlink("/proc/1/exe")
	comm, _ := os.ReadFile("/proc/1/comm")
	trace("reboot", "pid1: exe=%s comm=%s", exe, strings.TrimSpace(string(comm)))
	for _, p := range []string{
		"/run/systemd/system",
		"/run/systemd/private",
		"/run/dbus/system_bus_socket",
		"/run/current-system",
		"/etc/os-release",
	} {
		fi, err := os.Lstat(p)
		if err != nil {
			trace("reboot", "  %s: %v", p, err)
			continue
		}
		trace("reboot", "  %s: mode=%v", p, fi.Mode())
	}
	// What is pid 1 blocked on? /proc/1/wchan is a kernel symbol.
	wchan, _ := os.ReadFile("/proc/1/wchan")
	trace("reboot", "pid1 wchan=%s", strings.TrimSpace(string(wchan)))
	// Our own context.
	selfExe, _ := os.Readlink("/proc/self/exe")
	trace("reboot", "self: exe=%s pid=%d", selfExe, os.Getpid())
	// Try one more thing WITHOUT systemctl — read systemd notify socket entries.
	if entries, err := os.ReadDir("/run/systemd/units"); err == nil {
		trace("reboot", "/run/systemd/units has %d entries", len(entries))
	} else {
		trace("reboot", "/run/systemd/units: %v", err)
	}
	trace("reboot", "===== end diagnostics =====")
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
