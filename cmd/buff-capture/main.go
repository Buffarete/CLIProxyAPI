// Command buff-capture records the requests a client sends to the proxy and the
// requests the proxy sends upstream, so the two can be compared.
//
//	buff-capture -mode client -listen 127.0.0.1:4131 -target http://127.0.0.1:4121 -out /tmp/cap/client
//	buff-capture -mode upstream -listen 127.0.0.1:4132 -out /tmp/cap/upstream
//	buff-capture -mode compare /tmp/cap/client /tmp/cap/upstream
//
// client mode is a recording reverse proxy in front of the proxy. upstream mode
// is a recording HTTPS forward proxy: point the proxy's proxy-url at it and trust
// <out>/ca.pem (SSL_CERT_FILE), so the proxy keeps talking to the real upstream
// URLs and every request still reaches the real upstream. Credential header
// values are never written; bodies are written as sent.
package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// credentialHeaders are recorded as present or absent only.
var credentialHeaders = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "Proxy-Authorization": true, "Cookie": true,
	// The ChatGPT workspace of the account the proxy picked, part of the Codex credential.
	"Chatgpt-Account-Id": true,
}

// hopHeaders describe one connection, not the request, and differ by design.
var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Host": true, "Content-Length": true, "Accept-Encoding": true,
}

const oauthBeta = "oauth-2025-04-20"

type record struct {
	Seq     int64               `json:"seq"`
	Method  string              `json:"method"`
	Host    string              `json:"host"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body_file"`
}

type recorder struct {
	dir string
	seq atomic.Int64
}

func (r *recorder) save(req *http.Request, body []byte) {
	n := r.seq.Add(1)
	headers := map[string][]string{}
	for k, v := range req.Header {
		if credentialHeaders[http.CanonicalHeaderKey(k)] {
			v = []string{"<credential>"}
		}
		headers[http.CanonicalHeaderKey(k)] = v
	}
	rec := record{Seq: n, Method: req.Method, Host: req.Host, Path: req.URL.RequestURI(), Headers: headers, Body: fmt.Sprintf("%04d.body", n)}
	if err := os.WriteFile(filepath.Join(r.dir, rec.Body), body, 0o600); err != nil {
		log.Printf("write body: %v", err)
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(r.dir, fmt.Sprintf("%04d.json", n)), data, 0o600); err != nil {
		log.Printf("write record: %v", err)
	}
	log.Printf("%04d %s %s%s (%d bytes)", n, req.Method, req.Host, req.URL.RequestURI(), len(body))
}

// forward sends req to target and streams the response back, flushing as it goes.
func forward(w http.ResponseWriter, req *http.Request, body []byte, target *url.URL, client *http.Client) {
	out, err := http.NewRequestWithContext(req.Context(), req.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, v := range req.Header {
		if !hopHeaders[http.CanonicalHeaderKey(k)] {
			out.Header[k] = v
		}
	}
	resp, err := client.Do(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, v := range resp.Header {
		if !hopHeaders[http.CanonicalHeaderKey(k)] {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, errRead := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if errRead != nil {
			return
		}
	}
}

func clientMode(listen, target string, rec *recorder) error {
	base, err := url.Parse(target)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	return http.ListenAndServe(listen, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		rec.save(req, body)
		u := *base
		u.Path = strings.TrimRight(base.Path, "/") + req.URL.Path
		u.RawQuery = req.URL.RawQuery
		forward(w, req, body, &u, client)
	}))
}

type authority struct {
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

func newAuthority(dir string) (*authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "buff-capture CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	pemCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err = os.WriteFile(filepath.Join(dir, "ca.pem"), pemCA, 0o644); err != nil {
		return nil, err
	}
	return &authority{cert: cert, key: key, cache: map[string]*tls.Certificate{}}, nil
}

func (a *authority) leaf(host string) (*tls.Certificate, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c := a.cache[host]; c != nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key}
	a.cache[host] = c
	return c, nil
}

// connListener hands hijacked CONNECT tunnels to an http.Server.
type connListener struct {
	conns chan net.Conn
	addr  net.Addr
}

func (l *connListener) Accept() (net.Conn, error) { return <-l.conns, nil }
func (l *connListener) Close() error              { return nil }
func (l *connListener) Addr() net.Addr            { return l.addr }

