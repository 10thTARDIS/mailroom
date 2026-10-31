// SPDX-License-Identifier: AGPL-3.0-only

// Package sievetest is an in-process fake ManageSieve (RFC 5804) server for
// unit and integration tests of internal/sieve and its callers.
//
// Its wire bytes copy Dovecot Pigeonhole 2.3.21's, captured from a real
// session (the greeting, the "SASL" "" advertised before TLS, the re-issued
// capabilities after STARTTLS, the {n} literal GETSCRIPT returns and the
// "(NONEXISTENT)" code) rather than a belief about the format.
//
// It RECORDS every verb and every byte the client sends, and answers any verb
// that would change server state (PUTSCRIPT, SETACTIVE, DELETESCRIPT,
// RENAMESCRIPT, CHECKSCRIPT, HAVESPACE) with NO while counting it in
// WriteAttempts — so a test can assert that a read-only caller never so much
// as asked.
//
// It must not import internal/sieve: the sieve package's own tests import it.
package sievetest

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// PigeonholeSieve is the SIEVE capability Dovecot Pigeonhole 2.3.21 advertises
// with its stock configuration (captured; vacation-seconds is off by default).
const PigeonholeSieve = "fileinto reject envelope encoded-character vacation subaddress " +
	"comparator-i;ascii-numeric relational regex imap4flags copy include variables body " +
	"enotify environment mailbox date index ihave duplicate mime foreverypart extracttext"

// TLSMode selects how the fake offers TLS.
type TLSMode int

const (
	// StartTLS: cleartext greeting advertising STARTTLS (the RFC 5804 default).
	StartTLS TLSMode = iota
	// ImplicitTLS: TLS from the first byte.
	ImplicitTLS
	// NoTLS: no TLS at all (to prove the client refuses to authenticate).
	NoTLS
)

// Hostile selects a misbehaving server, for tests that the client's bounds
// hold against a server that never stops talking.
type Hostile int

const (
	// Honest is a well-behaved server.
	Honest Hostile = iota
	// EndlessGreeting streams capability lines forever instead of a greeting.
	EndlessGreeting
	// EndlessListScripts answers LISTSCRIPTS with names forever.
	EndlessListScripts
	// EndlessSpaces answers GETSCRIPT with spaces forever: no token, no line
	// break, so only a byte budget can stop it.
	EndlessSpaces
)

// Script is one script the fake holds.
type Script struct {
	Name   string
	Body   string
	Active bool
	// Deny makes GETSCRIPT of this script answer NO with Options.ErrorText.
	Deny bool
}

// Options configures a fake server.
type Options struct {
	Username, Password string
	Scripts            []Script
	// Sieve is the SIEVE capability value; "" means PigeonholeSieve.
	Sieve string
	TLS   TLSMode
	// SASL is the mechanism list offered once TLS is up; "" means "PLAIN".
	SASL string
	// Hostile makes the server misbehave; the zero value is Honest.
	Hostile Hostile
	// Implementation replaces the IMPLEMENTATION capability ("" means
	// "Dovecot Pigeonhole"). It is sent with ManageSieve quoting, so control
	// characters reach the client as raw bytes, as a hostile server would.
	Implementation string
	// ErrorText, when set, is the text of the NO for a failed AUTHENTICATE, a
	// Deny script's GETSCRIPT, and LOGOUT (which then answers NO).
	ErrorText string

	// AllowWrites makes CHECKSCRIPT, PUTSCRIPT, SETACTIVE and DELETESCRIPT
	// behave like Pigeonhole's instead of answering NO. They are still counted
	// in WriteAttempts, and Scripts() shows the resulting state.
	AllowWrites bool
	// MaxScriptSize, when set, is advertised as "MAXSCRIPTSIZE" and enforced:
	// a bigger CHECKSCRIPT or PUTSCRIPT gets NO (QUOTA/MAXSIZE).
	MaxScriptSize int
	// InvalidMarker, when set, makes CHECKSCRIPT and PUTSCRIPT of a body that
	// contains it fail as a compile error.
	InvalidMarker string
	// ActivateOnPut names a script the fake makes active as a side effect of
	// the next PUTSCRIPT: someone switching filters in webmail in the middle of
	// Mailroom's enable sequence.
	ActivateOnPut string
}

// stream writes chunk until the client goes away.
func stream(w io.Writer, chunk func(i int) string) {
	for i := 0; ; i++ {
		if _, err := io.WriteString(w, chunk(i)); err != nil {
			return
		}
	}
}

// Server is a running fake.
type Server struct {
	t    testing.TB
	opts Options
	ln   net.Listener
	cert tls.Certificate
	pool *x509.CertPool

	mu            sync.Mutex
	verbs         []string
	raw           []byte
	writeAttempts int
	wg            sync.WaitGroup
	conns         map[net.Conn]bool
}

