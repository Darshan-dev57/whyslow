// Package apps groups processes into the applications people recognise.
//
// A browser is dozens of processes, a compiler run is a tree of short-lived
// children, and both should be blamed as one thing. The rules, in order:
//
//  1. Kernel threads are "kernel".
//  2. Well-known multi-process apps are named by their binary
//     (every chrome renderer is "chrome").
//  3. Otherwise a process belongs to its parent's app, unless the parent is
//     a launcher: an interactive shell, terminal, session manager, systemd,
//     sudo, a container shim, or a wrapper such as timeout or nohup.
//     Launchers start independent work, so their children stand alone.
//  4. Interpreters are named by what they run (java:app.jar, python:train.py).
//  5. Anything else is named by its binary.
package apps

import (
	"path"
	"strings"
)

// Proc is the identity of one process, as far as grouping needs it.
type Proc struct {
	PID     int
	PPID    int
	Comm    string
	Cmdline []string
	Kernel  bool
}

// KernelApp is the app name for kernel threads.
const KernelApp = "kernel"

var known = map[string]string{
	"chrome": "chrome", "google-chrome": "chrome", "google-chrome-stable": "chrome",
	"chrome_crashpad_handler": "chrome",
	"chromium":                "chromium", "chromium-browser": "chromium",
	"firefox": "firefox", "firefox-esr": "firefox", "firefox-bin": "firefox",
	"brave": "brave", "brave-browser": "brave", "msedge": "edge", "opera": "opera", "vivaldi-bin": "vivaldi",
	"code": "vscode", "code-oss": "vscode", "codium": "vscode", "cursor": "cursor",
	"slack": "slack", "discord": "discord", "Discord": "discord", "spotify": "spotify",
	"obsidian": "obsidian", "zoom": "zoom", "teams-for-linux": "teams", "signal-desktop": "signal",
	"telegram-desktop": "telegram", "steam": "steam", "thunderbird": "thunderbird",
	"idea": "intellij", "pycharm": "pycharm", "goland": "goland", "studio": "android-studio",
}

var shells = map[string]bool{
	"sh": true, "bash": true, "dash": true, "zsh": true, "fish": true, "ksh": true,
	"tcsh": true, "csh": true, "nu": true,
}

// launchers start independent work; their children are separate apps.
var launchers = map[string]bool{
	"init": true, "systemd": true, "sudo": true, "su": true, "doas": true, "sshd": true,
	"login": true, "agetty": true, "tmux": true, "screen": true, "SCREEN": true,
	"gnome-terminal-server": true, "konsole": true, "alacritty": true, "kitty": true,
	"xterm": true, "wezterm-gui": true, "foot": true, "tilix": true, "terminator": true,
	"xfce4-terminal": true, "ptyxis-agent": true, "gnome-shell": true, "plasmashell": true,
	"kwin_x11": true, "kwin_wayland": true, "xfce4-panel": true, "xfce4-session": true,
	"gnome-session-binary": true, "lxsession": true, "cinnamon": true, "mate-panel": true,
	"i3": true, "sway": true, "Hyprland": true, "runc": true, "conmon": true, "crun": true,
	"timeout": true, "env": true, "nice": true, "ionice": true, "nohup": true, "setsid": true,
	"stdbuf": true, "xargs": true, "time": true, "strace": true, "flatpak-spawn": true,
	"bwrap": true, "dbus-daemon": true, "dbus-broker": true, "xdg-desktop-portal": true,
	"supervisord": true, "tini": true, "dumb-init": true,
}

// Exe returns the binary name of a process: the base name of argv[0], with
// setproctitle decorations removed ("postgres: checkpointer" -> "postgres"),
// or comm when there is no command line.
func Exe(p *Proc) string {
	if len(p.Cmdline) == 0 || p.Cmdline[0] == "" {
		return p.Comm
	}
	arg0 := p.Cmdline[0]
	// Programs that rewrite their title often pack everything into argv[0].
	// A title made only of spaces has no first field; fall back to comm.
	if len(p.Cmdline) == 1 && strings.ContainsAny(arg0, " :") {
		f := strings.Fields(arg0)
		if len(f) == 0 {
			return p.Comm
		}
		arg0 = strings.TrimSuffix(f[0], ":")
	}
	base := strings.TrimPrefix(path.Base(arg0), "-") // "-bash" is a login shell
	if base == "." || base == "/" || base == "" {
		return p.Comm
	}
	return base
}

