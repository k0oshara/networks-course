package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

const (
	icmpEchoReply    = 0
	icmpDestUnreach  = 3
	icmpEchoRequest  = 8
	icmpTimeExceeded = 11

	defaultProbes    = 3
	defaultMaxTTL    = 30
	defaultTimeoutMS = 2000
	payloadSize      = 32
	recvBufSize      = 4096
)

type replyKind int

const (
	replyNone replyKind = iota
	replyHop
	replyDone
	replyUnreach
)

func checksum(b []byte) uint16 {
	var sum uint32

	for len(b) > 1 {
		sum += uint32(binary.BigEndian.Uint16(b[:2]))
		b = b[2:]
	}

	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}

	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}

	return ^uint16(sum)
}

func makeICMPPacket(id, seq uint16) []byte {
	p := make([]byte, 8+payloadSize)

	p[0] = icmpEchoRequest
	p[1] = 0

	binary.BigEndian.PutUint16(p[4:6], id)
	binary.BigEndian.PutUint16(p[6:8], seq)

	for i := 8; i < len(p); i++ {
		p[i] = 'A'
	}

	csum := checksum(p)
	binary.BigEndian.PutUint16(p[2:4], csum)

	return p
}

func lookupName(ip string, cache map[string]string) string {
	if name, ok := cache[ip]; ok {
		return name
	}

	names, err := net.LookupAddr(ip)
	if err != nil || len(names) == 0 {
		cache[ip] = "-"
		return "-"
	}

	name := strings.TrimSuffix(names[0], ".")
	cache[ip] = name

	return name
}

func printReply(ip string, rtt time.Duration, cache map[string]string, mark string) {
	name := lookupName(ip, cache)
	ms := float64(rtt.Microseconds()) / 1000.0

	fmt.Printf(" %s (%s) %.3f ms%s", ip, name, ms, mark)
}

func embeddedEchoMatches(b []byte, id, seq uint16) bool {
	if len(b) < 20 {
		return false
	}

	ver := b[0] >> 4
	ihl := int(b[0]&0x0f) * 4

	if ver != 4 || ihl < 20 || len(b) < ihl+8 {
		return false
	}

	if b[9] != syscall.IPPROTO_ICMP {
		return false
	}

	icmp := b[ihl:]

	return icmp[0] == icmpEchoRequest &&
		binary.BigEndian.Uint16(icmp[4:6]) == id &&
		binary.BigEndian.Uint16(icmp[6:8]) == seq
}

func parseReply(b []byte, id, seq uint16) replyKind {
	if len(b) < 20 {
		return replyNone
	}

	ver := b[0] >> 4
	ihl := int(b[0]&0x0f) * 4

	if ver != 4 || ihl < 20 || len(b) < ihl+8 {
		return replyNone
	}

	if b[9] != syscall.IPPROTO_ICMP {
		return replyNone
	}

	icmp := b[ihl:]
	t := icmp[0]

	if t == icmpEchoReply &&
		binary.BigEndian.Uint16(icmp[4:6]) == id &&
		binary.BigEndian.Uint16(icmp[6:8]) == seq {
		return replyDone
	}

	if t != icmpTimeExceeded && t != icmpDestUnreach {
		return replyNone
	}

	inner := icmp[8:]

	if !embeddedEchoMatches(inner, id, seq) {
		return replyNone
	}

	if t == icmpTimeExceeded {
		return replyHop
	}

	return replyUnreach
}

func fdSet(fd int, set *syscall.FdSet) {
	i := fd / 64
	b := uint(fd % 64)

	set.Bits[i] |= int64(1) << b
}