// writeVerbs are the verbs that change server state.
var writeVerbs = map[string]bool{
	"PUTSCRIPT": true, "SETACTIVE": true, "DELETESCRIPT": true,
	"RENAMESCRIPT": true, "CHECKSCRIPT": true, "HAVESPACE": true,
}

// New starts a fake on 127.0.0.1 and stops it at test cleanup.
func New(t testing.TB, opts Options) *Server {
	t.Helper()
	if opts.Sieve == "" {
		opts.Sieve = PigeonholeSieve
	}
	if opts.SASL == "" {
		opts.SASL = "PLAIN"
	}
	cert, pool := selfSigned(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sievetest: listen: %v", err)
	}
	s := &Server{t: t, opts: opts, ln: ln, cert: cert, pool: pool, conns: map[net.Conn]bool{}}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		// A test that abandons a session must not hold cleanup for the
		// connection deadline: close whatever is still open.
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}

// Host and Port address the fake.
func (s *Server) Host() string { return "127.0.0.1" }

// Port is the listening port.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// ClientTLSConfig trusts the fake's certificate.
func (s *Server) ClientTLSConfig() *tls.Config {
	return &tls.Config{RootCAs: s.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
}

// Verbs returns every command verb received, in order, across all sessions.
func (s *Server) Verbs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.verbs...)
}

// Raw returns every byte the client sent after TLS was up (or in cleartext).
func (s *Server) Raw() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.raw...)
}

// WriteAttempts counts state-changing verbs received.
func (s *Server) WriteAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeAttempts
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = true
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				_ = c.Close()
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
			}()
			_ = c.SetDeadline(time.Now().Add(30 * time.Second))
			s.session(c)
		}()
	}
}

// recorder copies everything read into the server's raw log.
type recorder struct {
	s *Server
	r io.Reader
}

func (r recorder) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.s.mu.Lock()
		r.s.raw = append(r.s.raw, p[:n]...)
		r.s.mu.Unlock()
	}
	return n, err
}

func (s *Server) caps(tlsUp, authed bool) string {
	var b strings.Builder
	impl := s.opts.Implementation
	if impl == "" {
		impl = "Dovecot Pigeonhole"
	}
	b.WriteString("\"IMPLEMENTATION\" " + msQuote(impl) + "\r\n")
	fmt.Fprintf(&b, "\"SIEVE\" \"%s\"\r\n", s.opts.Sieve)
	if authed {
		b.WriteString("\"MAXREDIRECTS\" \"4\"\r\n")
	}
	b.WriteString("\"NOTIFY\" \"mailto\"\r\n")
	if !authed {
		if tlsUp || s.opts.TLS == NoTLS {
			fmt.Fprintf(&b, "\"SASL\" \"%s\"\r\n", s.opts.SASL)
		} else {
			b.WriteString("\"SASL\" \"\"\r\n")
		}
		if !tlsUp && s.opts.TLS == StartTLS {
			b.WriteString("\"STARTTLS\"\r\n")
		}
	}
	if s.opts.MaxScriptSize > 0 {
		fmt.Fprintf(&b, "\"MAXSCRIPTSIZE\" \"%d\"\r\n", s.opts.MaxScriptSize)
	}
	b.WriteString("\"VERSION\" \"1.0\"\r\n")
	return b.String()
}

