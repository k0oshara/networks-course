package main

import (
    "context"
    "errors"
    "flag"
    "fmt"
    "io"
    "log"
    "net"
    "net/http"
    "net/url"
    "os"
    "os/signal"
    "strings"
    "syscall"
    "time"
)

var hopByHopHeaders = map[string]struct{}{
    "Connection":          {},
    "Keep-Alive":          {},
    "Proxy-Authenticate":  {},
    "Proxy-Authorization": {},
    "Proxy-Connection":    {},
    "Te":                  {},
    "Trailer":             {},
    "Transfer-Encoding":   {},
    "Upgrade":             {},
}

type proxyServer struct {
    client *http.Client
    logger *log.Logger
}

func main() {
    addr := flag.String("addr", ":8888", "proxy listen address")
    logPath := flag.String("log", "proxy.log", "path to proxy log file")
    flag.Parse()

    logFile, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
    if err != nil {
        log.Fatalf("open log file: %v", err)
    }
    defer logFile.Close()

    proxy := &proxyServer{
        client: &http.Client{
            Timeout: 30 * time.Second,
            CheckRedirect: func(req *http.Request, via []*http.Request) error {
                return http.ErrUseLastResponse
            },
            Transport: &http.Transport{
                Proxy:                 nil,
                MaxIdleConns:          100,
                IdleConnTimeout:       90 * time.Second,
                TLSHandshakeTimeout:   10 * time.Second,
                ExpectContinueTimeout: 1 * time.Second,
            },
        },
        logger: log.New(logFile, "", log.LstdFlags),
    }

    server := &http.Server{
        Addr:         *addr,
        Handler:      http.HandlerFunc(proxy.serveProxy),
        ReadTimeout:  15 * time.Second,
        WriteTimeout: 30 * time.Second,
        IdleTimeout:  60 * time.Second,
    }

    go func() {
        log.Printf("proxy listening on %s", *addr)
        if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            log.Fatalf("proxy listen failed: %v", err)
        }
    }()

    stop := make(chan os.Signal, 1)
    signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
    <-stop

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    if err := server.Shutdown(ctx); err != nil {
        log.Printf("proxy shutdown failed: %v", err)
    }
}

func (p *proxyServer) serveProxy(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet && r.Method != http.MethodPost {
        p.respondWithError(w, r.Method, r.URL.String(), http.StatusMethodNotAllowed, "only GET and POST are supported")
        return
    }

    targetURL, err := parseTargetURL(r)
    if err != nil {
        p.respondWithError(w, r.Method, r.URL.String(), http.StatusBadRequest, err.Error())
        return
    }

    outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), r.Body)
    if err != nil {
        p.respondWithError(w, r.Method, targetURL.String(), http.StatusBadRequest, fmt.Sprintf("create upstream request: %v", err))
        return
    }

    outReq.Header = copyClientHeaders(r.Header)
    outReq.Host = targetURL.Host
    outReq.ContentLength = r.ContentLength
    outReq.Close = false
    addForwardedFor(outReq.Header, r.RemoteAddr)

    resp, err := p.client.Do(outReq)
    if err != nil {
        p.respondWithError(w, r.Method, targetURL.String(), http.StatusBadGateway, fmt.Sprintf("upstream request failed: %v", err))
        return
    }
    defer resp.Body.Close()

    copyServerHeaders(w.Header(), resp.Header)
    w.WriteHeader(resp.StatusCode)

    if _, err := io.Copy(w, resp.Body); err != nil {
        p.logger.Printf("%s %s -> %d body_copy_error=%q", r.Method, targetURL.String(), resp.StatusCode, err)
        return
    }

    p.logger.Printf("%s %s -> %d", r.Method, targetURL.String(), resp.StatusCode)
}

func parseTargetURL(r *http.Request) (*url.URL, error) {
    if r.URL == nil {
        return nil, errors.New("missing request URL")
    }

    if r.URL.IsAbs() {
        if r.URL.Scheme != "http" {
            return nil, fmt.Errorf("unsupported scheme %q", r.URL.Scheme)
        }
        return r.URL, nil
    }

    rawTarget := strings.TrimPrefix(r.URL.Path, "/")
    if rawTarget == "" {
        return nil, errors.New("target URL is empty")
    }

    if r.URL.RawQuery != "" {
        rawTarget += "?" + r.URL.RawQuery
    }

    if !strings.HasPrefix(rawTarget, "http://") {
        rawTarget = "http://" + rawTarget
    }

    targetURL, err := url.Parse(rawTarget)
    if err != nil {
        return nil, fmt.Errorf("parse target URL: %w", err)
    }

    if targetURL.Host == "" {
        return nil, errors.New("target host is empty")
    }

    if targetURL.Scheme != "http" {
        return nil, fmt.Errorf("unsupported scheme %q", targetURL.Scheme)
    }

    return targetURL, nil
}

func copyClientHeaders(src http.Header) http.Header {
    dst := make(http.Header, len(src))
    connectionHeaders := connectionTokens(src)
    for key, values := range src {
        if isProxyHopHeader(key) || connectionHeaders[http.CanonicalHeaderKey(key)] {
            continue
        }
        for _, value := range values {
            dst.Add(key, value)
        }
    }
    return dst
}

func copyServerHeaders(dst, src http.Header) {
    connectionHeaders := connectionTokens(src)
    for key, values := range src {
        if isProxyHopHeader(key) || connectionHeaders[http.CanonicalHeaderKey(key)] {
            continue
        }
        for _, value := range values {
            dst.Add(key, value)
        }
    }
}

func isProxyHopHeader(name string) bool {
    _, found := hopByHopHeaders[http.CanonicalHeaderKey(name)]
    return found
}

func connectionTokens(headers http.Header) map[string]bool {
    result := make(map[string]bool)
    for _, value := range headers.Values("Connection") {
        for _, token := range strings.Split(value, ",") {
            token = strings.TrimSpace(token)
            if token == "" {
                continue
            }
            result[http.CanonicalHeaderKey(token)] = true
        }
    }
    return result
}

func addForwardedFor(headers http.Header, remoteAddr string) {
    host, _, err := net.SplitHostPort(remoteAddr)
    if err != nil {
        host = remoteAddr
    }

    if prior := headers.Get("X-Forwarded-For"); prior != "" {
        headers.Set("X-Forwarded-For", prior+", "+host)
        return
    }

    headers.Set("X-Forwarded-For", host)
}

func (p *proxyServer) respondWithError(w http.ResponseWriter, method, rawURL string, status int, message string) {
    http.Error(w, message, status)
    p.logger.Printf("%s %s -> %d error=%q", method, rawURL, status, message)
}
