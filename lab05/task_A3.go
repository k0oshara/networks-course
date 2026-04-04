package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type cli struct {
	r *bufio.Reader
	w *bufio.Writer
	c net.Conn
}

func main() {
	to := flag.String("to", "", "recipient email address")
	subj := flag.String("subject", "SMTP image test", "email subject")
	body := flag.String("body", "Email with image attachment.", "message body")
	bodyPath := flag.String("body-file", "", "path to a file with message body")
	host := flag.String("smtp-host", "", "SMTP server host")
	port := flag.Int("smtp-port", 25, "SMTP server port")
	user := flag.String("smtp-user", "", "SMTP username")
	pass := flag.String("smtp-pass", "", "SMTP password")
	from := flag.String("from", "", "sender email address")
	name := flag.String("name", "localhost", "client name for EHLO/HELO")
	useTLS := flag.Bool("starttls", true, "use STARTTLS when server supports it")
	filePath := flag.String("file", "", "path to image file")

	flag.Parse()

	if err := chkReq(*to, *host, *user, *pass, *from, *filePath); err != nil {
		die("argument error: %v", err)
	}

	msgBody, err := readBody(*body, *bodyPath)
	if err != nil {
		die("failed to read message body: %v", err)
	}
	if strings.TrimSpace(msgBody) == "" {
		die("message body is empty: use -body or -body-file")
	}

	data, err := os.ReadFile(*filePath)
	if err != nil {
		die("failed to read image file: %v", err)
	}

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	c, err := dial(addr)
	if err != nil {
		die("failed to connect: %v", err)
	}
	defer c.c.Close()

	if _, _, err := c.read(); err != nil {
		die("failed to read greeting: %v", err)
	}

	ext, err := hello(c, *name)
	if err != nil {
		die("EHLO failed: %v", err)
	}

	if *useTLS && hasExt(ext, "STARTTLS") {
		if err := c.cmd(220, "STARTTLS"); err != nil {
			die("STARTTLS failed: %v", err)
		}
		if err := wrapTLS(c, *host); err != nil {
			die("TLS handshake failed: %v", err)
		}
		ext, err = hello(c, *name)
		if err != nil {
			die("EHLO after STARTTLS failed: %v", err)
		}
	}

	if *user != "" {
		if err := authPlain(c, *user, *pass); err != nil {
			die("AUTH failed: %v", err)
		}
	}

	if err := c.cmd(250, "MAIL FROM:<%s>", *from); err != nil {
		die("MAIL FROM failed: %v", err)
	}
	if err := c.cmd(250, "RCPT TO:<%s>", *to); err != nil {
		die("RCPT TO failed: %v", err)
	}
	if err := c.cmd(354, "DATA"); err != nil {
		die("DATA failed: %v", err)
	}

	msg := buildMsg(*from, *to, *subj, msgBody, *filePath, data)
	if err := c.data(msg); err != nil {
		die("message transfer failed: %v", err)
	}
	if err := c.cmd(221, "QUIT"); err != nil {
		die("QUIT failed: %v", err)
	}

	fmt.Printf("Email sent to %s\n", *to)
}

func chkReq(to, host, user, pass, from, filePath string) error {
	switch {
	case strings.TrimSpace(to) == "":
		return fmt.Errorf("missing required flag -to")
	case strings.TrimSpace(host) == "":
		return fmt.Errorf("missing required flag -smtp-host")
	case strings.TrimSpace(user) != "" && strings.TrimSpace(pass) == "":
		return fmt.Errorf("missing SMTP password: use -smtp-pass")
	case strings.TrimSpace(from) == "":
		return fmt.Errorf("missing sender address")
	case strings.TrimSpace(filePath) == "":
		return fmt.Errorf("missing required flag -file")
	default:
		return nil
	}
}

