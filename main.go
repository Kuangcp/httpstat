package main

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kuangcp/logger"

	"github.com/fatih/color"
)

const (
	httpsTemplate = `` +
		`  DNS Lookup   TCP Connection   TLS Handshake   Server Processing   Content Transfer` + "\n" +
		`[%s  ┃     %s  ┃    %s  ┃        %s  ┃       %s  ]` + "\n" +
		`            ┃                ┃               ┃                   ┃                  ┃` + "\n" +
		`  namelookup:%s       ┃               ┃                   ┃                  ┃` + "\n" +
		`                      connect:%s      ┃                   ┃                  ┃` + "\n" +
		`                                  pretransfer:%s          ┃                  ┃` + "\n" +
		`                                                    starttransfer:%s         ┃` + "\n" +
		`                                                                               total:%s` + "\n"

	httpTemplate = `` +
		`   DNS Lookup   TCP Connection   Server Processing   Content Transfer` + "\n" +
		`[ %s  ┃     %s  ┃        %s  ┃       %s  ]` + "\n" +
		`             ┃                ┃                   ┃                  ┃` + "\n" +
		`   namelookup:%s       ┃                   ┃                  ┃` + "\n" +
		`                       connect:%s          ┃                  ┃` + "\n" +
		`                                     starttransfer:%s         ┃` + "\n" +
		`                                                                total:%s` + "\n"
)

var (
	// Command line flags.
	httpMethod      string
	postBody        string
	followRedirects bool
	onlyHeader      bool
	insecure        bool
	httpHeaders     headers
	saveOutput      bool
	outputFile      string
	showVersion     bool
	clientCertFile  string
	fourOnly        bool
	sixOnly         bool

	requestTimeout int

	// number of redirects followed
	redirectsFollowed int

	version = "devel" // for -v flag, updated during the release process with -ldflags=-X=main.version=...
)

const maxRedirects = 10

func init() {
	logger.SetLogPathTrim("httpstat/")

	flag.StringVar(&httpMethod, "X", "GET", "HTTP method to use")
	flag.StringVar(&postBody, "d", "", "the body of a POST or PUT request; from file use @filename")
	flag.BoolVar(&followRedirects, "L", false, "follow 30x redirects")
	flag.BoolVar(&onlyHeader, "I", false, "don't read body of request")
	flag.BoolVar(&insecure, "k", false, "allow insecure SSL connections")
	flag.Var(&httpHeaders, "H", "set HTTP header; repeatable: -H 'Accept: ...' -H 'Range: ...'")
	flag.BoolVar(&saveOutput, "O", false, "save body as remote filename")
	flag.StringVar(&outputFile, "o", "", "output file for body")
	flag.BoolVar(&showVersion, "v", false, "print version number")
	flag.StringVar(&clientCertFile, "E", "", "client cert file for tls config")
	flag.BoolVar(&fourOnly, "4", false, "resolve IPv4 addresses only")
	flag.BoolVar(&sixOnly, "6", false, "resolve IPv6 addresses only")

	flag.IntVar(&requestTimeout, "m", 10, "Maximum  time  in  seconds  that you allow httpstat's connection to take")

	flag.Usage = usage
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS] URL\n\n", os.Args[0])
	fmt.Fprintln(os.Stderr, "OPTIONS:")
	flag.PrintDefaults()
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "ENVIRONMENT:")
	fmt.Fprintln(os.Stderr, "  HTTP_PROXY    proxy for HTTP requests; complete URL or HOST[:PORT]")
	fmt.Fprintln(os.Stderr, "                used for HTTPS requests if HTTPS_PROXY undefined")
	fmt.Fprintln(os.Stderr, "  HTTPS_PROXY   proxy for HTTPS requests; complete URL or HOST[:PORT]")
	fmt.Fprintln(os.Stderr, "  NO_PROXY      comma-separated list of hosts to exclude from proxy")
}

func printf(format string, a ...interface{}) (n int, err error) {
	return fmt.Fprintf(color.Output, format, a...)
}

