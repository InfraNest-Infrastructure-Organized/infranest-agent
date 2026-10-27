package collect

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
const udpHeader = "   sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops\n"

// row writes one line the way the kernel does; only the first four columns are read.
func row(i int, local, remote, state string) string {
	return fmt.Sprintf("%4d: %s %s %s 00000000:00000000 00:00000000 00000000   999        0 %d 1 0000000000000000 100 0 0 10 0\n",
		i, local, remote, state, 20000+i)
}

func table(header string, rows ...string) string {
	return header + strings.Join(rows, "")
}

const (
	v4Any = "00000000:0000"
	v6Any = "00000000000000000000000000000000:0000"
)

// A little-endian host (x86-64, arm64) — what nearly every server writes.
var leTables = map[string]string{
	"tcp": table(tcpHeader,
		row(0, "00000000:0016", v4Any, "0A"),            // 0.0.0.0:22
		row(1, "00000000:0016", v4Any, "0A"),            // 0.0.0.0:22 again: SO_REUSEPORT
		row(2, "0100007F:1538", v4Any, "0A"),            // 127.0.0.1:5432
		row(3, "3500007F:0035", v4Any, "0A"),            // 127.0.0.53:53
		row(4, "0A00000A:0CEA", v4Any, "0A"),            // 10.0.0.10:3306
		row(5, "0100007F:1538", "0100007F:D2F0", "01"),  // 127.0.0.1 → 5432
		row(6, "0100007F:1538", "0100007F:D2F2", "01"),  // the same peer again: one peer
		row(7, "0A00000A:0016", "0500000A:C350", "01"),  // 10.0.0.5 → 22, private
		row(8, "0A00000A:0016", "077100CB:C351", "01"),  // 203.0.113.7 → 22, public
		row(9, "0A00000A:0016", "01004064:C352", "01"),  // 100.64.0.1 → 22, CGNAT
		row(10, "0A00000A:9C40", "05710033:01BB", "01"), // outbound from :40000 — no listener, not a peer
		row(11, "0A00000A:0016", "090071C6:C353", "06"), // TIME_WAIT: not connected
		row(12, "0A00000A:20FB", "096433C6:C354", "01"), // 198.51.100.9 → 8443, whose listener is only in tcp6
	),
	"tcp6": table(tcpHeader,
		row(0, "00000000000000000000000000000000:0016", v6Any, "0A"), // [::]:22
		row(1, "00000000000000000000000001000000:1538", v6Any, "0A"), // [::1]:5432
		row(2, "00000000000000000000000000000000:0050", v6Any, "0A"), // [::]:80
		row(3, "00000000000000000000000000000000:20FB", v6Any, "0A"), // [::]:8443
		// ::ffff:10.0.0.10:80 ← ::ffff:203.0.113.7 — an IPv4 client of a dual-stack socket
		row(4, "0000000000000000FFFF00000A00000A:0050", "0000000000000000FFFF0000077100CB:C355", "01"),
		// the same IPv4 client on :22 through the v6 socket: already counted from tcp
		row(5, "0000000000000000FFFF00000A00000A:0016", "0000000000000000FFFF0000077100CB:C356", "01"),
		row(6, "B80D0120000000000000000001000000:0016", "B80D0120000000000000000002000000:C357", "01"), // 2001:db8::2, public
		row(7, "B80D0120000000000000000001000000:0016", "000000FD000000000000000005000000:C358", "01"), // fd00::5, ULA
		row(8, "B80D0120000000000000000001000000:0016", "000080FE000000000000000001000000:C359", "01"), // fe80::1, link-local
	),
	"udp": table(udpHeader,
		row(0, "3500007F:0035", v4Any, "07"),           // 127.0.0.53:53
		row(1, "00000000:0044", v4Any, "07"),           // 0.0.0.0:68
		row(2, "0A00000A:A1B2", "08080808:0035", "01"), // a connected client: not a listener
	),
	"udp6": table(udpHeader,
		row(0, "00000000000000000000000000000000:CA6C", v6Any, "07"), // [::]:51820
	),
}

