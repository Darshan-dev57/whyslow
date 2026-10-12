package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// invoker is the user who started whyslow. Under sudo that is SUDO_USER,
// not root, so the database lands in their home and stays readable by
// `whyslow report` run without sudo.
type invoker struct {
	home     string
	uid, gid int // -1 when not running under sudo
}

func currentInvoker() invoker {
	inv := invoker{uid: -1, gid: -1}
	if os.Geteuid() == 0 {
		if uid, err := strconv.Atoi(os.Getenv("SUDO_UID")); err == nil && uid != 0 {
			if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
				gid, _ := strconv.Atoi(os.Getenv("SUDO_GID"))
				return invoker{home: u.HomeDir, uid: uid, gid: gid}
			}
		}
	}
	inv.home, _ = os.UserHomeDir()
	return inv
}

// defaultDBPath follows the XDG base directory spec.
func defaultDBPath() string {
	inv := currentInvoker()
	if inv.uid < 0 {
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			return filepath.Join(d, "whyslow", "whyslow.db")
		}
	}
	return filepath.Join(inv.home, ".local", "share", "whyslow", "whyslow.db")
}

// handBack gives files created as root under sudo back to the invoking user.
func handBack(paths ...string) {
	inv := currentInvoker()
	if inv.uid < 0 {
		return
	}
	for _, p := range paths {
		_ = os.Lchown(p, inv.uid, inv.gid)
	}
}
