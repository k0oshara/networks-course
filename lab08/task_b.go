package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"time"
)

const (
	frameHeaderSize = 7
	maxUDPPayload   = 65507
	maxDataSize     = maxUDPPayload - frameHeaderSize
)

type frm struct {
	s byte
	l bool
	d []byte
}

var rr = rand.New(rand.NewSource(time.Now().UnixNano()))

func main() {
	if len(os.Args) < 2 {
		help()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "server":
		if err := srv(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "client":
		if err := cli(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		help()
		os.Exit(1)
	}
}

func help() {
	fmt.Println("client -> server:")
	fmt.Println("  server: go run task_B.go server -dir recv -addr :9000 -out recv.bin -loss 30 -grace 5s")
	fmt.Println("  client: go run task_B.go client -dir send -addr 127.0.0.1:9000 -in send.bin -chunk 512 -timeout 700ms -loss 30")
	fmt.Println()
	fmt.Println("server -> client:")
	fmt.Println("  server: go run task_B.go server -dir send -addr :9000 -in server_send.bin -chunk 512 -timeout 700ms -loss 30")
	fmt.Println("  client: go run task_B.go client -dir recv -addr 127.0.0.1:9000 -out client_recv.bin -loss 30 -grace 5s")
}

func srv(a []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	dir := fs.String("dir", "recv", "")
	ad := fs.String("addr", ":9000", "")
	ip := fs.String("in", "", "")
	op := fs.String("out", "recv.bin", "")
	ch := fs.Int("chunk", 256, "")
	ts := fs.String("timeout", "700ms", "")
	lp := fs.Int("loss", 30, "")
	gs := fs.String("grace", "5s", "")

	if err := fs.Parse(a); err != nil {
		return err
	}

	if *lp < 0 || *lp > 100 {
		return errors.New("loss must be in range 0..100")
	}
	if *ch <= 0 {
		return errors.New("chunk must be > 0")
	}
	if *ch > maxDataSize {
		return fmt.Errorf("chunk too large: max is %d", maxDataSize)
	}

	to, err := time.ParseDuration(*ts)
	if err != nil {
		return fmt.Errorf("bad timeout: %w", err)
	}
	if to <= 0 {
		return errors.New("timeout must be > 0")
	}

	gr, err := time.ParseDuration(*gs)
	if err != nil {
		return fmt.Errorf("bad grace: %w", err)
	}
	if gr <= 0 {
		return errors.New("grace must be > 0")
	}

	ua, err := net.ResolveUDPAddr("udp", *ad)
	if err != nil {
		return fmt.Errorf("resolve addr: %w", err)
	}

	c, err := net.ListenUDP("udp", ua)
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	defer c.Close()

	switch *dir {
	case "recv":
		return recvFile(c, nil, *op, *lp, gr, true, "server")

	case "send":
		if *ip == "" {
			return errors.New("missing -in")
		}

		ra, err := waitHello(c)
		if err != nil {
			return err
		}

		return sendFile(c, ra, *ip, *ch, to, *lp, true, "server")

	default:
		return errors.New("dir must be send or recv")
	}
}

func cli(a []string) error {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	dir := fs.String("dir", "send", "")
	ad := fs.String("addr", "127.0.0.1:9000", "")
	ip := fs.String("in", "", "")
	op := fs.String("out", "client_recv.bin", "")
	ch := fs.Int("chunk", 256, "")
	ts := fs.String("timeout", "700ms", "")
	lp := fs.Int("loss", 30, "")
	gs := fs.String("grace", "5s", "")

	if err := fs.Parse(a); err != nil {
		return err
	}

	if *lp < 0 || *lp > 100 {
		return errors.New("loss must be in range 0..100")
	}
	if *ch <= 0 {
		return errors.New("chunk must be > 0")
	}
	if *ch > maxDataSize {
		return fmt.Errorf("chunk too large: max is %d", maxDataSize)
	}

	to, err := time.ParseDuration(*ts)
	if err != nil {
		return fmt.Errorf("bad timeout: %w", err)
	}
	if to <= 0 {
		return errors.New("timeout must be > 0")
	}

	gr, err := time.ParseDuration(*gs)
	if err != nil {
		return fmt.Errorf("bad grace: %w", err)
	}
	if gr <= 0 {
		return errors.New("grace must be > 0")
	}

	ua, err := net.ResolveUDPAddr("udp", *ad)
	if err != nil {
		return fmt.Errorf("resolve addr: %w", err)
	}

	c, err := net.DialUDP("udp", nil, ua)
	if err != nil {
		return fmt.Errorf("dial udp: %w", err)
	}
	defer c.Close()

	switch *dir {
	case "send":
		if *ip == "" {
			return errors.New("missing -in")
		}

		return sendFile(c, nil, *ip, *ch, to, *lp, false, "client")

	case "recv":
		if err := sendHello(c); err != nil {
			return err
		}

		return recvFile(c, nil, *op, *lp, gr, false, "client")

	default:
		return errors.New("dir must be send or recv")
	}
}

func sendFile(c *net.UDPConn, peer *net.UDPAddr, ip string, ch int, to time.Duration, lp int, from bool, who string) error {
	f, err := os.Open(ip)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat input: %w", err)
	}

	sz := st.Size()
	if sz == 0 {
		return errors.New("input file is empty")
	}

	fmt.Printf("%s send file=%s size=%d chunk=%d timeout=%s loss=%d%%\n", who, ip, sz, ch, to, lp)

	db := make([]byte, ch)
	ab := make([]byte, 2)
	var s byte
	var n0 int64

	for n0 < sz {
		n1, err := f.Read(db)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read input: %w", err)
		}
		if n1 == 0 {
			break
		}

		n0 += int64(n1)
		l0 := n0 == sz
		p := ef(frm{s: s, l: l0, d: db[:n1]})

		for {
			fmt.Printf("%s send seq=%d bytes=%d last=%v\n", who, s, n1, l0)

			if err := sd(c, peer, p, lp, who+" data"); err != nil {
				return err
			}

			if err := c.SetReadDeadline(time.Now().Add(to)); err != nil {
				return fmt.Errorf("set deadline: %w", err)
			}

			ok := false

			for {
				n2, _, err := readData(c, ab, peer, from)
				if err != nil {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						fmt.Printf("%s timeout seq=%d\n", who, s)
						break
					}
					return fmt.Errorf("read ack: %w", err)
				}

				a0, err := da(ab[:n2])
				if err != nil {
					fmt.Printf("%s bad ack err=%v\n", who, err)
					continue
				}

				if a0 != s {
					fmt.Printf("%s skip ack=%d exp=%d\n", who, a0, s)
					continue
				}

				fmt.Printf("%s ack=%d\n", who, a0)
				ok = true
				break
			}

			if ok {
				s ^= 1
				break
			}
		}
	}

	fmt.Printf("%s done sent bytes=%d\n", who, sz)
	return nil
}

