package apps

import "testing"

func procs(list ...*Proc) map[int]*Proc {
	m := make(map[int]*Proc, len(list))
	for _, p := range list {
		m[p.PID] = p
	}
	return m
}

func TestGroup(t *testing.T) {
	tree := procs(
		&Proc{PID: 1, Comm: "systemd", Cmdline: []string{"/sbin/init", "splash"}},
		&Proc{PID: 2, Comm: "kthreadd", Kernel: true},
		&Proc{PID: 50, PPID: 2, Comm: "kworker/0:1", Kernel: true},
		// chrome: the browser under systemd --user, a zygote and renderers below it
		&Proc{PID: 900, PPID: 1, Comm: "systemd", Cmdline: []string{"/lib/systemd/systemd", "--user"}},
		&Proc{PID: 1000, PPID: 900, Comm: "chrome", Cmdline: []string{"/opt/google/chrome/chrome"}},
		&Proc{PID: 1001, PPID: 1000, Comm: "chrome", Cmdline: []string{"/opt/google/chrome/chrome --type=zygote"}},
		&Proc{PID: 1002, PPID: 1001, Comm: "chrome", Cmdline: []string{"/opt/google/chrome/chrome --type=renderer --lang=en"}},
		&Proc{PID: 1003, PPID: 1000, Comm: "chrome_crashpad", Cmdline: []string{"/opt/google/chrome/chrome_crashpad_handler"}},
		&Proc{PID: 1004, PPID: 1000, Comm: "cat", Cmdline: []string{"cat"}},
		// a terminal running an interactive shell that launched dd and make
		&Proc{PID: 2000, PPID: 900, Comm: "gnome-terminal-", Cmdline: []string{"/usr/libexec/gnome-terminal-server"}},
		&Proc{PID: 2001, PPID: 2000, Comm: "bash", Cmdline: []string{"bash"}},
		&Proc{PID: 2002, PPID: 2001, Comm: "dd", Cmdline: []string{"dd", "if=/dev/zero", "of=big"}},
		&Proc{PID: 2003, PPID: 2001, Comm: "make", Cmdline: []string{"make", "-j8"}},
		&Proc{PID: 2004, PPID: 2003, Comm: "gcc", Cmdline: []string{"gcc", "-c", "x.c"}},
		&Proc{PID: 2005, PPID: 2004, Comm: "cc1", Cmdline: []string{"/usr/lib/gcc/x86_64-linux-gnu/12/cc1", "x.c"}},
		&Proc{PID: 2006, PPID: 2001, Comm: "timeout", Cmdline: []string{"timeout", "30", "curl", "x"}},
		&Proc{PID: 2007, PPID: 2006, Comm: "curl", Cmdline: []string{"curl", "x"}},
		// a script shell owns its children; an inline "sh -c" does not
		&Proc{PID: 3000, PPID: 900, Comm: "bash", Cmdline: []string{"/bin/bash", "/home/d/backup.sh"}},
		&Proc{PID: 3001, PPID: 3000, Comm: "tar", Cmdline: []string{"tar", "czf", "b.tgz", "."}},
		&Proc{PID: 3100, PPID: 900, Comm: "sh", Cmdline: []string{"sh", "-c", "sha256sum /dev/zero"}},
		&Proc{PID: 3101, PPID: 3100, Comm: "sha256sum", Cmdline: []string{"sha256sum", "/dev/zero"}},
		// interpreters
		&Proc{PID: 4000, PPID: 2001, Comm: "java", Cmdline: []string{"java", "-Xmx2g", "-jar", "/srv/app/orders-service.jar"}},
		&Proc{PID: 4001, PPID: 2001, Comm: "java", Cmdline: []string{"/usr/bin/java", "-cp", "lib/*", "org.gradle.launcher.daemon.bootstrap.GradleDaemon", "8.5"}},
		&Proc{PID: 4002, PPID: 2001, Comm: "python3", Cmdline: []string{"python3", "-u", "train.py", "--epochs", "3"}},
		&Proc{PID: 4003, PPID: 2001, Comm: "python3.11", Cmdline: []string{"python3.11", "-m", "http.server"}},
		&Proc{PID: 4004, PPID: 2001, Comm: "node", Cmdline: []string{"node", "dist/server.js"}},
		&Proc{PID: 4005, PPID: 4004, Comm: "esbuild", Cmdline: []string{"esbuild", "--service"}},
		// setproctitle and orphans
		&Proc{PID: 5000, PPID: 1, Comm: "postgres", Cmdline: []string{"postgres: checkpointer "}},
		&Proc{PID: 5001, PPID: 4242, Comm: "orphan", Cmdline: []string{"/usr/bin/orphan"}},
		// login shell under sshd
		&Proc{PID: 6000, PPID: 1, Comm: "sshd", Cmdline: []string{"sshd: darshan@pts/0"}},
		&Proc{PID: 6001, PPID: 6000, Comm: "bash", Cmdline: []string{"-bash"}},
		&Proc{PID: 6002, PPID: 6001, Comm: "htop", Cmdline: []string{"htop"}},
	)
	want := map[int]string{
		1: "init", 2: "kernel", 50: "kernel", 900: "systemd",
		1000: "chrome", 1001: "chrome", 1002: "chrome", 1003: "chrome", 1004: "chrome",
		2000: "gnome-terminal-server", 2001: "bash",
		2002: "dd", 2003: "make", 2004: "make", 2005: "make", 2006: "timeout", 2007: "curl",
		3000: "backup.sh", 3001: "backup.sh", 3100: "sh", 3101: "sha256sum",
		4000: "java:orders-service.jar", 4001: "java:GradleDaemon",
		4002: "python:train.py", 4003: "python:http.server",
		4004: "node:server.js", 4005: "node:server.js",
		5000: "postgres", 5001: "orphan",
		6000: "sshd", 6001: "bash", 6002: "htop",
	}
	got := Group(tree)
	for pid, w := range want {
		if got[pid] != w {
			t.Errorf("pid %d (%s): got %q, want %q", pid, tree[pid].Comm, got[pid], w)
		}
	}
	if len(got) != len(tree) {
		t.Errorf("grouped %d of %d processes", len(got), len(tree))
	}
}

func TestExe(t *testing.T) {
	cases := []struct {
		p    Proc
		want string
	}{
		{Proc{Comm: "kworker"}, "kworker"},
		{Proc{Comm: "x", Cmdline: []string{""}}, "x"},
		{Proc{Comm: "Web Content", Cmdline: []string{"/usr/lib/firefox/firefox", "-contentproc"}}, "firefox"},
		{Proc{Comm: "sshd", Cmdline: []string{"sshd: user [priv]"}}, "sshd"},
	}
	for _, c := range cases {
		if got := Exe(&c.p); got != c.want {
			t.Errorf("Exe(%v) = %q, want %q", c.p.Cmdline, got, c.want)
		}
	}
}

func TestSelfParentDoesNotLoop(t *testing.T) {
	got := Group(procs(&Proc{PID: 7, PPID: 7, Comm: "weird", Cmdline: []string{"weird"}}))
	if got[7] != "weird" {
		t.Errorf("got %q", got[7])
	}
}