func grayscale(code color.Attribute) func(string, ...interface{}) string {
	return color.New(code + 232).SprintfFunc()
}

func main() {
	flag.Parse()

	if showVersion {
		fmt.Printf("%s %s (runtime: %s)\n", os.Args[0], version, runtime.Version())
		os.Exit(0)
	}

	if fourOnly && sixOnly {
		fmt.Fprintf(os.Stderr, "%s: Only one of -4 and -6 may be specified\n", os.Args[0])
		os.Exit(-1)
	}

	args := flag.Args()
	if len(args) != 1 {
		flag.Usage()
		os.Exit(2)
	}

	if (httpMethod == "POST" || httpMethod == "PUT") && postBody == "" {
		logger.Fatal("must supply post body using -d when POST or PUT is used")
	}

	if onlyHeader {
		httpMethod = "HEAD"
	}

	visit(parseURL(args[0]))
}

// readClientCert - helper function to read client certificate
// from pem formatted file
func readClientCert(filename string) []tls.Certificate {
	if filename == "" {
		return nil
	}
	var (
		pkeyPem []byte
		certPem []byte
	)

	// read client certificate file (must include client private key and certificate)
	certFileBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		logger.Fatal("failed to read client certificate file: ", err)
	}

	for {
		block, rest := pem.Decode(certFileBytes)
		if block == nil {
			break
		}
		certFileBytes = rest

		if strings.HasSuffix(block.Type, "PRIVATE KEY") {
			pkeyPem = pem.EncodeToMemory(block)
		}
		if strings.HasSuffix(block.Type, "CERTIFICATE") {
			certPem = pem.EncodeToMemory(block)
		}
	}

	cert, err := tls.X509KeyPair(certPem, pkeyPem)
	if err != nil {
		logger.Fatal("unable to load client cert and key pair: ", err)
	}
	return []tls.Certificate{cert}
}

func parseURL(uri string) *url.URL {
	if !strings.Contains(uri, "://") && !strings.HasPrefix(uri, "//") {
		uri = "//" + uri
	}

	parsedURL, err := url.Parse(uri)
	if err != nil {
		logger.Fatal("could not parse url %q: %v", uri, err)
	}

	if parsedURL.Scheme == "" {
		parsedURL.Scheme = "http"
		if !strings.HasSuffix(parsedURL.Host, ":80") {
			parsedURL.Scheme += "s"
		}
	}
	return parsedURL
}

func headerKeyValue(h string) (string, string) {
	i := strings.Index(h, ":")
	if i == -1 {
		logger.Fatal("Header '%s' has invalid format, missing ':'", h)
	}
	return strings.TrimRight(h[:i], " "), strings.TrimLeft(h[i:], " :")
}

func dialContext(network string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		return (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: false,
		}).DialContext(ctx, network, addr)
	}
}