func (s *Server) session(c net.Conn) {
	conn := c
	tlsUp := false
	if s.opts.TLS == ImplicitTLS {
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{s.cert}})
		if err := tc.Handshake(); err != nil {
			return
		}
		conn, tlsUp = tc, true
	}
	br := bufio.NewReader(recorder{s: s, r: conn})
	w := func(str string) { _, _ = io.WriteString(conn, str) }
	if s.opts.Hostile == EndlessGreeting {
		pad := strings.Repeat("a", 1024)
		stream(conn, func(int) string { return "\"X-PAD\" \"" + pad + "\"\r\n" })
		return
	}
	w(s.caps(tlsUp, false) + "OK \"Dovecot ready.\"\r\n")

	authed := false
	for {
		verb, args, err := readCommand(br)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.verbs = append(s.verbs, verb)
		if writeVerbs[verb] {
			s.writeAttempts++
		}
		s.mu.Unlock()

		switch {
		case verb == "STARTTLS" && !tlsUp && s.opts.TLS == StartTLS:
			w("OK \"Begin TLS negotiation now.\"\r\n")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{s.cert}})
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, tlsUp = tc, true
			br = bufio.NewReader(recorder{s: s, r: conn})
			w(s.caps(true, false) + "OK \"TLS negotiation successful.\"\r\n")
		case verb == "AUTHENTICATE" && !authed:
			if !tlsUp && s.opts.TLS != NoTLS {
				w("NO \"Plaintext authentication disallowed on non-secure (SSL/TLS) connections.\"\r\n")
				continue
			}
			if len(args) < 2 || !strings.EqualFold(args[0], "PLAIN") {
				w("NO \"Unsupported mechanism.\"\r\n")
				continue
			}
			raw, _ := base64.StdEncoding.DecodeString(args[1])
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 && parts[1] == s.opts.Username && parts[2] == s.opts.Password {
				authed = true
				w("OK \"Logged in.\"\r\n")
			} else {
				w("NO " + s.errText("Authentication failed.") + "\r\n")
			}
		case verb == "CAPABILITY":
			w(s.caps(tlsUp, authed) + "OK \"Capability completed.\"\r\n")
		case verb == "LOGOUT" && s.opts.ErrorText != "":
			w("NO " + msQuote(s.opts.ErrorText) + "\r\n")
			return
		case verb == "LOGOUT":
			w("OK \"Logout completed.\"\r\n")
			return
		case !authed:
			w("NO \"Error: Command not permitted in this state.\"\r\n")
		case verb == "LISTSCRIPTS" && s.opts.Hostile == EndlessListScripts:
			stream(conn, func(i int) string { return fmt.Sprintf("\"script-%d\"\r\n", i) })
			return
		case verb == "GETSCRIPT" && s.opts.Hostile == EndlessSpaces:
			spaces := strings.Repeat(" ", 4096)
			stream(conn, func(int) string { return spaces })
			return
		case verb == "LISTSCRIPTS":
			var b strings.Builder
			for _, sc := range s.Scripts() {
				b.WriteString(msQuote(sc.Name))
				if sc.Active {
					b.WriteString(" ACTIVE")
				}
				b.WriteString("\r\n")
			}
			w(b.String() + "OK \"Listscripts completed.\"\r\n")
		case verb == "GETSCRIPT" && len(args) == 1:
			found := false
			for _, sc := range s.Scripts() {
				if sc.Name == args[0] && sc.Deny {
					w("NO " + s.errText("Denied.") + "\r\n")
					found = true
					break
				}
				if sc.Name == args[0] {
					w(fmt.Sprintf("{%d}\r\n%s\r\nOK \"Getscript completed.\"\r\n", len(sc.Body), sc.Body))
					found = true
					break
				}
			}
			if !found {
				w(fmt.Sprintf("NO (NONEXISTENT) \"Sieve script `%s' not found\"\r\n", args[0]))
			}
		case s.opts.AllowWrites && writeVerbs[verb] && verb != "RENAMESCRIPT" && verb != "HAVESPACE":
			w(s.write(verb, args))
		default:
			w("NO \"sievetest: verb refused by the fake\"\r\n")
		}
	}
}

// readCommand reads one client command: an atom verb, then quoted strings or
// {n+}/{n} literals, ended by CRLF.
func readCommand(br *bufio.Reader) (string, []string, error) {
	var verb strings.Builder
	var args []string
	for {
		b, err := br.ReadByte()
		if err != nil {
			return "", nil, err
		}
		if b == ' ' || b == '\r' || b == '\n' {
			_ = br.UnreadByte()
			break
		}
		verb.WriteByte(b)
	}
	for {
		b, err := br.ReadByte()
		if err != nil {
			return "", nil, err
		}
		switch b {
		case ' ':
		case '\r':
		case '\n':
			return strings.ToUpper(verb.String()), args, nil
		case '"':
			var sb strings.Builder
			for {
				c, err := br.ReadByte()
				if err != nil {
					return "", nil, err
				}
				if c == '"' {
					break
				}
				if c == '\\' {
					if c, err = br.ReadByte(); err != nil {
						return "", nil, err
					}
				}
				sb.WriteByte(c)
			}
			args = append(args, sb.String())
		case '{':
			spec, err := br.ReadString('}')
			if err != nil {
				return "", nil, err
			}
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(spec, "}"), "+"))
			if err != nil || n < 0 || n > 1<<21 {
				return "", nil, fmt.Errorf("bad literal %q", spec)
			}
			if _, err := br.ReadString('\n'); err != nil {
				return "", nil, err
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(br, buf); err != nil {
				return "", nil, err
			}
			args = append(args, string(buf))
		default:
			var sb strings.Builder
			sb.WriteByte(b)
			for {
				c, err := br.ReadByte()
				if err != nil {
					return "", nil, err
				}
				if c == ' ' || c == '\r' || c == '\n' {
					_ = br.UnreadByte()
					break
				}
				sb.WriteByte(c)
			}
			args = append(args, sb.String())
		}
	}
}

