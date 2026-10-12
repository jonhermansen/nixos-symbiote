//go:build linux

package main

import (
	"fmt"
	"log"
	"os/exec"
	"strconv"
)

// Constants mirror upstream nix/scripts/install-systemd-multi-user.sh:
//   NIX_BUILD_GROUP_NAME         = "nixbld"
//   NIX_BUILD_GROUP_ID           = 30000
//   NIX_FIRST_BUILD_UID          = 30001
//   NIX_BUILD_USER_NAME_TEMPLATE = "nixbld%d"
//   NIX_USER_COUNT               = 32
const (
	nixBuildGroupName  = "nixbld"
	nixBuildGroupID    = 30000
	nixFirstBuildUID   = 30001
	nixBuildUserCount  = 32
	nixBuildUserFormat = "nixbld%d"
)

// createNixUsers creates the nixbld group + 32 build users exactly as the
// upstream multi-user installer does. Idempotent (skips existing entries).
func createNixUsers() {
	if err := exec.Command("getent", "group", nixBuildGroupName).Run(); err != nil {
		cmd := exec.Command("groupadd",
			"-g", strconv.Itoa(nixBuildGroupID),
			"--system",
			nixBuildGroupName)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("groupadd %s: %v: %s", nixBuildGroupName, err, out)
			return
		}
		log.Printf("created group %s (gid=%d)", nixBuildGroupName, nixBuildGroupID)
	}

	for i := 1; i <= nixBuildUserCount; i++ {
		name := fmt.Sprintf(nixBuildUserFormat, i)
		if err := exec.Command("getent", "passwd", name).Run(); err == nil {
			continue
		}
		uid := nixFirstBuildUID + i - 1
		cmd := exec.Command("useradd",
			"--home-dir", "/var/empty",
			"--comment", fmt.Sprintf("Nix build user %d", i),
			"--gid", strconv.Itoa(nixBuildGroupID),
			"--groups", nixBuildGroupName,
			"--no-user-group",
			"--system",
			"--shell", "/sbin/nologin",
			"--uid", strconv.Itoa(uid),
			"--password", "!",
			name)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("useradd %s: %v: %s", name, err, out)
		}
	}
	log.Printf("ensured %d build users (%s1..%s%d)",
		nixBuildUserCount, nixBuildGroupName[:6], nixBuildGroupName[:6], nixBuildUserCount)
}