// visit visits a url and times the interaction.
// If the response is a 30x, visit follows the redirect.
func visit(url *url.URL) {
	req := newRequest(httpMethod, url, postBody)

	var t0, t1, t2, t3, t4, t5, t6 time.Time

	trace := &httptrace.ClientTrace{
		DNSStart: func(_ httptrace.DNSStartInfo) { t0 = time.Now() },
		DNSDone:  func(_ httptrace.DNSDoneInfo) { t1 = time.Now() },
		ConnectStart: func(_, _ string) {
			if t1.IsZero() {
				// connecting to IP
				t1 = time.Now()
			}
		},
		ConnectDone: func(net, addr string, err error) {
			// TODO print timeout, also print tree
			if err != nil {
				fmt.Printf("     DNS Lookup: %v\n TCP Connection: %v\n", t1.Sub(t0), time.Now().Sub(t1))
				logger.Error("unable to connect to host %v: %v", addr, err)
				return
			}
			t2 = time.Now()

			printf("\n%s%s\n", color.GreenString("Connected to "), color.CyanString(addr))
		},
		GotConn:              func(_ httptrace.GotConnInfo) { t3 = time.Now() },
		GotFirstResponseByte: func() { t4 = time.Now() },
		TLSHandshakeStart:    func() { t5 = time.Now() },
		TLSHandshakeDone:     func(_ tls.ConnectionState, _ error) { t6 = time.Now() },
	}
	req = req.WithContext(httptrace.WithClientTrace(context.Background(), trace))

	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	switch {
	case fourOnly:
		tr.DialContext = dialContext("tcp4")
	case sixOnly:
		tr.DialContext = dialContext("tcp6")
	}

	switch url.Scheme {
	case "https":
		host, _, err := net.SplitHostPort(req.Host)
		if err != nil {
			host = req.Host
		}

		tr.TLSClientConfig = &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: insecure,
			Certificates:       readClientCert(clientCertFile),
			MinVersion:         tls.VersionTLS12,
		}
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   time.Second * time.Duration(requestTimeout),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// always refuse to follow redirects, visit does that
			// manually if required.
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		logger.Fatal("failed to read response: %v", err)
	}

	// Print SSL/TLS version which is used for connection
	connectedVia := "plaintext"
	if resp.TLS != nil {
		switch resp.TLS.Version {
		case tls.VersionTLS12:
			connectedVia = "TLSv1.2"
		case tls.VersionTLS13:
			connectedVia = "TLSv1.3"
		}
	}
	printf("\n%s %s\n", color.GreenString("Connected via"), color.CyanString("%s", connectedVia))

	bodyMsg := readResponseBody(req, resp)
	resp.Body.Close()

	t7 := time.Now() // after read body
	if t0.IsZero() {
		// we skipped DNS
		t0 = t1
	}

	// print status line and headers
	printf("\n%s%s%s\n", color.GreenString("HTTP"), grayscale(14)("/"),
		color.CyanString("%d.%d %s", resp.ProtoMajor, resp.ProtoMinor, resp.Status))

	names := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		names = append(names, k)
	}
	reqHeaders := headers(names)
	sort.Slice(reqHeaders, func(i, j int) bool {
		a, b := reqHeaders[i], reqHeaders[j]

		// server always sorts at the top
		if a == "Server" {
			return true
		}
		if b == "Server" {
			return false
		}

		endtoend := func(n string) bool {
			// https://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html#sec13.5.1
			switch n {
			case "Connection",
				"Keep-Alive",
				"Proxy-Authenticate",
				"Proxy-Authorization",
				"TE",
				"Trailers",
				"Transfer-Encoding",
				"Upgrade":
				return false
			default:
				return true
			}
		}

		x, y := endtoend(a), endtoend(b)
		if x == y {
			// both are of the same class
			return a < b
		}
		return x
	})
	for _, k := range names {
		printf("%s %s\n", grayscale(14)(k+":"), color.CyanString(strings.Join(resp.Header[k], ",")))
	}

	if bodyMsg != "" {
		printf("\n%s\n", bodyMsg)
	}

	blockFmt := func(d time.Duration) string {
		return color.CyanString("%7dms", int(d/time.Millisecond))
	}

	flagFmt := func(d time.Duration) string {
		return color.CyanString("%-9s", strconv.Itoa(int(d/time.Millisecond))+"ms")
	}

	colorize := func(s string) string {
		v := strings.Split(s, "\n")
		v[0] = grayscale(16)(v[0])
		return strings.Join(v, "\n")
	}

	fmt.Println()

	switch url.Scheme {
	case "https":
		printf(colorize(httpsTemplate),
			blockFmt(t1.Sub(t0)), // dns lookup
			blockFmt(t2.Sub(t1)), // tcp connection
			blockFmt(t6.Sub(t5)), // tls handshake
			blockFmt(t4.Sub(t3)), // server processing
			blockFmt(t7.Sub(t4)), // content transfer

			flagFmt(t1.Sub(t0)), // namelookup
			flagFmt(t2.Sub(t0)), // connect
			flagFmt(t3.Sub(t0)), // pretransfer
			flagFmt(t4.Sub(t0)), // starttransfer
			flagFmt(t7.Sub(t0)), // total
		)
	case "http":
		printf(colorize(httpTemplate),
			blockFmt(t1.Sub(t0)), // dns lookup
			blockFmt(t3.Sub(t1)), // tcp connection
			blockFmt(t4.Sub(t3)), // server processing
			blockFmt(t7.Sub(t4)), // content transfer

			flagFmt(t1.Sub(t0)), // namelookup
			flagFmt(t3.Sub(t0)), // connect
			flagFmt(t4.Sub(t0)), // starttransfer
			flagFmt(t7.Sub(t0)), // total
		)
	}

	if followRedirects && isRedirect(resp) {
		loc, err := resp.Location()
		if err != nil {
			if err == http.ErrNoLocation {
				// 30x but no Location to follow, give up.
				return
			}
			logger.Fatal("unable to follow redirect: %v", err)
		}

		redirectsFollowed++
		if redirectsFollowed > maxRedirects {
			logger.Fatal("maximum number of redirects (%d) followed", maxRedirects)
		}

		visit(loc)
	}
}

