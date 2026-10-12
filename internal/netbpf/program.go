// Package netbpf counts network bytes per process with eBPF.
//
// /proc has no per-process network counters: /proc/net/dev is per
// interface, and a socket in /proc/[pid]/fd says nothing about how much
// went through it. The kernel knows which process is calling send and
// receive, so whyslow attaches small programs to the kernel functions that
// every TCP and UDP send and receive goes through, and adds the byte counts
// to a map keyed by thread group id (the PID user space sees):
//
//	fexit  tcp_sendmsg       return value = bytes queued        -> tx
//	fentry tcp_cleanup_rbuf  "copied" argument = bytes read     -> rx
//	fexit  udp_sendmsg, udpv6_sendmsg  return value             -> tx
//	fexit  udp_recvmsg, udpv6_recvmsg  return value             -> rx
//
// fentry/fexit (BPF trampolines) are cheaper than kprobes and give typed
// access to arguments and return values. The kernel's BTF is used at load
// time to find each function and the slot of its return value, which
// differs between kernel versions (udp_recvmsg lost an argument in 5.19).
//
// Trampolines need the BPF JIT. On kernels built without it (some cloud
// and microVM kernels), whyslow falls back to socket-owner accounting: a
// cgroup sock_create program records which process opened each socket in
// socket-local storage, and cgroup skb programs on the root cgroup add every
// packet's length to that owner (see sockowner.go).
//
// The programs are written with cilium/ebpf's assembler instead of C, so
// building whyslow needs no clang.
package netbpf

import (
	"github.com/cilium/ebpf/asm"
)

// Value layout in the counters map; must match value below.
const (
	offRx    = 0
	offTx    = 8
	offComm  = 16
	commLen  = 16
	valueLen = offComm + commLen
)

// value is one map entry: cumulative bytes and the process name the kernel
// saw when the entry was created.
type value struct {
	Rx   uint64
	Tx   uint64
	Comm [commLen]byte
}

// Direction says which counter a hook adds to.
type Direction int

// The two directions.
const (
	Rx Direction = iota
	Tx
)

func (d Direction) offset() int16 {
	if d == Tx {
		return offTx
	}
	return offRx
}

// Stack layout (offsets from the frame pointer R10): the u32 key at -4,
// and a value being inserted in the 32 bytes below it, 8-byte aligned.
const (
	stackKey = -4
	valueAt  = -40
)

// xadd is "lock *(u64 *)(dst + off) += src".
func xadd(dst, src asm.Register, off int16) asm.Instruction {
	ins := asm.StoreXAdd(dst, src, asm.DWord)
	ins.Offset = off
	return ins
}

// count is the shared tail of every program: with the byte count in R6
// and the tgid key stored at stackKey, add the count to the key's entry,
// creating the entry when it is missing. fillComm writes the 16-byte process
// name into the on-stack value; it may use R1-R5 and must keep R6-R9. The
// program returns ret.
func count(dir Direction, fillComm asm.Instructions, ret int32) asm.Instructions {
	off := dir.offset()
	insns := asm.Instructions{
		// R0 = bpf_map_lookup_elem(&counters, &key)
		asm.LoadMapPtr(asm.R1, 0).WithReference(mapName),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, stackKey),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "insert"),
		// existing entry: atomic add, other CPUs may update it too
		xadd(asm.R0, asm.R6, off),
		asm.Ja.Label("exit"),

		// new entry: zero a value on the stack, then set the count and name
		// (the verifier rejects reading stack it has not seen written)
		asm.Mov.Imm(asm.R1, 0).WithSymbol("insert"),
		asm.StoreMem(asm.RFP, valueAt+offRx, asm.R1, asm.DWord),
		asm.StoreMem(asm.RFP, valueAt+offTx, asm.R1, asm.DWord),
		asm.StoreMem(asm.RFP, valueAt+off, asm.R6, asm.DWord),
	}
	insns = append(insns, fillComm...)
	return append(insns,
		// bpf_map_update_elem(&counters, &key, &value, BPF_NOEXIST)
		asm.LoadMapPtr(asm.R1, 0).WithReference(mapName),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, stackKey),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, valueAt),
		asm.Mov.Imm(asm.R4, 1), // BPF_NOEXIST
		asm.FnMapUpdateElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		// another CPU inserted the same key first: look it up and add
		asm.LoadMapPtr(asm.R1, 0).WithReference(mapName),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, stackKey),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		xadd(asm.R0, asm.R6, off),

		asm.Mov.Imm(asm.R0, ret).WithSymbol("exit"),
		asm.Return(),
	)
}

// currentComm fills the value's name with bpf_get_current_comm.
var currentComm = asm.Instructions{
	asm.Mov.Reg(asm.R1, asm.RFP),
	asm.Add.Imm(asm.R1, valueAt+offComm),
	asm.Mov.Imm(asm.R2, commLen),
	asm.FnGetCurrentComm.Call(),
}

// program returns an fentry or fexit program. slot is the index of the u64
// in the trampoline context that holds the byte count (an argument for
// fentry, the return value for fexit). The count is a C int, so only its
// low 32 bits are meaningful, and zero or negative values (nothing moved,
// or an error code) are ignored.
func program(slot int, dir Direction) asm.Instructions {
	head := asm.Instructions{
		// R6 = byte count = ((u64 *)ctx)[slot]
		asm.LoadMem(asm.R6, asm.R1, int16(slot*8), asm.DWord),
		asm.JSLE.Imm32(asm.R6, 0, "exit"),
		asm.Mov.Reg32(asm.R6, asm.R6), // mov32 clears the upper half
		// key = tgid = bpf_get_current_pid_tgid() >> 32
		asm.FnGetCurrentPidTgid.Call(),
		asm.RSh.Imm(asm.R0, 32),
		asm.StoreMem(asm.RFP, stackKey, asm.R0, asm.Word),
	}
	return append(head, count(dir, currentComm, 0)...)
}
