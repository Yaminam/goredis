// Package command maps parsed client commands to store operations and RESP
// replies. It owns command names, arity checks, argument validation,
// authentication, the exact reply format of every supported command, and the
// records written to the append-only file.
package command

import (
	"crypto/subtle"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"goredis/internal/protocol"
	"goredis/internal/store"
)

// Logger is the persistence hook. The engine calls Append while holding the
// store's write lock, so the order of records in the log always matches the
// order in which changes were applied in memory. Append must therefore only
// buffer in memory and never block on disk I/O. Wait is called after the lock
// is released and blocks until the record is as durable as the configured
// fsync policy promises.
type Logger interface {
	// Err returns a non-nil error while persistence is failing. Write
	// commands are refused (MISCONF) until it clears.
	Err() error
	// Append buffers one command record and returns its sequence number.
	Append(args ...[]byte) uint64
	// Wait blocks until record seq has been persisted per the fsync policy.
	Wait(seq uint64) error
	// StartRewrite begins a background compaction of the log.
	StartRewrite() error
}

// Recorder receives per-command observations, typically for metrics. The
// name is always a known command name or "unknown", never client input, so
// it is safe to use as a metric label.
type Recorder interface {
	ObserveCommand(name string, d time.Duration, failed bool)
}

// InfoField is one "key:value" line of an INFO section.
type InfoField struct{ Key, Value string }

// Options configures an Engine. The zero value is a valid in-memory engine
// with no authentication, persistence or metrics.
type Options struct {
	// Password enables AUTH when non-empty.
	Password string
	// DisabledCommands are refused even for authenticated clients.
	DisabledCommands []string
	// MaxMemory rejects SET once the estimated dataset size exceeds it.
	// Zero means unlimited.
	MaxMemory int64
	// AOF receives write records. Nil disables persistence.
	AOF Logger
	// Recorder receives command metrics. Nil disables them.
	Recorder Recorder
	// Version is reported by INFO.
	Version string
	// StartTime is used for uptime in INFO. Zero means time.Now().
	StartTime time.Time
	// InfoSections adds sections to INFO (e.g. "clients", "persistence")
	// from parts of the system the engine does not know about.
	InfoSections map[string]func() []InfoField
}

// Session is per-connection state.
type Session struct {
	authenticated bool
	// Quit is set by QUIT; the server closes the connection after sending
	// the reply.
	Quit bool
}

// NewSession returns state for a new client connection.
func NewSession() *Session { return &Session{} }

// Engine executes commands against a store. It is safe for concurrent use;
// each command runs in a single store transaction and is therefore atomic.
type Engine struct {
	store    *store.Store
	opts     Options
	table    map[string]*spec
	ordered  []*spec
	disabled map[string]bool
	started  time.Time

	commandsProcessed atomic.Uint64
}

// New returns an Engine backed by s.
func New(s *store.Store, opts Options) *Engine {
	e := &Engine{
		store:    s,
		opts:     opts,
		table:    make(map[string]*spec),
		disabled: make(map[string]bool),
		started:  opts.StartTime,
	}
	if e.started.IsZero() {
		e.started = time.Now()
	}
	for _, sp := range commandTable() {
		e.table[sp.name] = sp
		e.ordered = append(e.ordered, sp)
	}
	for _, name := range opts.DisabledCommands {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			e.disabled[name] = true
		}
	}
	return e
}

// call carries one command invocation through its handler.
type call struct {
	e      *Engine
	sess   *Session
	args   [][]byte
	replay bool
	seq    uint64 // last AOF record appended by this call, 0 if none
}

// log appends a record to the AOF. It must be called inside a store Update,
// right after the change it describes.
func (c *call) log(args ...[]byte) {
	if c.replay || c.e.opts.AOF == nil {
		return
	}
	c.seq = c.e.opts.AOF.Append(args...)
}

// Execute runs one client command. args[0] is the command name
// (case-insensitive). Failures are RESP error replies, never Go errors: a bad
// command is a normal outcome, not a server failure.
func (e *Engine) Execute(sess *Session, args [][]byte) protocol.Reply {
	start := time.Now()
	reply, name := e.execute(sess, args)
	e.commandsProcessed.Add(1)
	if e.opts.Recorder != nil {
		e.opts.Recorder.ObserveCommand(name, time.Since(start), reply.IsError())
	}
	return reply
}

