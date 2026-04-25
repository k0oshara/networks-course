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
	fmt.Println("server: go run task_A.go server -addr :9000 -out recv.bin -loss 30 -grace 5s")
	fmt.Println("client: go run task_A.go client -addr 127.0.0.1:9000 -in send.bin -chunk 256 -timeout 700ms -loss 30")
}

func srv(a []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	ad := fs.String("addr", ":9000", "")
	op := fs.String("out", "recv.bin", "")
	lp := fs.Int("loss", 30, "")
	gs := fs.String("grace", "5s", "")
	if err := fs.Parse(a); err != nil {
		return err
	}
	if *lp < 0 || *lp > 100 {
		return errors.New("loss must be in range 0..100")
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
	f, err := os.OpenFile(*op, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("output file already exists: %s", *op)
		}
		return fmt.Errorf("create output: %w", err)
	}
	defer f.Close()

	fmt.Printf("server listen=%s out=%s loss=%d%% grace=%s\n", *ad, *op, *lp, gr)

	b := make([]byte, maxUDPPayload)
	var ex_seq byte
	var ra *net.UDPAddr
	var n0 int64
	ok := false

	for {
		if ok {
			if err := c.SetReadDeadline(time.Now().Add(gr)); err != nil {
				return fmt.Errorf("set deadline: %w", err)
			}
		}

		n, a0, err := c.ReadFromUDP(b)
		if err != nil {
			if ok {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					fmt.Printf("server done bytes=%d file=%s\n", n0, *op)
					return nil
				}
			}
			return fmt.Errorf("read udp: %w", err)
		}

		ra = a0
		fr, err := df(b[:n])
		if err != nil {
			fmt.Printf("server bad frame from=%s err=%v\n", ra, err)
			continue
		}

		if fr.s == ex_seq && !ok {
			n1, err := f.Write(fr.d)
			if err != nil {
				return fmt.Errorf("write output: %w", err)
			}
			if n1 != len(fr.d) {
				return io.ErrShortWrite
			}

			n0 += int64(n1)
			fmt.Printf("server recv seq=%d bytes=%d last=%v from=%s\n", fr.s, len(fr.d), fr.l, ra)

			if err := sa(c, ra, fr.s, *lp); err != nil {
				return err
			}

			ex_seq ^= 1

			if fr.l {
				ok = true
				fmt.Printf("server file received bytes=%d waiting=%s\n", n0, gr)
			}

			continue
		}

		if fr.s == (ex_seq ^ 1) {
			fmt.Printf("server dup seq=%d from=%s\n", fr.s, ra)

			if err := sa(c, ra, fr.s, *lp); err != nil {
				return err
			}

			continue
		}

		fmt.Printf("server skip seq=%d exp=%d from=%s\n", fr.s, ex_seq, ra)
	}
}

func cli(a []string) error {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	ad := fs.String("addr", "127.0.0.1:9000", "")
	ip := fs.String("in", "", "")
	ch := fs.Int("chunk", 256, "")
	ts := fs.String("timeout", "700ms", "")
	lp := fs.Int("loss", 30, "")
	if err := fs.Parse(a); err != nil {
		return err
	}
	if *ip == "" {
		return errors.New("missing -in")
	}
	if *ch <= 0 {
		return errors.New("chunk must be > 0")
	}
	if *ch > maxDataSize {
		return fmt.Errorf("chunk too large: max is %d", maxDataSize)
	}
	if *lp < 0 || *lp > 100 {
		return errors.New("loss must be in range 0..100")
	}
	to, err := time.ParseDuration(*ts)
	if err != nil {
		return fmt.Errorf("bad timeout: %w", err)
	}
	if to <= 0 {
		return errors.New("timeout must be > 0")
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

	f, err := os.Open(*ip)
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

	fmt.Printf("client addr=%s in=%s size=%d chunk=%d timeout=%s loss=%d%%\n", *ad, *ip, sz, *ch, to, *lp)

	db := make([]byte, *ch)
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
		p := ef(frm{s: s, l: l0, d: append([]byte(nil), db[:n1]...)})

		for {
			fmt.Printf("client send seq=%d bytes=%d last=%v\n", s, n1, l0)

			if err := sd(c, p, *lp, "client data"); err != nil {
				return err
			}

			if err := c.SetReadDeadline(time.Now().Add(to)); err != nil {
				return fmt.Errorf("set deadline: %w", err)
			}

			ok := false

			for {
				n2, err := c.Read(ab)
				if err != nil {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						fmt.Printf("client timeout seq=%d\n", s)
						break
					}
					return fmt.Errorf("read ack: %w", err)
				}

				a0, err := da(ab[:n2])
				if err != nil {
					fmt.Printf("client bad ack err=%v\n", err)
					continue
				}

				if a0 != s {
					fmt.Printf("client skip ack=%d exp=%d\n", a0, s)
					continue
				}

				fmt.Printf("client ack=%d\n", a0)
				ok = true
				break
			}

			if ok {
				s ^= 1
				break
			}
		}
	}

	fmt.Printf("client done bytes=%d\n", sz)
	return nil
}

func ef(f frm) []byte {
	b := make([]byte, frameHeaderSize+len(f.d))
	b[0] = 'D'
	b[1] = f.s
	if f.l {
		b[2] = 1
	}
	binary.BigEndian.PutUint32(b[3:7], uint32(len(f.d)))
	copy(b[7:], f.d)
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

	n := int(binary.BigEndian.Uint32(b[3:7]))
	if n > maxDataSize {
		return frm{}, errors.New("data too large")
	}
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
		fmt.Printf("drop ack seq=%d to=%s\n", s, a)
		return nil
	}

	n, err := c.WriteToUDP(p, a)
	if err != nil {
		return fmt.Errorf("send ack: %w", err)
	}
	if n != len(p) {
		return io.ErrShortWrite
	}

	fmt.Printf("send ack seq=%d to=%s\n", s, a)
	return nil
}

func sd(c *net.UDPConn, b []byte, l int, k string) error {
	if rr.Intn(100) < l {
		fmt.Printf("drop %s\n", k)
		return nil
	}

	n, err := c.Write(b)
	if err != nil {
		return fmt.Errorf("send data: %w", err)
	}
	if n != len(b) {
		return io.ErrShortWrite
	}

	return nil
}