func isRedirect(resp *http.Response) bool {
	return resp.StatusCode > 299 && resp.StatusCode < 400
}

func newRequest(method string, url *url.URL, body string) *http.Request {
	req, err := http.NewRequest(method, url.String(), createBody(body))
	if err != nil {
		logger.Fatal("unable to create request: %v", err)
	}
	for _, h := range httpHeaders {
		k, v := headerKeyValue(h)
		if strings.EqualFold(k, "host") {
			req.Host = v
			continue
		}
		req.Header.Add(k, v)
	}
	return req
}

func createBody(body string) io.Reader {
	if strings.HasPrefix(body, "@") {
		filename := body[1:]
		f, err := os.Open(filename)
		if err != nil {
			logger.Fatal("failed to open data file %s: %v", filename, err)
		}
		return f
	}
	return strings.NewReader(body)
}

// getFilenameFromHeaders tries to automatically determine the output filename,
// when saving to disk, based on the Content-Disposition header.
// If the header is not present, or it does not contain enough information to
// determine which filename to use, this function returns "".
func getFilenameFromHeaders(headers http.Header) string {
	// if the Content-Disposition header is set parse it
	if hdr := headers.Get("Content-Disposition"); hdr != "" {
		// pull the media type, and subsequent params, from
		// the body of the header field
		mt, params, err := mime.ParseMediaType(hdr)

		// if there was no error and the media type is attachment
		if err == nil && mt == "attachment" {
			if filename := params["filename"]; filename != "" {
				return filename
			}
		}
	}

	// return an empty string if we were unable to determine the filename
	return ""
}

// readResponseBody consumes the body of the response.
// readResponseBody returns an informational message about the
// disposition of the response body's contents.
func readResponseBody(req *http.Request, resp *http.Response) string {
	if isRedirect(resp) || req.Method == http.MethodHead {
		return ""
	}

	w := ioutil.Discard
	msg := color.CyanString("Body discarded")

	if saveOutput || outputFile != "" {
		filename := outputFile

		if saveOutput {
			// try to get the filename from the Content-Disposition header
			// otherwise fall back to the RequestURI
			if filename = getFilenameFromHeaders(resp.Header); filename == "" {
				filename = path.Base(req.URL.RequestURI())
			}

			if filename == "/" {
				logger.Fatal("No remote filename; specify output filename with -o to save response body")
			}
		}

		f, err := os.Create(filename)
		if err != nil {
			logger.Fatal("unable to create file %s: %v", filename, err)
		}
		defer f.Close()
		w = f
		msg = color.CyanString("Body read")
	}

	if _, err := io.Copy(w, resp.Body); err != nil && w != ioutil.Discard {
		logger.Fatal("failed to read response body: %v", err)
	}

	return msg
}

type headers []string

func (h headers) String() string {
	var o []string
	for _, v := range h {
		o = append(o, "-H "+v)
	}
	return strings.Join(o, " ")
}

func (h *headers) Set(v string) error {
	*h = append(*h, v)
	return nil
}