// IsLauncher reports whether children of p should be separate apps.
func IsLauncher(p *Proc) bool {
	exe := Exe(p)
	if launchers[exe] || strings.HasPrefix(exe, "containerd-shim") {
		return true
	}
	if shells[exe] {
		return scriptArg(p.Cmdline) == ""
	}
	return false
}

// wrappers run other commands and do almost no work of their own.
var wrappers = map[string]bool{
	"timeout": true, "env": true, "nice": true, "ionice": true, "nohup": true, "setsid": true,
	"stdbuf": true, "xargs": true, "time": true, "strace": true, "watch": true, "parallel": true,
}

// IsWrapper reports whether p is a launcher that runs commands one after
// another and does little itself: a shell running inline code or reading
// commands, xargs, timeout and the like. Sessions that adopt unrelated
// processes (init, sshd, terminals) are launchers but not wrappers.
func IsWrapper(p *Proc) bool {
	exe := Exe(p)
	if shells[exe] {
		return scriptArg(p.Cmdline) == ""
	}
	return wrappers[exe]
}

// OwnName is the app name a process would have if it stood alone.
func OwnName(p *Proc) string {
	if p.Kernel {
		return KernelApp
	}
	exe := Exe(p)
	if name, ok := known[exe]; ok {
		return name
	}
	switch {
	case exe == "java":
		if s := javaTarget(p.Cmdline); s != "" {
			return "java:" + s
		}
	case strings.HasPrefix(exe, "python"), exe == "node", exe == "ruby", exe == "perl",
		exe == "php", exe == "deno", exe == "bun":
		lang := exe
		if strings.HasPrefix(exe, "python") {
			lang = "python"
		}
		if s := scriptArg(p.Cmdline); s != "" {
			return lang + ":" + s
		}
		return lang
	case shells[exe]:
		if s := scriptArg(p.Cmdline); s != "" {
			return s
		}
	}
	return exe
}

func isKnown(p *Proc) bool {
	_, ok := known[Exe(p)]
	return ok && !p.Kernel
}

// scriptArg finds the script an interpreter runs: the first argument that is
// not an option, or the module after "-m". "-c" means inline code, no script.
func scriptArg(argv []string) string {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-c" || a == "-e" || a == "--eval" || a == "-r":
			return ""
		case a == "-m" && i+1 < len(argv):
			return argv[i+1]
		case strings.HasPrefix(a, "-"):
			continue
		default:
			return path.Base(a)
		}
	}
	return ""
}

// javaTarget returns the jar or main class a JVM runs.
func javaTarget(argv []string) string {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-jar" && i+1 < len(argv):
			return path.Base(argv[i+1])
		case a == "-cp" || a == "-classpath" || a == "--class-path" || a == "-p" ||
			a == "--module-path" || a == "--add-opens" || a == "--add-exports":
			i++ // option with a value
		case (a == "-m" || a == "--module") && i+1 < len(argv):
			return lastDot(strings.Split(argv[i+1], "/")[0])
		case strings.HasPrefix(a, "-"):
			continue
		default:
			return lastDot(a)
		}
	}
	return ""
}

func lastDot(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	return s
}

// Group assigns every process to an app name. procs is keyed by PID; a
// parent that is missing from the map (already exited, or outside our PID
// namespace) ends the walk.
func Group(procs map[int]*Proc) map[int]string {
	out := make(map[int]string, len(procs))
	var resolve func(pid int, depth int) string
	resolve = func(pid int, depth int) string {
		if name, ok := out[pid]; ok {
			return name
		}
		p := procs[pid]
		name := OwnName(p)
		if !p.Kernel && !isKnown(p) && depth < 64 {
			if parent, ok := procs[p.PPID]; ok && p.PPID != pid && !parent.Kernel && !IsLauncher(parent) {
				name = resolve(p.PPID, depth+1)
			}
		}
		out[pid] = name
		return name
	}
	for pid := range procs {
		resolve(pid, 0)
	}
	return out
}