func recvFile(c *net.UDPConn, peer *net.UDPAddr, op string, lp int, gr time.Duration, from bool, who string) error {
	f, err := os.OpenFile(op, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("output file already exists: %s", op)
		}
		return fmt.Errorf("create output: %w", err)
	}
	defer f.Close()

	fmt.Printf("%s recv out=%s loss=%d%% grace=%s\n", who, op, lp, gr)

	b := make([]byte, maxUDPPayload)
	var ex byte
	var n0 int64
	ok := false

	for {
		if ok {
			if err := c.SetReadDeadline(time.Now().Add(gr)); err != nil {
				return fmt.Errorf("set deadline: %w", err)
			}
		}

		n, a0, err := readData(c, b, peer, from)
		if err != nil {
			if ok {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					fmt.Printf("%s done bytes=%d file=%s\n", who, n0, op)
					return nil
				}
			}
			return fmt.Errorf("read udp: %w", err)
		}

		fr, err := df(b[:n])
		if err != nil {
			fmt.Printf("%s bad frame from=%v err=%v\n", who, a0, err)
			continue
		}

		if from && peer == nil {
			peer = a0
		}

		if fr.s == ex && !ok {
			n1, err := f.Write(fr.d)
			if err != nil {
				return fmt.Errorf("write output: %w", err)
			}
			if n1 != len(fr.d) {
				return io.ErrShortWrite
			}

			n0 += int64(n1)
			fmt.Printf("%s recv seq=%d bytes=%d last=%v from=%v\n", who, fr.s, len(fr.d), fr.l, a0)

			if err := sa(c, a0, fr.s, lp); err != nil {
				return err
			}

			ex ^= 1

			if fr.l {
				ok = true
				fmt.Printf("%s file received bytes=%d waiting=%s\n", who, n0, gr)
			}

			continue
		}

		if fr.s == (ex ^ 1) {
			fmt.Printf("%s dup seq=%d from=%v\n", who, fr.s, a0)

			if err := sa(c, a0, fr.s, lp); err != nil {
				return err
			}

			continue
		}

		fmt.Printf("%s skip seq=%d exp=%d from=%v\n", who, fr.s, ex, a0)
	}
}

