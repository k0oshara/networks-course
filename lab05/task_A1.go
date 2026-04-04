package main

import (
    "flag"
    "fmt"
    "log"
    "net/smtp"
    "os"
    "strconv"
    "strings"
)

const defaultFrom = "sender@example.com"

func main() {
    to := flag.String("to", "", "recipient email address")
    subj := flag.String("subject", "Test message", "email subject")
    fmtOpt := flag.String("format", "txt", "message format: txt or html")
    body := flag.String("body", "", "message body")
    bodyPath := flag.String("body-file", "", "path to a file with message body")

    host := flag.String("smtp-host", envStr("SMTP_HOST", ""), "SMTP server host")
    port := flag.Int("smtp-port", envInt("SMTP_PORT", 587), "SMTP server port")
    user := flag.String("smtp-user", envStr("SMTP_USER", ""), "SMTP username")
    pass := flag.String("smtp-pass", envStr("SMTP_PASS", ""), "SMTP password")
    from := flag.String("from", envStr("SMTP_FROM", defaultFrom), "sender email address")

    flag.Parse()

    if err := chkReq(*to, *host, *user, *pass, *from); err != nil {
        log.Fatalf("argument error: %v", err)
    }

    ct, err := msgType(*fmtOpt)
    if err != nil {
        log.Fatal(err)
    }

    msgBody, err := readBody(*body, *bodyPath)
    if err != nil {
        log.Fatalf("failed to read message body: %v", err)
    }
    if strings.TrimSpace(msgBody) == "" {
        log.Fatal("message body is empty: use -body or -body-file")
    }

    msg := buildMsg(*from, *to, *subj, ct, msgBody)
    addr := fmt.Sprintf("%s:%d", *host, *port)
    auth := smtp.PlainAuth("", *user, *pass, *host)

    if err := smtp.SendMail(addr, auth, *from, []string{*to}, []byte(msg)); err != nil {
        log.Fatalf("failed to send email: %v", err)
    }

    fmt.Printf("Email sent to %s\n", *to)
}

func chkReq(to, host, user, pass, from string) error {
    switch {
    case strings.TrimSpace(to) == "":
        return fmt.Errorf("missing required flag -to")
    case strings.TrimSpace(host) == "":
        return fmt.Errorf("missing SMTP host: use -smtp-host or SMTP_HOST")
    case strings.TrimSpace(user) == "":
        return fmt.Errorf("missing SMTP username: use -smtp-user or SMTP_USER")
    case strings.TrimSpace(pass) == "":
        return fmt.Errorf("missing SMTP password: use -smtp-pass or SMTP_PASS")
    case strings.TrimSpace(from) == "":
        return fmt.Errorf("missing sender address")
    default:
        return nil
    }
}

func msgType(fmtOpt string) (string, error) {
    switch strings.ToLower(strings.TrimSpace(fmtOpt)) {
    case "txt", "text", "plain":
        return "text/plain; charset=UTF-8", nil
    case "html":
        return "text/html; charset=UTF-8", nil
    default:
        return "", fmt.Errorf("unsupported format %q: use txt or html", fmtOpt)
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

func buildMsg(from, to, subj, ct, body string) string {
    hdrs := []string{
        fmt.Sprintf("From: %s", from),
        fmt.Sprintf("To: %s", to),
        fmt.Sprintf("Subject: %s", subj),
        "MIME-Version: 1.0",
        fmt.Sprintf("Content-Type: %s", ct),
        "",
        body,
    }
    return strings.Join(hdrs, "\r\n")
}

func envStr(key, def string) string {
    val := strings.TrimSpace(os.Getenv(key))
    if val == "" {
        return def
    }
    return val
}

func envInt(key string, def int) int {
    val := strings.TrimSpace(os.Getenv(key))
    if val == "" {
        return def
    }

    n, err := strconv.Atoi(val)
    if err != nil {
        return def
    }
    return n
}