func opener(tables map[string]string) func(string) (io.ReadCloser, error) {
	return func(name string) (io.ReadCloser, error) {
		body, ok := tables[name]
		if !ok {
			return nil, &fs.PathError{Op: "open", Path: "/proc/net/" + name, Err: fs.ErrNotExist}
		}
		return io.NopCloser(strings.NewReader(body)), nil
	}
}

func peers(loopback, private, public int) *PeerCounts {
	return &PeerCounts{Loopback: loopback, Private: private, Public: public}
}

func TestListeningFromTheKernelsTables(t *testing.T) {
	got, err := collectListening(opener(leTables), binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}

	want := &Listening{Ports: []ListeningPort{
		// Every address first, TCP before UDP, by port.
		{Protocol: "tcp", Port: 22, Bind: "all", Addresses: []string{"0.0.0.0", "::"}, Peers: peers(0, 4, 2)},
		{Protocol: "tcp", Port: 80, Bind: "all", Addresses: []string{"::"}, Peers: peers(0, 0, 1)},
		{Protocol: "tcp", Port: 8443, Bind: "all", Addresses: []string{"::"}, Peers: peers(0, 0, 1)},
		{Protocol: "udp", Port: 68, Bind: "all", Addresses: []string{"0.0.0.0"}},
		{Protocol: "udp", Port: 51820, Bind: "all", Addresses: []string{"::"}},
		{Protocol: "tcp", Port: 3306, Bind: "specific", Addresses: []string{"10.0.0.10"}, Peers: peers(0, 0, 0)},
		{Protocol: "tcp", Port: 53, Bind: "loopback", Addresses: []string{"127.0.0.53"}, Peers: peers(0, 0, 0)},
		{Protocol: "tcp", Port: 5432, Bind: "loopback", Addresses: []string{"127.0.0.1", "::1"}, Peers: peers(1, 0, 0)},
		{Protocol: "udp", Port: 53, Bind: "loopback", Addresses: []string{"127.0.0.53"}},
	}}

	if !reflect.DeepEqual(got, want) {
		a, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("got\n%s", a)
	}
}

// s390x and big-endian ppc64 print the same address the other way round. Same sockets, same answer.
func TestABigEndianHostReadsTheSame(t *testing.T) {
	be := map[string]string{
		"tcp": table(tcpHeader,
			row(0, "7F000001:1538", v4Any, "0A"),
			row(1, "7F000001:1538", "7F000001:D2F0", "01"),
			row(2, "0A00000A:1538", "CB007107:D2F1", "01"),
		),
		"tcp6": table(tcpHeader,
			row(0, "00000000000000000000000000000001:1538", v6Any, "0A"),
			row(1, "00000000000000000000FFFF0A00000A:1538", "00000000000000000000FFFFCB007107:D2F2", "01"),
		),
	}
	le := map[string]string{
		"tcp": table(tcpHeader,
			row(0, "0100007F:1538", v4Any, "0A"),
			row(1, "0100007F:1538", "0100007F:D2F0", "01"),
			row(2, "0A00000A:1538", "077100CB:D2F1", "01"),
		),
		"tcp6": table(tcpHeader,
			row(0, "00000000000000000000000001000000:1538", v6Any, "0A"),
			row(1, "0000000000000000FFFF00000A00000A:1538", "0000000000000000FFFF0000077100CB:D2F2", "01"),
		),
	}

	fromBE, err := collectListening(opener(be), binary.BigEndian)
	if err != nil {
		t.Fatal(err)
	}
	fromLE, err := collectListening(opener(le), binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromBE, fromLE) {
		t.Fatalf("big-endian %+v\nlittle-endian %+v", fromBE, fromLE)
	}
	p := fromBE.Ports[0]
	if p.Port != 5432 || p.Bind != "loopback" || !reflect.DeepEqual(p.Addresses, []string{"127.0.0.1", "::1"}) ||
		*p.Peers != (PeerCounts{Loopback: 1, Public: 1}) {
		t.Fatalf("got %+v %+v", p, *p.Peers)
	}
}

