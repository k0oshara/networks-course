package main

import (
    "bufio"
    "fmt"
    "io"
    "log"
    "mime"
    "net"
    "net/http"
    "net/url"
    "os"
    "path/filepath"
    "strconv"
    "strings"
)

const readBufferSize = 4096

func main() {
    if len(os.Args) != 2 {
        fmt.Fprintf(os.Stderr, "usage: %s server_port\n", filepath.Base(os.Args[0]))
        os.Exit(1)
    }

    port, err := strconv.Atoi(os.Args[1])
    if err != nil || port < 1 || port > 65535 {
        fmt.Fprintln(os.Stderr, "server_port must be an integer in range 1..65535")
        os.Exit(1)
    }

    listener, err := net.Listen("tcp", ":"+strconv.Itoa(port))
    if err != nil {
        log.Fatalf("listen failed: %v", err)
    }
    defer listener.Close()

    log.Printf("server is listening on http://127.0.0.1:%d", port)

    for {
        conn, err := listener.Accept()
        if err != nil {
            log.Printf("accept failed: %v", err)
            continue
        }

        handleConnection(conn)
    }
}

func handleConnection(conn net.Conn) {
    defer conn.Close()

    reader := bufio.NewReaderSize(conn, readBufferSize)
    requestLine, err := reader.ReadString('\n')
    if err != nil {
        writeResponse(conn, "400 Bad Request", "text/plain; charset=utf-8", []byte("400 Bad Request\n"))
        return
    }

    method, target, ok := parseRequestLine(requestLine)
    if !ok {
        writeResponse(conn, "400 Bad Request", "text/plain; charset=utf-8", []byte("400 Bad Request\n"))
        return
    }

    if err := discardHeaders(reader); err != nil {
        writeResponse(conn, "400 Bad Request", "text/plain; charset=utf-8", []byte("400 Bad Request\n"))
        return
    }

    if method != "GET" {
        writeResponse(conn, "405 Method Not Allowed", "text/plain; charset=utf-8", []byte("405 Method Not Allowed\n"))
        return
    }

    path, err := resolvePath(target)
    if err != nil {
        writeResponse(conn, "404 Not Found", "text/plain; charset=utf-8", []byte("404 Not Found\n"))
        return
    }

    data, err := os.ReadFile(path)
    if err != nil {
        if os.IsNotExist(err) {
            writeResponse(conn, "404 Not Found", "text/plain; charset=utf-8", []byte("404 Not Found\n"))
            return
        }

        writeResponse(conn, "500 Internal Server Error", "text/plain; charset=utf-8", []byte("500 Internal Server Error\n"))
        return
    }

    contentType := detectContentType(path, data)
    writeResponse(conn, "200 OK", contentType, data)
}

func parseRequestLine(line string) (method string, target string, ok bool) {
    parts := strings.Fields(strings.TrimSpace(line))
    if len(parts) != 3 {
        return "", "", false
    }

    if !strings.HasPrefix(parts[2], "HTTP/") {
        return "", "", false
    }

    return parts[0], parts[1], true
}

func discardHeaders(reader *bufio.Reader) error {
    for {
        line, err := reader.ReadString('\n')
        if err != nil {
            if err == io.EOF && (line == "" || line == "\r\n") {
                return nil
            }
            return err
        }

        if line == "\r\n" || line == "\n" {
            return nil
        }
    }
}

func resolvePath(target string) (string, error) {
    parsedTarget, err := url.PathUnescape(strings.SplitN(target, "?", 2)[0])
    if err != nil {
        return "", err
    }

    cleanPath := filepath.Clean("/" + parsedTarget)
    if cleanPath == "/" {
        cleanPath = "/index.html"
    }

    relativePath := strings.TrimPrefix(cleanPath, "/")
    fullPath := filepath.Join(".", relativePath)
    absolutePath, err := filepath.Abs(fullPath)
    if err != nil {
        return "", err
    }

    workingDir, err := os.Getwd()
    if err != nil {
        return "", err
    }

    workingDir = filepath.Clean(workingDir)
    if absolutePath != workingDir && !strings.HasPrefix(absolutePath, workingDir+string(os.PathSeparator)) {
        return "", fmt.Errorf("path escapes working directory")
    }

    info, err := os.Stat(absolutePath)
    if err != nil {
        return "", err
    }

    if info.IsDir() {
        indexPath := filepath.Join(absolutePath, "index.html")
        indexInfo, err := os.Stat(indexPath)
        if err != nil || indexInfo.IsDir() {
            return "", os.ErrNotExist
        }
        return indexPath, nil
    }

    return absolutePath, nil
}

func detectContentType(path string, data []byte) string {
    ext := strings.ToLower(filepath.Ext(path))
    if contentType := mime.TypeByExtension(ext); contentType != "" {
        return contentType
    }

    sniffLen := len(data)
    sniffLen = min(sniffLen, 512);
    if sniffLen == 0 {
        return "application/octet-stream"
    }
    return http.DetectContentType(data[:sniffLen])
}

func writeResponse(conn net.Conn, status, contentType string, body []byte) {
    fmt.Fprintf(conn, "HTTP/1.1 %s\r\n", status)
    fmt.Fprintf(conn, "Content-Length: %d\r\n", len(body))
    fmt.Fprintf(conn, "Content-Type: %s\r\n", contentType)
    fmt.Fprint(conn, "Connection: close\r\n")
    fmt.Fprint(conn, "\r\n")
    _, _ = conn.Write(body)
}
