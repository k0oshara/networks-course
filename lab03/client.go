package main

import (
    "bufio"
    "fmt"
    "io"
    "net"
    "net/url"
    "os"
    "strconv"
    "strings"
)

const maxRedirects = 5

type response struct {
    statusLine string
    headers    map[string]string
    body       []byte
    raw        []byte
}

func main() {
    if len(os.Args) != 4 {
        fmt.Fprintf(os.Stderr, "usage: %s server_host server_port filename\n", os.Args[0])
        os.Exit(1)
    }

    host := os.Args[1]
    port := os.Args[2]
    filename := normalizePath(os.Args[3])

    if _, err := strconv.Atoi(port); err != nil {
        fmt.Fprintln(os.Stderr, "server_port must be an integer")
        os.Exit(1)
    }

    resp, err := fetch(host, port, filename, maxRedirects)
    if err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }

    fmt.Print(string(resp.raw))
}

func fetch(host, port, path string, redirectsLeft int) (response, error) {
    resp, err := sendGET(host, port, path)
    if err != nil {
        return response{}, err
    }

    statusCode := parseStatusCode(resp.statusLine)
    if (statusCode == 301 || statusCode == 302 || statusCode == 307 || statusCode == 308) && redirectsLeft > 0 {
        location := resp.headers["Location"]
        if location == "" {
            return resp, nil
        }

        nextHost, nextPort, nextPath, err := parseLocation(host, port, location)
        if err != nil {
            return response{}, err
        }

        return fetch(nextHost, nextPort, nextPath, redirectsLeft-1)
    }

    return resp, nil
}

func sendGET(host, port, path string) (response, error) {
    address := net.JoinHostPort(host, port)
    conn, err := net.Dial("tcp", address)
    if err != nil {
        return response{}, fmt.Errorf("connect failed: %w", err)
    }
    defer conn.Close()

    request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, host)
    if _, err := conn.Write([]byte(request)); err != nil {
        return response{}, fmt.Errorf("send failed: %w", err)
    }

    reader := bufio.NewReader(conn)

    statusLine, err := reader.ReadString('\n')
    if err != nil {
        return response{}, fmt.Errorf("read status line failed: %w", err)
    }

    var raw strings.Builder
    raw.WriteString(statusLine)

    headers := make(map[string]string)
    for {
        line, err := reader.ReadString('\n')
        if err != nil {
            return response{}, fmt.Errorf("read headers failed: %w", err)
        }

        raw.WriteString(line)
        if line == "\r\n" || line == "\n" {
            break
        }

        headerLine := strings.TrimSpace(line)
        parts := strings.SplitN(headerLine, ":", 2)
        if len(parts) == 2 {
            headers[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
        }
    }

    body, err := io.ReadAll(reader)
    if err != nil {
        return response{}, fmt.Errorf("read body failed: %w", err)
    }

    raw.Write(body)

    return response{
        statusLine: strings.TrimSpace(statusLine),
        headers:    headers,
        body:       body,
        raw:        []byte(raw.String()),
    }, nil
}

func parseStatusCode(statusLine string) int {
    parts := strings.Fields(statusLine)
    if len(parts) < 2 {
        return 0
    }

    code, err := strconv.Atoi(parts[1])
    if err != nil {
        return 0
    }

    return code
}

func parseLocation(defaultHost, defaultPort, location string) (host, port, path string, err error) {
    parsed, err := url.Parse(location)
    if err != nil {
        return "", "", "", fmt.Errorf("invalid redirect location: %w", err)
    }

    host = defaultHost
    port = defaultPort
    path = parsed.Path

    if path == "" {
        path = "/"
    }

    if parsed.Hostname() != "" {
        host = parsed.Hostname()
    }

    if parsed.Port() != "" {
        port = parsed.Port()
    }

    if parsed.RawQuery != "" {
        path += "?" + parsed.RawQuery
    }

    return host, port, path, nil
}

func normalizePath(path string) string {
    if path == "" {
        return "/"
    }

    if !strings.HasPrefix(path, "/") {
        return "/" + path
    }

    return path
}
