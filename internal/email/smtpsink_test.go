package email

// A minimal in-process SMTP sink: enough ESMTP (EHLO, AUTH PLAIN/LOGIN,
// MAIL, RCPT, DATA, RSET, QUIT) for go-mail's real client to deliver
// messages, recording everything for assertions. Deliberately stdlib-only.

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type sinkMessage struct {
	From string
	To   []string
	Data string // raw RFC 5322 bytes as received (dot-unstuffed)
}

type smtpSink struct {
	ln net.Listener

	mu   sync.Mutex
	msgs []sinkMessage
	auth []string // raw AUTH command lines observed
}

func newSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("smtp sink listen: %v", err)
	}
	s := &smtpSink{ln: ln}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *smtpSink) port() int32 {
	return int32(s.ln.Addr().(*net.TCPAddr).Port)
}

func (s *smtpSink) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *smtpSink) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReader(conn)
	w := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

	w("220 sink ESMTP")
	var (
		from string
		rcpt []string
	)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(verb, "EHLO"), strings.HasPrefix(verb, "HELO"):
			w("250-sink")
			w("250-AUTH PLAIN LOGIN")
			w("250 8BITMIME")
		case strings.HasPrefix(verb, "AUTH PLAIN"):
			s.mu.Lock()
			s.auth = append(s.auth, line)
			s.mu.Unlock()
			if strings.TrimSpace(line[len("AUTH PLAIN"):]) == "" {
				w("334 ")
				if _, err := r.ReadString('\n'); err != nil {
					return
				}
			}
			w("235 2.7.0 ok")
		case strings.HasPrefix(verb, "AUTH LOGIN"):
			s.mu.Lock()
			s.auth = append(s.auth, line)
			s.mu.Unlock()
			w("334 VXNlcm5hbWU6")
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
			w("334 UGFzc3dvcmQ6")
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
			w("235 2.7.0 ok")
		case strings.HasPrefix(verb, "MAIL FROM:"):
			from = strings.Trim(line[len("MAIL FROM:"):], " <>")
			if i := strings.IndexByte(from, '>'); i >= 0 {
				from = from[:i]
			}
			if i := strings.IndexByte(from, ' '); i >= 0 {
				from = from[:i] // strip BODY=8BITMIME etc.
			}
			from = strings.Trim(from, "<>")
			w("250 ok")
		case strings.HasPrefix(verb, "RCPT TO:"):
			to := strings.Trim(strings.TrimSpace(line[len("RCPT TO:"):]), "<>")
			rcpt = append(rcpt, to)
			w("250 ok")
		case verb == "DATA":
			w("354 go ahead")
			var data strings.Builder
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					return
				}
				trimmed := strings.TrimRight(dl, "\r\n")
				if trimmed == "." {
					break
				}
				data.WriteString(strings.TrimPrefix(trimmed, ".")) // dot-unstuff
				data.WriteString("\r\n")
			}
			s.mu.Lock()
			s.msgs = append(s.msgs, sinkMessage{From: from, To: rcpt, Data: data.String()})
			s.mu.Unlock()
			from, rcpt = "", nil
			w("250 accepted")
		case verb == "RSET", verb == "NOOP":
			from, rcpt = "", nil
			w("250 ok")
		case verb == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

func (s *smtpSink) messages() []sinkMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkMessage(nil), s.msgs...)
}

// waitForMessages polls until n messages arrived or the timeout expires.
func (s *smtpSink) waitForMessages(t *testing.T, n int, timeout time.Duration) []sinkMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		msgs := s.messages()
		if len(msgs) >= n {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("smtp sink: wanted %d messages, have %d after %s", n, len(msgs), timeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// header extracts a header value from a raw sink message (first match,
// unfolded).
func (m sinkMessage) header(name string) string {
	lines := strings.Split(m.Data, "\r\n")
	prefix := strings.ToLower(name) + ":"
	for i, line := range lines {
		if line == "" {
			return "" // end of headers
		}
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			val := strings.TrimSpace(line[len(prefix):])
			// Unfold continuation lines.
			for j := i + 1; j < len(lines); j++ {
				if strings.HasPrefix(lines[j], " ") || strings.HasPrefix(lines[j], "\t") {
					val += " " + strings.TrimSpace(lines[j])
				} else {
					break
				}
			}
			return val
		}
	}
	return ""
}
