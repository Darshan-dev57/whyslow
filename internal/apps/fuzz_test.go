package apps

import (
	"bytes"
	"strings"
	"testing"
)

// A process can set its whole command line to a single string of spaces
// (exec -a " " cat). That crashed whyslow, root daemon included.
func TestBlankTitle(t *testing.T) {
	for _, argv := range [][]string{{" "}, {"   "}, {"\t \n"}, {":"}, {" : "}} {
		p := &Proc{PID: 7, Comm: "cat", Cmdline: argv}
		if got := Exe(p); got != "cat" {
			t.Errorf("Exe(%q) = %q, want comm", argv, got)
		}
	}
}

func TestNoEmptyName(t *testing.T) {
	for _, p := range []*Proc{{Comm: ""}, {Comm: "  "}, {Comm: "", Cmdline: []string{"/"}}, {Comm: "", Cmdline: []string{" "}}} {
		if got := OwnName(p); got != Unnamed {
			t.Errorf("OwnName(%+v) = %q", p, got)
		}
	}
}

// FuzzGroup builds a small process tree from fuzz input and checks that
// naming and grouping never panic and give every process a name.
func FuzzGroup(f *testing.F) {
	f.Add([]byte("bash\x00-c\x00x"), []byte("a) b ("), 3)
	f.Add([]byte(" "), []byte(""), 0)
	f.Add([]byte("python3\x00-m"), []byte("python3"), 1)
	f.Add([]byte("postgres: checkpointer"), []byte("postgres"), 2)
	f.Add([]byte("java\x00-jar"), []byte("java"), 4)
	f.Fuzz(func(t *testing.T, cmd, comm []byte, shape int) {
		var argv []string
		if c := bytes.TrimRight(cmd, "\x00"); len(c) > 0 {
			argv = strings.Split(string(c), "\x00")
		}
		procs := map[int]*Proc{}
		for i := 1; i <= 4; i++ {
			ppid := (i + shape) % 6 // parents may be missing, self or a cycle
			procs[i] = &Proc{PID: i, PPID: ppid, Comm: string(comm), Cmdline: argv, Kernel: shape%7 == i}
		}
		procs[5] = &Proc{PID: 5, PPID: 1, Comm: "sudo", Cmdline: []string{"sudo", "x"}}
		names := Group(procs)
		for pid := range procs {
			if n, ok := names[pid]; !ok || n == "" {
				t.Fatalf("pid %d has no name", pid)
			}
		}
	})
}