func selfSigned(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("sievetest: key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sievetest"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("sievetest: cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("sievetest: parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// msQuote renders a name the way a ManageSieve server does (RFC 5804 §4): a
// quoted string escaping only backslash and double quote — every other byte,
// control characters included, goes on the wire as itself — or a literal when
// it holds a line break.
func msQuote(s string) string {
	if strings.ContainsAny(s, "\r\n") {
		return fmt.Sprintf("{%d}\r\n%s", len(s), s)
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// errText is the NO text: Options.ErrorText when set, else def.
func (s *Server) errText(def string) string {
	if s.opts.ErrorText != "" {
		return msQuote(s.opts.ErrorText)
	}
	return msQuote(def)
}

// Scripts returns a copy of the scripts the fake holds now.
func (s *Server) Scripts() []Script {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Script(nil), s.opts.Scripts...)
}

// write performs one state-changing verb the way Pigeonhole does, returning
// the whole response.
func (s *Server) write(verb string, args []string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	find := func(name string) int {
		for i, sc := range s.opts.Scripts {
			if sc.Name == name {
				return i
			}
		}
		return -1
	}
	checkBody := func(body string) string {
		if s.opts.MaxScriptSize > 0 && len(body) > s.opts.MaxScriptSize {
			return "NO (QUOTA/MAXSIZE) \"Script is too large (max " + strconv.Itoa(s.opts.MaxScriptSize) + " bytes).\"\r\n"
		}
		if s.opts.InvalidMarker != "" && strings.Contains(body, s.opts.InvalidMarker) {
			return "NO \"line 1: error: unknown command.\"\r\n"
		}
		return ""
	}
	switch verb {
	case "CHECKSCRIPT":
		if len(args) != 1 {
			return "NO \"Syntax error.\"\r\n"
		}
		if bad := checkBody(args[0]); bad != "" {
			return bad
		}
		return "OK \"Script is valid.\"\r\n"
	case "PUTSCRIPT":
		if len(args) != 2 {
			return "NO \"Syntax error.\"\r\n"
		}
		if bad := checkBody(args[1]); bad != "" {
			return bad
		}
		if i := find(args[0]); i >= 0 {
			s.opts.Scripts[i].Body = args[1]
		} else {
			s.opts.Scripts = append(s.opts.Scripts, Script{Name: args[0], Body: args[1]})
		}
		if s.opts.ActivateOnPut != "" {
			for i := range s.opts.Scripts {
				s.opts.Scripts[i].Active = s.opts.Scripts[i].Name == s.opts.ActivateOnPut
			}
			s.opts.ActivateOnPut = ""
		}
		return "OK \"PUTSCRIPT completed.\"\r\n"
	case "SETACTIVE":
		if len(args) != 1 {
			return "NO \"Syntax error.\"\r\n"
		}
		if args[0] != "" && find(args[0]) < 0 {
			return "NO (NONEXISTENT) \"Sieve script `" + args[0] + "' not found\"\r\n"
		}
		for i := range s.opts.Scripts {
			s.opts.Scripts[i].Active = s.opts.Scripts[i].Name == args[0]
		}
		return "OK \"Setactive completed.\"\r\n"
	case "DELETESCRIPT":
		if len(args) != 1 {
			return "NO \"Syntax error.\"\r\n"
		}
		i := find(args[0])
		if i < 0 {
			return "NO (NONEXISTENT) \"Sieve script `" + args[0] + "' not found\"\r\n"
		}
		if s.opts.Scripts[i].Active {
			return "NO (ACTIVE) \"Cannot delete active Sieve script.\"\r\n"
		}
		s.opts.Scripts = append(s.opts.Scripts[:i], s.opts.Scripts[i+1:]...)
		return "OK \"Deletescript completed.\"\r\n"
	}
	return "NO \"sievetest: verb refused by the fake\"\r\n"
}

// Activate makes name the active script ("" for none) from outside any
// session: the user switching filters in the provider's webmail.
func (s *Server) Activate(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.opts.Scripts {
		s.opts.Scripts[i].Active = s.opts.Scripts[i].Name == name
	}
}

// Replace sets a script's body from outside any session (an edit in webmail).
func (s *Server) Replace(name, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.opts.Scripts {
		if s.opts.Scripts[i].Name == name {
			s.opts.Scripts[i].Body = body
		}
	}
}

// VerbCount counts how many times verb was received.
func (s *Server) VerbCount(verb string) int {
	n := 0
	for _, v := range s.Verbs() {
		if v == verb {
			n++
		}
	}
	return n
}
