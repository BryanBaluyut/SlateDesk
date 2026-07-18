package email

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/jobs"
	"github.com/BryanBaluyut/slatedesk/internal/storage"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

var testSeq atomic.Int64

// uniq returns a per-test unique lowercase token for addresses/names.
func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, testSeq.Add(1), time.Now().UnixNano()%1_000_000)
}

// newTestEngine builds an Engine on the shared test DB, a temp blob dir,
// and the shared secrets box. No river client is wired by default (tests
// wire an insert-only or worker client as needed).
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	blobs, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("local storage: %v", err)
	}
	return NewEngine(testPool, NewAuth(testPool, testBox), blobs)
}

// wireInsertOnlyRiver attaches an insert-only river client to the engine:
// enqueues commit but nothing runs — job rows can be asserted at rest.
func wireInsertOnlyRiver(t *testing.T, e *Engine) {
	t.Helper()
	jc, err := jobs.NewClient(testPool, nil)
	if err != nil {
		t.Fatalf("insert-only river client: %v", err)
	}
	e.SetRiver(jc.River())
}

// startWorkerRiver registers the email workers for engine, starts a River
// client working them, and stops it on cleanup. Call clearJobs first if
// stale jobs from earlier tests could interfere.
func startWorkerRiver(t *testing.T, e *Engine) {
	t.Helper()
	reg := jobs.NewRegistry()
	if err := RegisterWorkers(reg, e); err != nil {
		t.Fatalf("register email workers: %v", err)
	}
	jc, err := jobs.NewClient(testPool, reg)
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	e.SetRiver(jc.River())
	if err := jc.Start(context.Background()); err != nil {
		t.Fatalf("start river: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := jc.Stop(ctx); err != nil {
			t.Errorf("stop river: %v", err)
		}
	})
}

// clearJobs removes all river jobs so a worker client started by this test
// cannot pick up leftovers from earlier tests (tests share one DB and the
// job kinds are global).
func clearJobs(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), "DELETE FROM river_job"); err != nil {
		t.Fatalf("clear river_job: %v", err)
	}
}

// jobRow is a river_job row snapshot for assertions.
type jobRow struct {
	Kind  string
	State string
	Args  map[string]any
}

// listJobs returns river jobs of the given kind, oldest first.
func listJobs(t *testing.T, kind string) []jobRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		"SELECT kind, state::text, args FROM river_job WHERE kind = $1 ORDER BY id", kind)
	if err != nil {
		t.Fatalf("query river_job: %v", err)
	}
	defer rows.Close()
	var out []jobRow
	for rows.Next() {
		var r jobRow
		var args []byte
		if err := rows.Scan(&r.Kind, &r.State, &args); err != nil {
			t.Fatalf("scan river_job: %v", err)
		}
		if err := json.Unmarshal(args, &r.Args); err != nil {
			t.Fatalf("decode job args: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// mailboxOpts tweaks the makeMailbox fixture.
type mailboxOpts struct {
	address     string
	imapHost    string
	imapPort    int32
	smtpHost    string
	smtpPort    int32
	autoAck     bool
	signature   string
	displayName string
	password    string
}

// makeMailbox inserts an active basic-auth mailbox with tls_mode=none
// (pointing at in-process test servers) and returns the row.
func makeMailbox(t *testing.T, opts mailboxOpts) store.Mailbox {
	t.Helper()
	if opts.address == "" {
		opts.address = uniq("support") + "@slatedesk.test"
	}
	if opts.imapHost == "" {
		opts.imapHost = "127.0.0.1"
	}
	if opts.imapPort == 0 {
		opts.imapPort = 1 // unused unless a supervisor connects
	}
	if opts.smtpHost == "" {
		opts.smtpHost = "127.0.0.1"
	}
	if opts.smtpPort == 0 {
		opts.smtpPort = 1
	}
	if opts.password == "" {
		opts.password = "secret"
	}
	if opts.displayName == "" {
		opts.displayName = "SlateDesk Support"
	}
	enc, err := EncryptCredentials(testBox, Credentials{Password: opts.password})
	if err != nil {
		t.Fatalf("encrypt credentials: %v", err)
	}
	username := opts.address
	mb, err := store.New(testPool).CreateMailbox(context.Background(), store.CreateMailboxParams{
		Name:            "Test mailbox " + opts.address,
		EmailAddress:    opts.address,
		Active:          true,
		AuthKind:        store.MailboxAuthKindBasic,
		ImapHost:        opts.imapHost,
		ImapPort:        opts.imapPort,
		ImapTlsMode:     store.MailTlsModeNone,
		ImapUsername:    username,
		SmtpHost:        opts.smtpHost,
		SmtpPort:        opts.smtpPort,
		SmtpTlsMode:     store.MailTlsModeNone,
		SmtpUsername:    username,
		CredentialsEnc:  enc,
		FromDisplayName: opts.displayName,
		Signature:       opts.signature,
		AutoAckEnabled:  opts.autoAck,
	})
	if err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	return mb
}

// makeUser inserts a user with the given role and returns it.
func makeUser(t *testing.T, role store.UserRole) store.User {
	t.Helper()
	u, err := store.New(testPool).CreateUser(context.Background(), store.CreateUserParams{
		Email: uniq(string(role)) + "@example.test",
		Name:  "Test " + string(role),
		Role:  role,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// rawMessage builds an RFC 5322 message. headers are literal lines
// (without CRLF); body is appended after a blank line. CRLF line endings
// throughout.
func rawMessage(headers []string, body string) []byte {
	var b strings.Builder
	for _, h := range headers {
		b.WriteString(h)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String())
}

// getTicket loads a ticket row.
func getTicket(t *testing.T, id uuid.UUID) store.Ticket {
	t.Helper()
	row, err := store.New(testPool).GetTicketForUpdate(context.Background(), id)
	if err != nil {
		t.Fatalf("load ticket %s: %v", id, err)
	}
	return row
}

// getArticle loads an article row.
func getArticle(t *testing.T, id uuid.UUID) store.Article {
	t.Helper()
	a, err := store.New(testPool).GetArticle(context.Background(), id)
	if err != nil {
		t.Fatalf("load article %s: %v", id, err)
	}
	return a
}

// textVal unwraps pgtype.Text for assertions.
func textVal(v pgtype.Text) string {
	if !v.Valid {
		return ""
	}
	return v.String
}
