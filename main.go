package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func main() {
	log.SetFlags(0)
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	cfg := parseArgs(args)

	nixBin := ensureNix()

	system := buildSystem(nixBin, cfg)
	log.Printf("built: %s", system)

	return switchSystem(nixBin, system)
}

// --- config ---

type config struct {
	configPath string
	nixpkgsSrc string
	flakeAttr  string
}

func parseArgs(args []string) config {
	var cfg config
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			i++
			cfg.configPath, _ = filepath.Abs(args[i])
		case "--nixpkgs":
			i++
			cfg.nixpkgsSrc = args[i]
		case "--flake":
			i++
			cfg.flakeAttr = args[i]
		default:
			cfg.configPath, _ = filepath.Abs(args[i])
		}
	}
	if cfg.configPath == "" && cfg.flakeAttr == "" {
		log.Fatal("usage: nixos-symbiote --config <path> | --flake <attr>")
	}
	return cfg
}

// --- nix bootstrap ---

func ensureNix() string {
	if p, err := exec.LookPath("nix-build"); err == nil {
		return filepath.Dir(p)
	}
	for _, p := range []string{
		"/nix/var/nix/profiles/default/bin/nix-build",
		filepath.Join(os.Getenv("HOME"), ".nix-profile/bin/nix-build"),
	} {
		if _, err := os.Stat(p); err == nil {
			return filepath.Dir(p)
		}
	}

	if _, err := os.Stat("/nix"); err == nil {
		log.Fatal("/nix exists but nix-build not found — refusing to clobber existing installation")
	}

	log.Println("nix not found, bootstrapping...")

	goArchToNix := map[string]string{
		"amd64": "x86_64", "arm64": "aarch64", "386": "i686",
		"arm": "armv7l", "riscv64": "riscv64",
	}
	arch := goArchToNix[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}

	platform := arch + "-" + runtime.GOOS
	expectedHash, ok := nixPlatformHashes[platform]
	if !ok {
		log.Fatalf("no nix release for %s", platform)
	}

	url := fmt.Sprintf("https://releases.nixos.org/nix/nix-%s/nix-%s-%s.tar.xz",
		nixVersion, nixVersion, platform)

	log.Printf("fetching nix %s for %s", nixVersion, platform)

	os.MkdirAll("/nix", 0755)
	tmpDir, _ := os.MkdirTemp("", "nix-bootstrap-*")

	resp, err := http.Get(url)
	if err != nil {
		log.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()

	tarball := filepath.Join(tmpDir, "nix.tar.xz")
	f, _ := os.Create(tarball)
	h := sha256.New()
	io.Copy(f, io.TeeReader(resp.Body, h))
	f.Close()

	got := "sha256-" + base64.StdEncoding.EncodeToString(h.Sum(nil))
	if got != expectedHash {
		log.Fatalf("hash mismatch for %s:\n  expected: %s\n       got: %s", platform, expectedHash, got)
	}
	log.Printf("verified %s", expectedHash)

	mustRun(exec.Command("tar", "xJf", tarball, "-C", tmpDir, "--strip-components=1"))
	mustRun(exec.Command(filepath.Join(tmpDir, "install"), "--no-daemon"))

	nixBin := filepath.Join(os.Getenv("HOME"), ".nix-profile/bin")
	log.Printf("nix %s bootstrapped", nixVersion)
	return nixBin
}

// --- build ---

func buildSystem(nixBin string, cfg config) string {
	if cfg.flakeAttr != "" {
		cmd := exec.Command(filepath.Join(nixBin, "nix"), "build",
			cfg.flakeAttr+".config.system.build.toplevel",
			"--no-link", "--print-out-paths")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			log.Fatalf("nix build: %v", err)
		}
		return strings.TrimSpace(string(out))
	}

	nixpkgs := cfg.nixpkgsSrc
	if nixpkgs == "" {
		nixpkgs = nixpkgsRevFromLock()
	}
	if nixpkgs == "" {
		nixpkgs = "nixos-unstable"
	}

	rootDev, rootFs := mountSource("/")
	nixDev, nixFs := mountSource("/nix")

	scriptDir, _ := filepath.Abs(filepath.Dir(os.Args[0]))

	cmd := exec.Command(filepath.Join(nixBin, "nix-build"), filepath.Join(scriptDir, "system.nix"),
		"--argstr", "configPath", cfg.configPath,
		"--argstr", "nixpkgsSrc", nixpkgs,
		"--argstr", "rootDevice", rootDev,
		"--argstr", "rootFsType", rootFs,
		"--argstr", "nixDevice", nixDev,
		"--argstr", "nixFsType", nixFs,
		"--argstr", "dnsServers", dnsServers(),
		"--argstr", "username", os.Getenv("USER"),
		"--argstr", "userHome", os.Getenv("HOME"),
		"--argstr", "userUid", strconv.Itoa(os.Getuid()),
		"--no-out-link",
	)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		log.Fatalf("nix-build: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// --- helpers ---

func mustRun(cmd *exec.Cmd) {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("%s: %v", cmd.Path, err)
	}
}

func dnsServers() string {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return "1.1.1.1"
	}
	defer f.Close()
	var servers []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "nameserver ") {
			servers = append(servers, strings.TrimPrefix(line, "nameserver "))
		}
	}
	if len(servers) == 0 {
		return "1.1.1.1"
	}
	return strings.Join(servers, " ")
}

func nixpkgsRevFromLock() string {
	scriptDir, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	data, err := os.ReadFile(filepath.Join(scriptDir, "flake.lock"))
	if err != nil {
		return ""
	}
	var lock struct {
		Nodes map[string]struct {
			Locked struct {
				Rev string `json:"rev"`
			} `json:"locked"`
		} `json:"nodes"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return ""
	}
	return lock.Nodes["nixpkgs"].Locked.Rev
}
