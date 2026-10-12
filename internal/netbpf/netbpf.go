package netbpf

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"

	"github.com/Darshan-dev57/whyslow/internal/collector"
)

// MapSize bounds the counters map. It is an LRU map: when full, the
// least recently updated process is evicted, and the collector treats its
// next appearance as a fresh counter.
const MapSize = 16384

const mapName = "whyslow_net"

// enotsupp is the kernel-internal ENOTSUPP (524) some BPF paths return.
const enotsupp = unix.Errno(524)

// Hook is one kernel function whyslow attaches to.
type Hook struct {
	Func     string
	Exit     bool // fexit (byte count is the return value) instead of fentry
	Arg      int  // for fentry: index of the byte-count argument
	Dir      Direction
	Required bool // whyslow refuses to run eBPF without it
}

// Hooks lists every attach point.
var Hooks = []Hook{
	{Func: "tcp_sendmsg", Exit: true, Dir: Tx, Required: true},
	{Func: "tcp_cleanup_rbuf", Arg: 1, Dir: Rx, Required: true},
	{Func: "udp_sendmsg", Exit: true, Dir: Tx},
	{Func: "udp_recvmsg", Exit: true, Dir: Rx},
	{Func: "udpv6_sendmsg", Exit: true, Dir: Tx},
	{Func: "udpv6_recvmsg", Exit: true, Dir: Rx},
}

// Slot finds where a hook's byte count lives in the trampoline context:
// fentry sees the arguments as u64 slots, and fexit sees them followed by
// the return value. The kernel's BTF says how many arguments the function
// has on this kernel and that the count really is an int.
func (h Hook) Slot(spec *btf.Spec) (int, error) {
	var fn *btf.Func
	if err := spec.TypeByName(h.Func, &fn); err != nil {
		return 0, fmt.Errorf("%s: not in kernel BTF: %w", h.Func, err)
	}
	proto, ok := fn.Type.(*btf.FuncProto)
	if !ok {
		return 0, fmt.Errorf("%s: no function prototype in BTF", h.Func)
	}
	slot, typ := h.Arg, btf.Type(nil)
	if h.Exit {
		slot, typ = len(proto.Params), proto.Return
	} else {
		if h.Arg >= len(proto.Params) {
			return 0, fmt.Errorf("%s: has %d arguments, want argument %d", h.Func, len(proto.Params), h.Arg)
		}
		typ = proto.Params[h.Arg].Type
	}
	if !isInt(typ) {
		return 0, fmt.Errorf("%s: byte count has type %v, want int", h.Func, typ)
	}
	if slot >= 12 {
		return 0, fmt.Errorf("%s: slot %d out of range", h.Func, slot)
	}
	return slot, nil
}

func isInt(t btf.Type) bool {
	for {
		switch v := t.(type) {
		case *btf.Typedef:
			t = v.Type
		case *btf.Int:
			return v.Size == 4 && v.Encoding == btf.Signed
		default:
			return false
		}
	}
}

// Mode names how a Source counts.
type Mode string

// The two counting modes.
const (
	// ModeTrampoline counts at the TCP/UDP send and receive functions with
	// fentry/fexit: application bytes, every socket.
	ModeTrampoline Mode = "fentry"
	// ModeSocketOwner counts IP packets per socket owner with cgroup
	// programs, for kernels without the BPF JIT.
	ModeSocketOwner Mode = "socket-owner"
)

// Source is the loaded eBPF network counter. It implements
// collector.NetSource.
type Source struct {
	mode     Mode
	counters *ebpf.Map
	owners   *ebpf.Map
	progs    []*ebpf.Program
	links    []link.Link
	attached []string
	skipped  []string
	fallback error // why ModeTrampoline was not used
}

var _ collector.NetSource = (*Source)(nil)