func waitReply(fd int, id, seq uint16, timeout time.Duration, sentAt time.Time) (replyKind, string, time.Duration, error) {
	deadline := sentAt.Add(timeout)
	buf := make([]byte, recvBufSize)

	for {
		left := time.Until(deadline)
		if left <= 0 {
			return replyNone, "", 0, nil
		}

		var readfds syscall.FdSet
		fdSet(fd, &readfds)

		tv := syscall.NsecToTimeval(left.Nanoseconds())

		n, err := syscall.Select(fd+1, &readfds, nil, nil, &tv)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}

			return replyNone, "", 0, err
		}

		if n == 0 {
			return replyNone, "", 0, nil
		}

		nread, from, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}

			return replyNone, "", 0, err
		}

		kind := parseReply(buf[:nread], id, seq)
		if kind == replyNone {
			continue
		}

		sa, ok := from.(*syscall.SockaddrInet4)
		if !ok {
			continue
		}

		ip := net.IPv4(sa.Addr[0], sa.Addr[1], sa.Addr[2], sa.Addr[3]).String()
		rtt := time.Since(sentAt)

		return kind, ip, rtt, nil
	}
}

func resolveIPv4(host string) (net.IP, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}

	for _, ip := range ips {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4, nil
		}
	}

	return nil, fmt.Errorf("IPv4 address for %q not found", host)
}

func main() {
	probes := flag.Int("c", defaultProbes, "number of probes per TTL")
	maxTTL := flag.Int("m", defaultMaxTTL, "max TTL")
	timeoutMS := flag.Int("w", defaultTimeoutMS, "timeout in milliseconds")

	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s [-c probes] [-m max_ttl] [-w timeout_ms] host\n", os.Args[0])
		os.Exit(1)
	}

	if *probes <= 0 || *maxTTL <= 0 || *timeoutMS <= 0 {
		fmt.Fprintln(os.Stderr, "error: numeric parameters must be positive")
		os.Exit(1)
	}

	host := flag.Arg(0)

	dstIP, err := resolveIPv4(host)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve: %v\n", err)
		os.Exit(1)
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			fmt.Fprintf(os.Stderr, "error: raw socket requires root or CAP_NET_RAW\n")
			fmt.Fprintf(os.Stderr, "try: sudo %s ...\n", os.Args[0])
		} else {
			fmt.Fprintf(os.Stderr, "socket: %v\n", err)
		}

		os.Exit(1)
	}
	defer syscall.Close(fd)

	var dst syscall.SockaddrInet4
	copy(dst.Addr[:], dstIP)

	id := uint16(os.Getpid() & 0xffff)
	seq := uint16(0)

	fmt.Printf("trace to %s (%s), %d hops max, %d probes per hop\n",
		host, dstIP.String(), *maxTTL, *probes)

	timeout := time.Duration(*timeoutMS) * time.Millisecond
	nameCache := make(map[string]string)

	for ttl := 1; ttl <= *maxTTL; ttl++ {
		reached := false

		fmt.Printf("%2d ", ttl)

		for p := 0; p < *probes; p++ {
			seq++

			err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
			if err != nil {
				fmt.Fprintf(os.Stderr, "\nsetsockopt IP_TTL: %v\n", err)
				os.Exit(1)
			}

			packet := makeICMPPacket(id, seq)

			sentAt := time.Now()

			err = syscall.Sendto(fd, packet, 0, &dst)
			if err != nil {
				fmt.Fprintf(os.Stderr, "\nsendto: %v\n", err)
				os.Exit(1)
			}

			kind, ip, rtt, err := waitReply(fd, id, seq, timeout, sentAt)
			if err != nil {
				fmt.Fprintf(os.Stderr, "\nrecv: %v\n", err)
				os.Exit(1)
			}

			switch kind {
			case replyNone:
				fmt.Print(" *")

			case replyHop:
				printReply(ip, rtt, nameCache, "")

			case replyDone:
				printReply(ip, rtt, nameCache, "")
				reached = true

			case replyUnreach:
				printReply(ip, rtt, nameCache, " !")
				reached = true
			}
		}

		fmt.Println()

		if reached {
			break
		}
	}
}
