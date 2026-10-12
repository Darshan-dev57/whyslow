//go:build ebpf

package netbpf

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLoadAndCount loads the programs into the running kernel and checks
// that a curl download is charged to curl's PID, and the upload to this
// process (the server). It needs root:
//
//	go test -c -tags ebpf -o netbpf.test ./internal/netbpf && sudo ./netbpf.test -test.v
func TestLoadAndCount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("needs curl")
	}
	src, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer src.Close()
	t.Logf("mode %s, attached %v, skipped %v, fallback reason: %v", src.Mode(), src.Attached(), src.Skipped(), src.Fallback())

	const size = 5 << 20
	body := strings.Repeat("x", size)
	// Go listeners default to MPTCP since Go 1.24. Its subflows are kernel
	// sockets that socket-owner mode cannot attribute (see sockowner.go),
	// so the server uses plain TCP, like whyslow's own listener.
	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	ln, err := lc.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, body)
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	before, _ := src.Counters()
	self := uint32(os.Getpid())
	cmd := exec.Command(curl, "-s", "-o", "/dev/null", "--data-binary", "@-", srv.URL)
	cmd.Stdin = strings.NewReader(strings.Repeat("y", 1<<20)) // 1 MiB upload
	if err := cmd.Run(); err != nil {
		t.Fatalf("curl: %v", err)
	}
	pid := uint32(cmd.Process.Pid)

	// UDP from this process to itself: 200 datagrams of 1000 bytes.
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	conn, err := net.Dial("udp4", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1000)
	for i := 0; i < 200; i++ {
		conn.Write(buf)
		pc.SetReadDeadline(time.Now().Add(time.Second))
		pc.ReadFrom(buf)
	}
	conn.Close()

	after, err := src.Counters()
	if err != nil {
		t.Fatal(err)
	}
	c := after[pid]
	t.Logf("curl pid %d: rx %d tx %d comm %q", pid, c.Rx, c.Tx, c.Comm)
	t.Logf("this process (%d): rx %d tx %d (before: rx %d tx %d)", self, after[self].Rx, after[self].Tx, before[self].Rx, before[self].Tx)
	if c.Rx < size || c.Rx > 2*size {
		t.Errorf("curl rx = %d, want about %d", c.Rx, size)
	}
	if c.Tx < 1<<20 {
		t.Errorf("curl tx = %d, want at least 1 MiB", c.Tx)
	}
	if c.Comm != "curl" {
		t.Errorf("comm = %q, want curl", c.Comm)
	}
	srvTx := after[self].Tx - before[self].Tx
	if srvTx < size+200*1000 {
		t.Errorf("server+udp tx = %d, want at least %d", srvTx, size+200*1000)
	}
	if err := src.Forget([]uint32{pid}); err != nil {
		t.Error(err)
	}
	if again, _ := src.Counters(); again[pid].Rx != 0 {
		t.Error("Forget left the entry")
	}
}