// Load loads and attaches the counters: trampolines when the kernel can
// run them, socket-owner programs otherwise. It needs root (CAP_BPF,
// CAP_PERFMON and, for the fallback, CAP_NET_ADMIN); the error says what
// failed in each mode.
func Load() (*Source, error) {
	// kernels before 5.11 charge BPF memory to RLIMIT_MEMLOCK
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, permHint(fmt.Errorf("raise memlock limit: %w", err))
	}
	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Name: mapName, Type: ebpf.LRUHash, KeySize: 4, ValueSize: valueLen, MaxEntries: MapSize,
	})
	if err != nil {
		return nil, permHint(fmt.Errorf("create map: %w", err))
	}
	s := &Source{counters: m}
	errT := s.loadTrampolines()
	if errT == nil {
		s.mode = ModeTrampoline
		return s, nil
	}
	s.detach()
	errS := s.loadSocketOwner()
	if errS == nil {
		s.mode = ModeSocketOwner
		s.fallback = errT
		return s, nil
	}
	s.Close()
	return nil, permHint(fmt.Errorf("fentry: %v; socket-owner fallback: %w", errT, errS))
}

func (s *Source) loadTrampolines() error {
	if _, err := os.Stat("/sys/kernel/btf/vmlinux"); err != nil {
		return fmt.Errorf("kernel has no BTF (/sys/kernel/btf/vmlinux)")
	}
	spec, err := btf.LoadKernelSpec()
	if err != nil {
		return fmt.Errorf("read kernel BTF: %w", err)
	}
	for _, h := range Hooks {
		if err := s.attach(h, spec); err != nil {
			if h.Required {
				return err
			}
			s.skipped = append(s.skipped, fmt.Sprintf("%s (%v)", h.Func, err))
			continue
		}
		s.attached = append(s.attached, h.Func)
	}
	return nil
}

func (s *Source) attach(h Hook, spec *btf.Spec) error {
	slot, err := h.Slot(spec)
	if err != nil {
		return err
	}
	at := ebpf.AttachTraceFEntry
	if h.Exit {
		at = ebpf.AttachTraceFExit
	}
	prog, err := s.newProgram(&ebpf.ProgramSpec{
		Name: progName(h), Type: ebpf.Tracing, AttachType: at, AttachTo: h.Func,
		Instructions: program(slot, h.Dir), License: "GPL",
	}, spec)
	if err != nil {
		if errors.Is(err, unix.ENOMEM) || errors.Is(err, enotsupp) || errors.Is(err, unix.EOPNOTSUPP) {
			// the kernel cannot build a trampoline (no BPF JIT)
			return fmt.Errorf("%s: kernel cannot run fentry/fexit programs (is the BPF JIT built in?): %w", h.Func, err)
		}
		return fmt.Errorf("%s: %w", h.Func, err)
	}
	l, err := link.AttachTracing(link.TracingOptions{Program: prog, AttachType: at})
	if err != nil {
		return fmt.Errorf("%s: attach: %w", h.Func, err)
	}
	s.links = append(s.links, l)
	return nil
}

// newProgram resolves map references, loads the program and keeps it for Close.
func (s *Source) newProgram(ps *ebpf.ProgramSpec, kernel *btf.Spec) (*ebpf.Program, error) {
	// a program may use either map or both; AssociateMap fails only when
	// the program never references the name
	maps := map[string]*ebpf.Map{mapName: s.counters, ownerMapName: s.owners}
	for name, m := range maps {
		if m == nil || !references(ps.Instructions, name) {
			continue
		}
		if err := ps.Instructions.AssociateMap(name, m); err != nil {
			return nil, err
		}
	}
	prog, err := ebpf.NewProgramWithOptions(ps, ebpf.ProgramOptions{KernelTypes: kernel})
	if err != nil {
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) && !errors.Is(err, unix.ENOMEM) {
			return nil, fmt.Errorf("verifier rejected the program: %+v", ve)
		}
		return nil, fmt.Errorf("load: %w", err)
	}
	s.progs = append(s.progs, prog)
	return prog, nil
}

func references(insns asm.Instructions, name string) bool {
	for _, ins := range insns {
		if ins.Reference() == name {
			return true
		}
	}
	return false
}

// cgroupRoot finds the cgroup v2 mount, whose root covers every process.
func cgroupRoot() (string, error) {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[2] == "cgroup2" {
			return f[1], nil
		}
	}
	return "", errors.New("no cgroup v2 mount (socket-owner mode needs the unified hierarchy)")
}