func upstreamMode(listen, dir string, rec *recorder) error {
	ca, err := newAuthority(dir)
	if err != nil {
		return err
	}
	tunnels := &connListener{conns: make(chan net.Conn), addr: &net.TCPAddr{}}
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableCompression: true, ForceAttemptHTTP2: true}}
	inner := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			body, _ := io.ReadAll(req.Body)
			rec.save(req, body)
			forward(w, req, body, &url.URL{Scheme: "https", Host: req.Host, Path: req.URL.Path, RawQuery: req.URL.RawQuery}, client)
		}),
		TLSConfig: &tls.Config{
			NextProtos: []string{"h2", "http/1.1"},
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				return ca.leaf(hello.ServerName)
			},
		},
	}
	go func() { fail(inner.ServeTLS(tunnels, "", "")) }()
	log.Printf("upstream capture on %s; trust %s", listen, filepath.Join(dir, "ca.pem"))
	return http.ListenAndServe(listen, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			http.Error(w, "only CONNECT is supported", http.StatusMethodNotAllowed)
			return
		}
		conn, buf, errHijack := w.(http.Hijacker).Hijack()
		if errHijack != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		tunnels.conns <- &bufferedConn{Conn: conn, r: buf.Reader}
	}))
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func load(dir string) ([]record, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []record
	for _, f := range files {
		data, errRead := os.ReadFile(f)
		if errRead != nil {
			return nil, errRead
		}
		var r record
		if err = json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// compare pairs each client request with the upstream request for the same
// path, in order, and reports every header and body difference that is not a
// credential or hop-by-hop header.
func compare(clientDir, upstreamDir string) (int, error) {
	clientRecs, err := load(clientDir)
	if err != nil {
		return 0, err
	}
	upstreamRecs, err := load(upstreamDir)
	if err != nil {
		return 0, err
	}
	used := map[int64]bool{}
	differences := 0
	for _, c := range clientRecs {
		var u *record
		for i := range upstreamRecs {
			cand := &upstreamRecs[i]
			// The proxy serves /v1/<endpoint> and calls <upstream base>/<endpoint>.
			if !used[cand.Seq] && cand.Method == c.Method && strings.HasSuffix(pathOnly(cand.Path), strings.TrimPrefix(pathOnly(c.Path), "/v1")) {
				u = cand
				break
			}
		}
		if u == nil {
			fmt.Printf("client %04d %s %s: no upstream request\n", c.Seq, c.Method, c.Path)
			continue
		}
		used[u.Seq] = true
		fmt.Printf("client %04d %s %s  ->  upstream %04d %s %s%s\n", c.Seq, c.Method, c.Path, u.Seq, u.Method, u.Host, u.Path)
		if strings.TrimPrefix(c.Path, "/v1") != strings.TrimPrefix(u.Path, "/v1") && !strings.HasSuffix(u.Path, strings.TrimPrefix(c.Path, "/v1")) {
			fmt.Printf("  path/query differs: %q vs %q\n", c.Path, u.Path)
			differences++
		}
		names := map[string]bool{}
		for k := range c.Headers {
			names[k] = true
		}
		for k := range u.Headers {
			names[k] = true
		}
		sorted := make([]string, 0, len(names))
		for k := range names {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		same := 0
		for _, k := range sorted {
			cv, inClient := c.Headers[k]
			uv, inUpstream := u.Headers[k]
			switch {
			case hopHeaders[k]:
				continue
			case credentialHeaders[k]:
				fmt.Printf("  credential %s: client %v, upstream %v\n", k, inClient, inUpstream)
				continue
			case !inUpstream:
				fmt.Printf("  - %s: %q (removed)\n", k, strings.Join(cv, ", "))
			case !inClient:
				fmt.Printf("  + %s: %q (added)\n", k, strings.Join(uv, ", "))
			case k == "Anthropic-Beta" && strings.Join(uv, ",") == strings.Join(cv, ",")+","+oauthBeta:
				fmt.Printf("  = %s: client list + %s, which Anthropic requires with an OAuth bearer\n", k, oauthBeta)
				continue
			case strings.Join(cv, "\x00") != strings.Join(uv, "\x00"):
				fmt.Printf("  ~ %s: %q -> %q\n", k, strings.Join(cv, ", "), strings.Join(uv, ", "))
			default:
				same++
				continue
			}
			differences++
		}
		fmt.Printf("  %d other headers identical\n", same)
		cb, _ := os.ReadFile(filepath.Join(clientDir, c.Body))
		ub, _ := os.ReadFile(filepath.Join(upstreamDir, u.Body))
		if bytes.Equal(cb, ub) {
			fmt.Printf("  body identical (%d bytes)\n", len(cb))
			continue
		}
		differences++
		fmt.Printf("  body differs: %d -> %d bytes\n", len(cb), len(ub))
		var cj, uj map[string]any
		if json.Unmarshal(cb, &cj) == nil && json.Unmarshal(ub, &uj) == nil {
			for k := range names {
				delete(names, k)
			}
			for k := range cj {
				names[k] = true
			}
			for k := range uj {
				names[k] = true
			}
			for k := range names {
				a, _ := json.Marshal(cj[k])
				b, _ := json.Marshal(uj[k])
				if !bytes.Equal(a, b) {
					fmt.Printf("    field %q differs (%d -> %d bytes)\n", k, len(a), len(b))
				}
			}
		}
	}
	for _, u := range upstreamRecs {
		if !used[u.Seq] {
			fmt.Printf("upstream %04d %s %s%s: no client request (made by the proxy itself)\n", u.Seq, u.Method, u.Host, u.Path)
		}
	}
	return differences, nil
}

func main() {
	mode := flag.String("mode", "", "client, upstream or compare")
	listen := flag.String("listen", "127.0.0.1:4131", "listen address")
	target := flag.String("target", "http://127.0.0.1:4121", "client mode: proxy base URL")
	out := flag.String("out", "", "directory for captured requests")
	flag.Parse()
	if *mode == "compare" {
		if flag.NArg() != 2 {
			fail(fmt.Errorf("compare needs <client dir> <upstream dir>"))
		}
		n, err := compare(flag.Arg(0), flag.Arg(1))
		if err != nil {
			fail(err)
		}
		fmt.Printf("%d differences outside credential and hop-by-hop headers\n", n)
		if n > 0 {
			os.Exit(1)
		}
		return
	}
	if *out == "" {
		fail(fmt.Errorf("-out is required"))
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		fail(err)
	}
	rec := &recorder{dir: *out}
	var err error
	switch *mode {
	case "client":
		err = clientMode(*listen, *target, rec)
	case "upstream":
		err = upstreamMode(*listen, *out, rec)
	default:
		err = fmt.Errorf("unknown -mode %q", *mode)
	}
	fail(err)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "buff-capture:", err)
	os.Exit(1)
}