func TestProcAddressesInBothByteOrders(t *testing.T) {
	cases := []struct {
		hex   string
		order binary.ByteOrder
		want  string
	}{
		{"0100007F", binary.LittleEndian, "127.0.0.1"},
		{"7F000001", binary.BigEndian, "127.0.0.1"},
		{"00000000000000000000000001000000", binary.LittleEndian, "::1"},
		{"00000000000000000000000000000001", binary.BigEndian, "::1"},
		{"B80D0120000000000000000001000000", binary.LittleEndian, "2001:db8::1"},
		{"20010DB8000000000000000000000001", binary.BigEndian, "2001:db8::1"},
		// IPv4-mapped is unmapped, so a dual-stack socket's IPv4 client is one address, not two.
		{"0000000000000000FFFF0000077100CB", binary.LittleEndian, "203.0.113.7"},
		{"00000000000000000000FFFFCB007107", binary.BigEndian, "203.0.113.7"},
		{"0000000000000000ffff0000077100cb", binary.LittleEndian, "203.0.113.7"}, // lower case is still hex
	}
	for _, c := range cases {
		got, err := decodeProcAddr(c.hex, c.order)
		if err != nil || got.String() != c.want {
			t.Errorf("%s: got %v, %v; want %s", c.hex, got, err, c.want)
		}
	}

	for _, bad := range []string{"", "0100007", "0100007G", "000000000000000000000000010000"} {
		if _, err := decodeProcAddr(bad, binary.LittleEndian); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
}

func TestPeerClasses(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1":       "loopback",
		"127.8.9.10":      "loopback",
		"::1":             "loopback",
		"10.1.2.3":        "private",
		"172.16.0.1":      "private",
		"172.31.255.1":    "private",
		"192.168.1.1":     "private",
		"100.64.0.1":      "private", // CGNAT, Tailscale
		"100.127.255.254": "private",
		"169.254.1.1":     "private",
		"fd12::1":         "private",
		"fe80::1":         "private",
		"172.32.0.1":      "public",
		"100.128.0.1":     "public",
		"8.8.8.8":         "public",
		"2a01:4f8::1":     "public",
	} {
		if got := peerClass(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: got %s, want %s", addr, got, want)
		}
	}
}

// A kernel booted with ipv6.disable=1 has no tcp6 or udp6. That is an answer, not a failure.
func TestAKernelWithoutIPv6(t *testing.T) {
	got, err := collectListening(opener(map[string]string{
		"tcp": table(tcpHeader, row(0, "00000000:0016", v4Any, "0A")),
		"udp": table(udpHeader),
	}), binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Ports) != 1 || got.Ports[0].Port != 22 {
		t.Fatalf("got %+v", got)
	}
}