func (s *Source) loadSocketOwner() error {
	root, err := cgroupRoot()
	if err != nil {
		return err
	}
	key, val := ownerTypes()
	s.owners, err = ebpf.NewMap(&ebpf.MapSpec{
		Name: ownerMapName, Type: ebpf.SkStorage, KeySize: 4, ValueSize: ownerLen,
		Flags: bpfFNoPrealloc | bpfFClone, Key: key, Value: val,
	})
	if err != nil {
		return fmt.Errorf("create socket storage: %w", err)
	}
	progs := []struct {
		name  string
		typ   ebpf.ProgramType
		at    ebpf.AttachType
		insns asm.Instructions
	}{
		{"ws_sock_create", ebpf.CGroupSock, ebpf.AttachCGroupInetSockCreate, sockCreateProgram()},
		{"ws_skb_egress", ebpf.CGroupSKB, ebpf.AttachCGroupInetEgress, skbProgram(Tx)},
		{"ws_skb_ingress", ebpf.CGroupSKB, ebpf.AttachCGroupInetIngress, skbProgram(Rx)},
	}
	for _, p := range progs {
		prog, err := s.newProgram(&ebpf.ProgramSpec{Name: p.name, Type: p.typ, AttachType: p.at,
			Instructions: p.insns, License: "GPL"}, nil)
		if err != nil {
			return fmt.Errorf("%s: %w", p.name, err)
		}
		l, err := link.AttachCgroup(link.CgroupOptions{Path: root, Attach: p.at, Program: prog})
		if err != nil {
			return fmt.Errorf("%s: attach to %s: %w", p.name, root, err)
		}
		s.links = append(s.links, l)
		s.attached = append(s.attached, strings.TrimPrefix(p.name, "ws_"))
	}
	return nil
}

// progName is the kernel-visible program name (at most 15 characters).
func progName(h Hook) string {
	n := "ws_" + strings.TrimSuffix(h.Func, "msg")
	if len(n) > 15 {
		n = n[:15]
	}
	return n
}

func permHint(err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w (eBPF needs root, or CAP_BPF and CAP_PERFMON)", err)
	}
	return err
}

// Mode says how this source counts.
func (s *Source) Mode() Mode { return s.mode }

// Fallback is why trampolines were not used, or nil.
func (s *Source) Fallback() error { return s.fallback }

// Attached lists the hooked kernel functions (or cgroup hooks).
func (s *Source) Attached() []string { return s.attached }

// Skipped lists optional hooks that could not be attached, with reasons.
func (s *Source) Skipped() []string { return s.skipped }

// Counters reads the whole map.
func (s *Source) Counters() (map[uint32]collector.NetCounter, error) {
	out := make(map[uint32]collector.NetCounter, 256)
	var k uint32
	var v value
	it := s.counters.Iterate()
	for it.Next(&k, &v) {
		out[k] = collector.NetCounter{Rx: v.Rx, Tx: v.Tx, Comm: commString(v.Comm[:])}
	}
	// An LRU map can change while it is walked; a partial read only delays
	// some bytes to the next sample, so keep what was read.
	if err := it.Err(); err != nil && len(out) == 0 {
		return nil, err
	}
	return out, nil
}

func commString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// Forget deletes the counters of exited processes.
func (s *Source) Forget(pids []uint32) error {
	var first error
	for _, p := range pids {
		if err := s.counters.Delete(p); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) && first == nil {
			first = err
		}
	}
	return first
}

// detach closes links and programs but keeps the counters map.
func (s *Source) detach() {
	for _, l := range s.links {
		l.Close()
	}
	for _, p := range s.progs {
		p.Close()
	}
	s.links, s.progs, s.attached, s.skipped = nil, nil, nil, nil
	if s.owners != nil {
		s.owners.Close()
		s.owners = nil
	}
}

// Close detaches every program and frees the maps.
func (s *Source) Close() error {
	s.detach()
	if s.counters != nil {
		err := s.counters.Close()
		s.counters = nil
		return err
	}
	return nil
}