func waitHello(c *net.UDPConn) (*net.UDPAddr, error) {
	b := make([]byte, 16)

	fmt.Println("server waiting hello from client")

	for {
		n, a, err := c.ReadFromUDP(b)
		if err != nil {
			return nil, fmt.Errorf("read hello: %w", err)
		}

		if n == 1 && b[0] == 'H' {
			fmt.Printf("server hello from=%s\n", a)
			return a, nil
		}

		fmt.Printf("server skip non-hello from=%s\n", a)
	}
}

func sendHello(c *net.UDPConn) error {
	p := []byte{'H'}

	n, err := c.Write(p)
	if err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	if n != len(p) {
		return io.ErrShortWrite
	}

	fmt.Println("client hello sent")
	return nil
}

func readData(c *net.UDPConn, b []byte, peer *net.UDPAddr, useReadFrom bool) (int, *net.UDPAddr, error) {
	if !useReadFrom {
		n, err := c.Read(b)
		return n, nil, err
	}

	for {
		n, a, err := c.ReadFromUDP(b)
		if err != nil {
			return 0, nil, err
		}

		if peer != nil && !sameAddr(a, peer) {
			fmt.Printf("skip packet from=%s want=%s\n", a, peer)
			continue
		}

		return n, a, nil
	}
}

func ef(f frm) []byte {
	b := make([]byte, frameHeaderSize+len(f.d))
	b[0] = 'D'
	b[1] = f.s
	if f.l {
		b[2] = 1
	}
	binary.BigEndian.PutUint32(b[3:7], uint32(len(f.d)))
	copy(b[frameHeaderSize:], f.d)
	return b
}

func df(b []byte) (frm, error) {
	if len(b) < frameHeaderSize {
		return frm{}, errors.New("short data frame")
	}
	if b[0] != 'D' {
		return frm{}, errors.New("bad data type")
	}
	if b[1] > 1 {
		return frm{}, errors.New("bad data seq")
	}
	if b[2] != 0 && b[2] != 1 {
		return frm{}, errors.New("bad last flag")
	}

	n0 := binary.BigEndian.Uint32(b[3:7])
	if n0 > uint32(maxDataSize) {
		return frm{}, errors.New("data too large")
	}

	n := int(n0)
	if len(b) != frameHeaderSize+n {
		return frm{}, errors.New("bad data size")
	}

	d := make([]byte, n)
	copy(d, b[frameHeaderSize:])

	return frm{s: b[1], l: b[2] == 1, d: d}, nil
}

func ea(s byte) []byte {
	return []byte{'A', s}
}

func da(b []byte) (byte, error) {
	if len(b) != 2 {
		return 0, errors.New("bad ack size")
	}
	if b[0] != 'A' {
		return 0, errors.New("bad ack type")
	}
	if b[1] > 1 {
		return 0, errors.New("bad ack seq")
	}

	return b[1], nil
}

func sa(c *net.UDPConn, a *net.UDPAddr, s byte, l int) error {
	p := ea(s)

	if rr.Intn(100) < l {
		fmt.Printf("drop ack seq=%d to=%v\n", s, a)
		return nil
	}

	n, err := writePkt(c, a, p)
	if err != nil {
		return fmt.Errorf("send ack: %w", err)
	}
	if n != len(p) {
		return io.ErrShortWrite
	}

	fmt.Printf("send ack seq=%d to=%v\n", s, a)
	return nil
}

func sd(c *net.UDPConn, a *net.UDPAddr, b []byte, l int, k string) error {
	if rr.Intn(100) < l {
		fmt.Printf("drop %s\n", k)
		return nil
	}

	n, err := writePkt(c, a, b)
	if err != nil {
		return fmt.Errorf("send data: %w", err)
	}
	if n != len(b) {
		return io.ErrShortWrite
	}

	return nil
}

func writePkt(c *net.UDPConn, a *net.UDPAddr, b []byte) (int, error) {
	if a != nil {
		return c.WriteToUDP(b, a)
	}

	return c.Write(b)
}

func sameAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Port == b.Port && a.Zone == b.Zone && a.IP.Equal(b.IP)
}