func (e *Engine) execute(sess *Session, args [][]byte) (protocol.Reply, string) {
	if len(args) == 0 {
		return protocol.Err("ERR empty command"), "unknown"
	}
	sp, ok := e.table[strings.ToLower(string(args[0]))]
	if !ok {
		if e.opts.Password != "" && !sess.authenticated {
			return noAuth, "unknown"
		}
		return unknownCommand(args), "unknown"
	}
	if e.opts.Password != "" && !sess.authenticated && !sp.noAuth {
		return noAuth, sp.name
	}
	if e.disabled[sp.name] {
		return protocol.Err(fmt.Sprintf("ERR command '%s' is disabled by server configuration", strings.ToUpper(sp.name))), sp.name
	}
	if !arityOK(sp.arity, len(args)) {
		return wrongArgs(sp.name), sp.name
	}
	if sp.write && e.opts.AOF != nil {
		if err := e.opts.AOF.Err(); err != nil {
			return protocol.Err("MISCONF Errors writing to the AOF file: " + err.Error()), sp.name
		}
	}

	c := &call{e: e, sess: sess, args: args}
	reply := sp.handler(c)

	// The change is applied in memory; now wait (without holding the store
	// lock) until it is as durable as the fsync policy promises.
	if c.seq != 0 {
		if err := e.opts.AOF.Wait(c.seq); err != nil {
			return protocol.Err("ERR AOF write failed; the change is applied in memory but is not durable: " + err.Error()), sp.name
		}
	}
	return reply, sp.name
}

// Replay applies one record read back from the append-only file. Only the
// commands the engine itself writes to the log are accepted, and nothing is
// re-logged. Authentication, disabled commands and maxmemory do not apply:
// the log is trusted local state, and refusing a record would silently lose
// data at startup.
func (e *Engine) Replay(args [][]byte) error {
	if len(args) == 0 {
		return fmt.Errorf("empty record")
	}
	name := strings.ToLower(string(args[0]))
	sp, ok := e.table[name]
	if !ok || !sp.replayable {
		return fmt.Errorf("command %q is not valid in an append-only file", truncate(args[0]))
	}
	if !arityOK(sp.arity, len(args)) {
		return fmt.Errorf("wrong number of arguments for %q", name)
	}
	reply := sp.handler(&call{e: e, sess: &Session{authenticated: true}, args: args, replay: true})
	if reply.IsError() {
		return fmt.Errorf("%s: %s", name, reply.Str)
	}
	return nil
}

// CommandsProcessed returns the total number of commands executed.
func (e *Engine) CommandsProcessed() uint64 { return e.commandsProcessed.Load() }

var noAuth = protocol.Err("NOAUTH Authentication required.")

func arityOK(arity, n int) bool {
	if arity >= 0 {
		return n == arity
	}
	return n >= -arity
}

func wrongArgs(name string) protocol.Reply {
	return protocol.Err(fmt.Sprintf("ERR wrong number of arguments for '%s' command", name))
}

var (
	errSyntax     = protocol.Err("ERR syntax error")
	errNotInteger = protocol.Err("ERR value is not an integer or out of range")
)

func invalidExpire(cmd string) protocol.Reply {
	return protocol.Err(fmt.Sprintf("ERR invalid expire time in '%s' command", cmd))
}

// unknownCommand mirrors the Redis message, including a short preview of the
// arguments. The preview is truncated so a hostile client cannot make the
// server echo megabytes back.
func unknownCommand(args [][]byte) protocol.Reply {
	var b strings.Builder
	fmt.Fprintf(&b, "ERR unknown command '%s', with args beginning with: ", truncate(args[0]))
	for i, a := range args[1:] {
		if i == 4 {
			break
		}
		fmt.Fprintf(&b, "'%s' ", truncate(a))
	}
	return protocol.Err(b.String())
}

func truncate(b []byte) string {
	const max = 64
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

// checkPassword compares in constant time so response timing does not leak
// how much of a guess was correct.
func checkPassword(want string, got []byte) bool {
	return subtle.ConstantTimeCompare([]byte(want), got) == 1
}
