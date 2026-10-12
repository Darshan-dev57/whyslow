package netbpf

import (
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/btf"
)

// Socket-owner accounting, for kernels that cannot run fentry/fexit.
//
// sock_create runs in the context of the process calling socket(), so it
// can record that process in the socket's local storage. The skb programs
// run for every packet a socket sends or receives (possibly in softirq
// context, where "current" means nothing), and charge the packet to the
// socket's recorded owner. Storage is cloned to sockets returned by
// accept(), so a server's connections belong to the server.
//
// Limits: sockets opened before whyslow started have no owner and are not
// counted; a socket passed to another process stays with its creator; the
// numbers are IP packet lengths (headers and retransmits included), not
// the bytes the application read or wrote.

const ownerMapName = "whyslow_owner"

// owner is the socket-local storage value.
const (
	ownerTgid = 0
	ownerComm = 8
	ownerLen  = ownerComm + commLen
)

// Offsets in the kernel's UAPI context structs (stable ABI).
const (
	bpfSockProtocol = 12  // struct bpf_sock.protocol
	skbLen          = 0   // struct __sk_buff.len
	skbSk           = 168 // struct __sk_buff.sk
)

const (
	ipprotoTCP = 6
	ipprotoUDP = 17

	skStorageGetFCreate = 1
	bpfFClone           = 1 << 9 // BPF_F_CLONE: copy storage to accept()ed sockets
	bpfFNoPrealloc      = 1 << 0
)

// ownerTypes describes the storage map to the kernel, which insists on BTF
// for socket storage maps.
func ownerTypes() (key, value btf.Type) {
	u32 := &btf.Int{Name: "unsigned int", Size: 4}
	char := &btf.Int{Name: "char", Size: 1, Encoding: btf.Char}
	val := &btf.Struct{Name: "whyslow_owner", Size: ownerLen, Members: []btf.Member{
		{Name: "tgid", Type: u32, Offset: btf.Bits(ownerTgid * 8)},
		{Name: "pad", Type: u32, Offset: btf.Bits(4 * 8)},
		{Name: "comm", Type: &btf.Array{Index: u32, Type: char, Nelems: commLen}, Offset: btf.Bits(ownerComm * 8)},
	}}
	return &btf.Int{Name: "int", Size: 4, Encoding: btf.Signed}, val
}

// sockCreateProgram records the calling process as the owner of new TCP
// and UDP sockets. It always allows the socket (returns 1).
func sockCreateProgram() asm.Instructions {
	return asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1),
		asm.LoadMem(asm.R2, asm.R6, bpfSockProtocol, asm.Word),
		asm.JEq.Imm(asm.R2, ipprotoTCP, "own"),
		asm.JNE.Imm(asm.R2, ipprotoUDP, "allow"),

		// R0 = bpf_sk_storage_get(&owners, sk, NULL, F_CREATE)
		asm.LoadMapPtr(asm.R1, 0).WithReference(ownerMapName).WithSymbol("own"),
		asm.Mov.Reg(asm.R2, asm.R6),
		asm.Mov.Imm(asm.R3, 0),
		asm.Mov.Imm(asm.R4, skStorageGetFCreate),
		asm.FnSkStorageGet.Call(),
		asm.JEq.Imm(asm.R0, 0, "allow"),
		asm.Mov.Reg(asm.R7, asm.R0),
		asm.FnGetCurrentPidTgid.Call(),
		asm.RSh.Imm(asm.R0, 32),
		asm.StoreMem(asm.R7, ownerTgid, asm.R0, asm.Word),
		asm.Mov.Reg(asm.R1, asm.R7),
		asm.Add.Imm(asm.R1, ownerComm),
		asm.Mov.Imm(asm.R2, commLen),
		asm.FnGetCurrentComm.Call(),

		asm.Mov.Imm(asm.R0, 1).WithSymbol("allow"),
		asm.Return(),
	}
}

// skbProgram charges a packet's length to its socket's owner. It always
// lets the packet through (returns 1).
func skbProgram(dir Direction) asm.Instructions {
	head := asm.Instructions{
		asm.Mov.Reg(asm.R9, asm.R1),
		asm.LoadMem(asm.R6, asm.R9, skbLen, asm.Word),
		asm.JEq.Imm(asm.R6, 0, "exit"),
		asm.LoadMem(asm.R1, asm.R9, skbSk, asm.DWord),
		asm.JEq.Imm(asm.R1, 0, "exit"),
		asm.FnSkFullsock.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		// R7 = bpf_sk_storage_get(&owners, sk, NULL, 0)
		asm.Mov.Reg(asm.R2, asm.R0),
		asm.LoadMapPtr(asm.R1, 0).WithReference(ownerMapName),
		asm.Mov.Imm(asm.R3, 0),
		asm.Mov.Imm(asm.R4, 0),
		asm.FnSkStorageGet.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		asm.Mov.Reg(asm.R7, asm.R0),
		asm.LoadMem(asm.R1, asm.R7, ownerTgid, asm.Word),
		asm.JEq.Imm(asm.R1, 0, "exit"),
		asm.StoreMem(asm.RFP, stackKey, asm.R1, asm.Word),
	}
	copyComm := asm.Instructions{
		asm.LoadMem(asm.R1, asm.R7, ownerComm, asm.DWord),
		asm.StoreMem(asm.RFP, valueAt+offComm, asm.R1, asm.DWord),
		asm.LoadMem(asm.R1, asm.R7, ownerComm+8, asm.DWord),
		asm.StoreMem(asm.RFP, valueAt+offComm+8, asm.R1, asm.DWord),
	}
	return append(head, count(dir, copyComm, 1)...)
}