func readBody(body, bodyPath string) (string, error) {
	if strings.TrimSpace(body) != "" {
		return body, nil
	}
	if strings.TrimSpace(bodyPath) == "" {
		return "", nil
	}

	data, err := os.ReadFile(bodyPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func dial(addr string) (*cli, error) {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, err
	}

	return &cli{
		r: bufio.NewReader(conn),
		w: bufio.NewWriter(conn),
		c: conn,
	}, nil
}

func hello(c *cli, name string) ([]string, error) {
	lines, err := c.cmdLines(250, "EHLO %s", name)
	if err == nil {
		return lines, nil
	}
	if _, err := c.cmdLines(250, "HELO %s", name); err != nil {
		return nil, err
	}
	return nil, nil
}

func authPlain(c *cli, user, pass string) error {
	raw := "\x00" + user + "\x00" + pass
	enc := base64.StdEncoding.EncodeToString([]byte(raw))
	return c.cmd(235, "AUTH PLAIN %s", enc)
}

func wrapTLS(c *cli, host string) error {
	tlsConn := tls.Client(c.c, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		return err
	}

	c.c = tlsConn
	c.r = bufio.NewReader(tlsConn)
	c.w = bufio.NewWriter(tlsConn)
	return nil
}

func buildMsg(from, to, subj, body, filePath string, data []byte) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")

	boundary := "lab05-boundary-123456789"
	name := filepath.Base(filePath)
	ct := fileType(name)
	enc := split64(base64.StdEncoding.EncodeToString(data))

	hdrs := []string{
		fmt.Sprintf("From: %s", from),
		fmt.Sprintf("To: %s", to),
		fmt.Sprintf("Subject: %s", subj),
		"MIME-Version: 1.0",
		fmt.Sprintf("Content-Type: multipart/mixed; boundary=%q", boundary),
		"",
		fmt.Sprintf("--%s", boundary),
		"Content-Type: text/plain; charset=UTF-8",
		"",
		body,
		"",
		fmt.Sprintf("--%s", boundary),
		fmt.Sprintf("Content-Type: %s; name=%q", ct, name),
		"Content-Transfer-Encoding: base64",
		fmt.Sprintf("Content-Disposition: attachment; filename=%q", name),
		"",
		enc,
		fmt.Sprintf("--%s--", boundary),
		".",
	}
	return strings.Join(hdrs, "\r\n")
}

func fileType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

func split64(s string) string {
	var parts []string
	for len(s) > 76 {
		parts = append(parts, s[:76])
		s = s[76:]
	}
	if s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\r\n")
}

func hasExt(lines []string, key string) bool {
	for _, s := range lines {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), key) {
			return true
		}
	}
	return false
}

func (c *cli) data(msg string) error {
	if _, err := c.w.WriteString(msg + "\r\n"); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}

	code, _, err := c.read()
	if err != nil {
		return err
	}
	if code != 250 {
		return fmt.Errorf("unexpected server code %d after DATA", code)
	}
	return nil
}

func (c *cli) cmd(want int, f string, args ...any) error {
	_, err := c.cmdLines(want, f, args...)
	return err
}

func (c *cli) cmdLines(want int, f string, args ...any) ([]string, error) {
	if _, err := c.w.WriteString(fmt.Sprintf(f, args...) + "\r\n"); err != nil {
		return nil, err
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}

	code, lines, err := c.read()
	if err != nil {
		return nil, err
	}
	if code != want {
		return nil, fmt.Errorf("unexpected server code %d, want %d: %s", code, want, strings.Join(lines, " | "))
	}
	return lines, nil
}

func (c *cli) read() (int, []string, error) {
	var (
		code  int
		lines []string
	)

	for {
		s, err := c.r.ReadString('\n')
		if err != nil {
			if err == io.EOF && len(s) > 0 {
				return 0, nil, fmt.Errorf("short SMTP response: %q", strings.TrimSpace(s))
			}
			return 0, nil, err
		}

		s = strings.TrimRight(s, "\r\n")
		if len(s) < 3 {
			return 0, nil, fmt.Errorf("bad SMTP response: %q", s)
		}

		n, err := strconv.Atoi(s[:3])
		if err != nil {
			return 0, nil, fmt.Errorf("bad SMTP status code in %q", s)
		}
		code = n

		txt := ""
		if len(s) > 4 {
			txt = s[4:]
		}
		lines = append(lines, txt)

		if len(s) >= 4 && s[3] == ' ' {
			return code, lines, nil
		}
		if len(s) < 4 || s[3] != '-' {
			return 0, nil, fmt.Errorf("bad SMTP response line: %q", s)
		}
	}
}

func die(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(1)
}
