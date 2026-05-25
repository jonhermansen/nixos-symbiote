//go:build linux

package main

import (
	"bufio"
	"fmt"
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

func switchSystem(nixBin, system string) error {
	ensureProtectedScope()

	protected := protectedUnits()
	log.Printf("protected: %v", protected)

	newRoot := constructRoot(system)
	activate(nixBin, system, newRoot)
	protect(protected)
	return softReboot(newRoot)
}

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

	pid := os.Getpid()
	for pid > 1 {
		if f, err := os.Open(fmt.Sprintf("/proc/%d/cgroup", pid)); err == nil {
			s := bufio.NewScanner(f)
			for s.Scan() {
				if m := unitRe.FindStringSubmatch(s.Text()); m != nil && !seen[m[1]] {
					seen[m[1]] = true
					units = append(units, m[1])
				}
			}
			f.Close()
		}
		pid = ppidOf(pid)
	}

	for _, u := range []string{"nix-daemon.service", "walinuxagent.service"} {
		if !seen[u] {
			units = append(units, u)
		}
	}
	return units
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
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	os.Remove(filepath.Join(newRoot, "etc/os-release"))
	os.WriteFile(filepath.Join(newRoot, "etc/os-release"), []byte("ID=nixos\n"), 0644)

	for _, d := range []string{"run", "sys", "proc", "dev"} {
		syscall.Unmount(filepath.Join(newRoot, d), syscall.MNT_DETACH)
	}
}

// --- protect units ---

func protect(units []string) {
	dropin := []byte("[Unit]\nDefaultDependencies=no\nIgnoreOnIsolate=yes\nSurviveFinalKillSignal=yes\n")
	for _, unit := range units {
		dir := filepath.Join("/run/systemd/system", unit+".d")
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, "survive.conf"), dropin, 0644)
		log.Printf("protected: %s", unit)
	}
	mustRun(exec.Command("systemctl", "daemon-reload"))
}

// --- soft-reboot ---

func softReboot(newRoot string) error {
	syscall.Mount(newRoot, newRoot, "", syscall.MS_BIND, "")
	os.Remove("/run/nextroot")
	os.Symlink(newRoot, "/run/nextroot")

	log.Printf("triggering soft-reboot into %s", newRoot)
	return exec.Command("systemctl", "soft-reboot").Run()
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