func TestNothingListeningIsAnEmptyListNotNull(t *testing.T) {
	got, err := collectListening(opener(map[string]string{"tcp": tcpHeader}), binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"ports":[],"more":0,"partial":false}` {
		t.Fatalf("got %s", b)
	}
}

// Without /proc/net/tcp there is no answer, and "nothing listens" is a claim the receiver acts on.
func TestNoAnswerIsAFailureNotAnEmptyList(t *testing.T) {
	if _, err := collectListening(opener(map[string]string{}), binary.LittleEndian); err == nil {
		t.Fatal("a missing /proc/net/tcp reported nothing listening")
	}

	refused := func(name string) (io.ReadCloser, error) {
		if name == "tcp6" {
			return nil, &fs.PathError{Op: "open", Path: "/proc/net/tcp6", Err: fs.ErrPermission}
		}
		return opener(leTables)(name)
	}
	if _, err := collectListening(refused, binary.LittleEndian); err == nil {
		t.Fatal("a tcp6 that exists and refused us was read as a kernel without IPv6")
	}

	for name, body := range map[string]string{
		"no header":  row(0, "00000000:0016", v4Any, "0A"),
		"empty":      "",
		"short row":  tcpHeader + "   0: 00000000:0016\n",
		"bad port":   tcpHeader + row(0, "00000000:16", v4Any, "0A"),
		"bad state":  tcpHeader + row(0, "00000000:0016", v4Any, "A"),
		"bad number": tcpHeader + "   x 00000000:0016 00000000:0000 0A\n",
	} {
		if _, err := collectListening(opener(map[string]string{"tcp": body}), binary.LittleEndian); !errors.Is(err, errSocketTableFormat) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// Over the cap, the exposed survive and the rest is counted rather than silently missing.
func TestTheCapKeepsWhatIsExposedAndCountsTheRest(t *testing.T) {
	rows := []string{}
	for i := 0; i < 150; i++ {
		rows = append(rows, row(i, fmt.Sprintf("0100007F:%04X", 10000+i), v4Any, "0A")) // loopback
	}
	rows = append(rows, row(150, "00000000:0016", v4Any, "0A")) // the one that matters, listed last
	got, err := collectListening(opener(map[string]string{"tcp": table(tcpHeader, rows...)}), binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Ports) != maxListeningPorts || got.More != 51 {
		t.Fatalf("got %d ports, more %d", len(got.Ports), got.More)
	}
	if got.Ports[0].Port != 22 || got.Ports[0].Bind != "all" {
		t.Fatalf("the cap dropped the port bound to every address: %+v", got.Ports[0])
	}
}

func TestAddressesPerPortAreCapped(t *testing.T) {
	rows := []string{row(0, "00000000:01BB", v4Any, "0A")}
	for i := 1; i <= 12; i++ {
		rows = append(rows, row(i, fmt.Sprintf("%02X00000A:01BB", i), v4Any, "0A")) // 10.0.0.i:443
	}
	got, err := collectListening(opener(map[string]string{"tcp": table(tcpHeader, rows...)}), binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	p := got.Ports[0]
	if len(p.Addresses) != maxListeningAddresses || p.Addresses[0] != "0.0.0.0" || p.Bind != "all" {
		t.Fatalf("got %+v", p)
	}
}

// A machine with more connections than the agent will hold: the listeners are still all there — the
// kernel lists them first — and the counts say they are lower bounds.
func TestManyConnectionsMakeTheCountsALowerBound(t *testing.T) {
	var b strings.Builder
	b.WriteString(tcpHeader)
	b.WriteString(row(0, "00000000:01BB", v4Any, "0A"))
	for i := 0; i < maxPeerPairs+10; i++ {
		// 203.0.x.y, every one a different peer
		b.WriteString(row(i+1, "0A00000A:01BB", fmt.Sprintf("%02X%02X%02XCB:C350", i&0xff, (i>>8)&0xff, (i>>16)&0xff), "01"))
	}
	got, err := collectListening(opener(map[string]string{"tcp": b.String()}), binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Partial || got.Ports[0].Peers.Public != maxPeerPairs {
		t.Fatalf("partial %v, public %d", got.Partial, got.Ports[0].Peers.Public)
	}
}

func TestALongTableIsReadOnlyThisFar(t *testing.T) {
	var b strings.Builder
	b.WriteString(tcpHeader)
	b.WriteString(row(0, "00000000:0016", v4Any, "0A"))
	for i := 0; i < maxSocketRows+5; i++ {
		b.WriteString(row(i+1, "0A00000A:0016", "0500000A:C350", "06"))
	}
	got, err := collectListening(opener(map[string]string{"tcp": b.String()}), binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Partial || len(got.Ports) != 1 {
		t.Fatalf("got %+v", got)
	}
}

// The promise in PAYLOAD.md: a peer's address never leaves the machine.
func TestNoPeerAddressIsSent(t *testing.T) {
	got, err := collectListening(opener(leTables), binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	for _, peer := range []string{"10.0.0.5", "203.0.113.7", "100.64.0.1", "198.51.100.9", "2001:db8::2", "fd00::5", "fe80::1", "8.8.8.8", "51.0.113.5"} {
		if strings.Contains(string(b), peer) {
			t.Errorf("%s is in the payload: %s", peer, b)
		}
	}
}

// What the cap costs on the wire, so PAYLOAD.md's size budget can be checked against it.
func TestTheWorstCaseIsBounded(t *testing.T) {
	l := &Listening{More: 1 << 20, Partial: true}
	for i := 0; i < maxListeningPorts; i++ {
		p := ListeningPort{Protocol: "tcp", Port: 65535 - i, Bind: "specific", Peers: peers(1<<20, 1<<20, 1<<20)}
		for j := 0; j < maxListeningAddresses; j++ {
			p.Addresses = append(p.Addresses, "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")
		}
		l.Ports = append(l.Ports, p)
	}
	b, _ := json.Marshal(l)
	if len(b) > 50_000 {
		t.Fatalf("worst case is %d bytes", len(b))
	}
	t.Logf("worst-case listening: %d bytes", len(b))
}
