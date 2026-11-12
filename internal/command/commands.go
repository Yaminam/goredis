package command

import (
	"math"
	"strconv"
	"strings"

	"goredis/internal/protocol"
	"goredis/internal/store"
)

// spec describes one command. Arity follows the Redis convention: a positive
// number is the exact argument count including the command name; a negative
// number -N means "at least N". firstKey/lastKey/step describe key positions
// for COMMAND INFO (lastKey -1 means "through the last argument").
type spec struct {
	name       string
	arity      int
	write      bool // modifies data; refused while persistence is failing
	noAuth     bool // allowed before AUTH
	replayable bool // may appear in the append-only file
	flags      []string
	firstKey   int
	lastKey    int
	step       int
	handler    func(c *call) protocol.Reply
}

func commandTable() []*spec {
	return []*spec{
		{name: "ping", arity: -1, flags: []string{"fast"}, handler: cmdPing},
		{name: "echo", arity: 2, flags: []string{"fast"}, handler: cmdEcho},
		{name: "get", arity: 2, flags: []string{"readonly", "fast"}, firstKey: 1, lastKey: 1, step: 1, handler: cmdGet},
		{name: "set", arity: -3, write: true, replayable: true, flags: []string{"write", "denyoom"}, firstKey: 1, lastKey: 1, step: 1, handler: cmdSet},
		{name: "del", arity: -2, write: true, replayable: true, flags: []string{"write"}, firstKey: 1, lastKey: -1, step: 1, handler: cmdDel},
		{name: "exists", arity: -2, flags: []string{"readonly", "fast"}, firstKey: 1, lastKey: -1, step: 1, handler: cmdExists},
		{name: "expire", arity: -3, write: true, flags: []string{"write", "fast"}, firstKey: 1, lastKey: 1, step: 1, handler: expireHandler("expire", 1000, false)},
		{name: "pexpire", arity: -3, write: true, flags: []string{"write", "fast"}, firstKey: 1, lastKey: 1, step: 1, handler: expireHandler("pexpire", 1, false)},
		{name: "expireat", arity: -3, write: true, flags: []string{"write", "fast"}, firstKey: 1, lastKey: 1, step: 1, handler: expireHandler("expireat", 1000, true)},
		{name: "pexpireat", arity: -3, write: true, replayable: true, flags: []string{"write", "fast"}, firstKey: 1, lastKey: 1, step: 1, handler: expireHandler("pexpireat", 1, true)},
		{name: "ttl", arity: 2, flags: []string{"readonly", "fast"}, firstKey: 1, lastKey: 1, step: 1, handler: ttlHandler(false)},
		{name: "pttl", arity: 2, flags: []string{"readonly", "fast"}, firstKey: 1, lastKey: 1, step: 1, handler: ttlHandler(true)},
		{name: "persist", arity: 2, write: true, replayable: true, flags: []string{"write", "fast"}, firstKey: 1, lastKey: 1, step: 1, handler: cmdPersist},
		{name: "dbsize", arity: 1, flags: []string{"readonly", "fast"}, handler: cmdDBSize},
		{name: "flushdb", arity: -1, write: true, replayable: true, flags: []string{"write"}, handler: cmdFlushDB},
		{name: "info", arity: -1, flags: []string{"loading", "stale"}, handler: cmdInfo},
		{name: "command", arity: -1, flags: []string{"loading", "stale"}, handler: cmdCommand},
		{name: "auth", arity: -2, noAuth: true, flags: []string{"noscript", "loading", "stale", "fast", "no_auth"}, handler: cmdAuth},
		{name: "select", arity: 2, flags: []string{"loading", "stale", "fast"}, handler: cmdSelect},
		{name: "quit", arity: -1, noAuth: true, flags: []string{"fast", "no_auth"}, handler: cmdQuit},
		{name: "bgrewriteaof", arity: 1, flags: []string{"admin", "noscript"}, handler: cmdBgRewriteAOF},
	}
}

func cmdPing(c *call) protocol.Reply {
	switch len(c.args) {
	case 1:
		return protocol.Simple("PONG")
	case 2:
		return protocol.Bulk(c.args[1])
	}
	return wrongArgs("ping")
}

func cmdEcho(c *call) protocol.Reply { return protocol.Bulk(c.args[1]) }

func cmdGet(c *call) protocol.Reply {
	var v store.Value
	var ok bool
	c.e.store.View(func(tx *store.ReadTx) { v, ok = tx.Get(c.args[1]) })
	if !ok {
		return protocol.NullBulk()
	}
	return protocol.Bulk(v.Data)
}

// expiry kinds for SET options.
const (
	expNone = iota
	expRelMs
	expAbsMs
	expKeep
)

// cmdSet implements SET key value [NX|XX] [GET] [EX s|PX ms|EXAT s|PXAT ms|KEEPTTL].
//
// The AOF record is always the resolved form "SET key value [PXAT ms]": the
// NX/XX condition has already been decided and relative TTLs have been
// turned into absolute times, so replay is deterministic.
func cmdSet(c *call) protocol.Reply {
	var nx, xx, get bool
	kind := expNone
	var n int64
	args := c.args
	for i := 3; i < len(args); i++ {
		opt := strings.ToUpper(string(args[i]))
		switch opt {
		case "NX":
			if xx || nx {
				return errSyntax
			}
			nx = true
		case "XX":
			if xx || nx {
				return errSyntax
			}
			xx = true
		case "GET":
			if get {
				return errSyntax
			}
			get = true
		case "KEEPTTL":
			if kind != expNone {
				return errSyntax
			}
			kind = expKeep
		case "EX", "PX", "EXAT", "PXAT":
			if kind != expNone || i+1 >= len(args) {
				return errSyntax
			}
			i++
			v, ok := protocol.ParseInt(args[i])
			if !ok {
				return errNotInteger
			}
			if v <= 0 {
				return invalidExpire("set")
			}
			if opt == "EX" || opt == "EXAT" {
				if v > math.MaxInt64/1000 {
					return invalidExpire("set")
				}
				v *= 1000
			}
			n = v
			if opt == "EX" || opt == "PX" {
				kind = expRelMs
			} else {
				kind = expAbsMs
			}
		default:
			return errSyntax
		}
	}

	// maxmemory uses the noeviction policy: refuse writes that would grow
	// the dataset once it is over the limit. Checked outside the lock; the
	// limit is approximate by design.
	if !c.replay && c.e.opts.MaxMemory > 0 && c.e.store.Stats().UsedBytes >= c.e.opts.MaxMemory {
		return protocol.Err("OOM command not allowed when used memory > 'maxmemory'.")
	}

	key, value := args[1], args[2]
	var reply protocol.Reply
	c.e.store.Update(func(tx *store.Tx) {
		old, exists := tx.Get(key)
		if (nx && exists) || (xx && !exists) {
			if get && exists {
				reply = protocol.Bulk(old.Data)
			} else {
				reply = protocol.NullBulk()
			}
			return
		}
		var expireAt int64
		switch kind {
		case expRelMs:
			if n > math.MaxInt64-tx.Now() {
				reply = invalidExpire("set")
				return
			}
			expireAt = tx.Now() + n
		case expAbsMs:
			expireAt = n
		case expKeep:
			if exists {
				expireAt = old.ExpireAt
			}
		}
		tx.Set(key, value, expireAt)
		if expireAt != 0 {
			c.log([]byte("SET"), key, value, []byte("PXAT"), strconv.AppendInt(nil, expireAt, 10))
		} else {
			c.log([]byte("SET"), key, value)
		}
		switch {
		case !get:
			reply = protocol.OK
		case exists:
			reply = protocol.Bulk(old.Data)
		default:
			reply = protocol.NullBulk()
		}
	})
	return reply
}

func cmdDel(c *call) protocol.Reply {
	var n int64
	c.e.store.Update(func(tx *store.Tx) {
		deleted := make([][]byte, 0, len(c.args))
		deleted = append(deleted, []byte("DEL"))
		for _, k := range c.args[1:] {
			if tx.Delete(k) {
				n++
				deleted = append(deleted, k)
			}
		}
		if n > 0 {
			c.log(deleted...)
		}
	})
	return protocol.Int(n)
}

// cmdExists counts live keys; a key named twice is counted twice (Redis
// semantics).
func cmdExists(c *call) protocol.Reply {
	var n int64
	c.e.store.View(func(tx *store.ReadTx) {
		for _, k := range c.args[1:] {
			if tx.Exists(k) {
				n++
			}
		}
	})
	return protocol.Int(n)
}

// expireHandler builds EXPIRE, PEXPIRE, EXPIREAT and PEXPIREAT.
// unit is 1000 for seconds and 1 for milliseconds; absolute selects the *AT
// variants. Options NX, XX, GT and LT follow Redis 7 semantics, where a key
// without a TTL counts as having an infinite one.
//
// The AOF record is "PEXPIREAT key ms", or "DEL key" when the new time is
// not in the future (which deletes the key).
func expireHandler(name string, unit int64, absolute bool) func(c *call) protocol.Reply {
	return func(c *call) protocol.Reply {
		var nx, xx, gt, lt bool
		for _, a := range c.args[3:] {
			switch opt := strings.ToUpper(string(a)); opt {
			case "NX":
				nx = true
			case "XX":
				xx = true
			case "GT":
				gt = true
			case "LT":
				lt = true
			default:
				return protocol.Err("ERR Unsupported option " + truncate(a))
			}
		}
		if nx && (xx || gt || lt) {
			return protocol.Err("ERR NX and XX, GT or LT options at the same time are not compatible")
		}
		if gt && lt {
			return protocol.Err("ERR GT and LT options at the same time are not compatible")
		}
		v, ok := protocol.ParseInt(c.args[2])
		if !ok {
			return errNotInteger
		}
		if unit == 1000 {
			if v > math.MaxInt64/1000 || v < math.MinInt64/1000 {
				return invalidExpire(name)
			}
			v *= 1000
		}

		key := c.args[1]
		var reply protocol.Reply
		c.e.store.Update(func(tx *store.Tx) {
			at := v
			if !absolute {
				if (v > 0 && v > math.MaxInt64-tx.Now()) || (v < 0 && v < math.MinInt64+tx.Now()) {
					reply = invalidExpire(name)
					return
				}
				at = tx.Now() + v
			}
			cur, exists := tx.Get(key)
			if !exists {
				reply = protocol.Int(0)
				return
			}
			switch {
			case nx && cur.ExpireAt != 0,
				xx && cur.ExpireAt == 0,
				gt && (cur.ExpireAt == 0 || at <= cur.ExpireAt),
				lt && cur.ExpireAt != 0 && at >= cur.ExpireAt:
				reply = protocol.Int(0)
				return
			}
			tx.SetExpireAt(key, at)
			if at <= tx.Now() {
				c.log([]byte("DEL"), key)
			} else {
				c.log([]byte("PEXPIREAT"), key, strconv.AppendInt(nil, at, 10))
			}
			reply = protocol.Int(1)
		})
		return reply
	}
}

// ttlHandler builds TTL and PTTL: -2 if the key does not exist, -1 if it has
// no expiry, otherwise the remaining time (TTL rounds to the nearest second,
// as Redis does).
func ttlHandler(ms bool) func(c *call) protocol.Reply {
	return func(c *call) protocol.Reply {
		var v store.Value
		var ok bool
		var now int64
		c.e.store.View(func(tx *store.ReadTx) {
			v, ok = tx.Get(c.args[1])
			now = tx.Now()
		})
		switch {
		case !ok:
			return protocol.Int(-2)
		case v.ExpireAt == 0:
			return protocol.Int(-1)
		}
		left := max(v.ExpireAt-now, 0)
		if ms {
			return protocol.Int(left)
		}
		return protocol.Int((left + 500) / 1000)
	}
}

func cmdPersist(c *call) protocol.Reply {
	var removed bool
	c.e.store.Update(func(tx *store.Tx) {
		if removed = tx.Persist(c.args[1]); removed {
			c.log([]byte("PERSIST"), c.args[1])
		}
	})
	if removed {
		return protocol.Int(1)
	}
	return protocol.Int(0)
}

func cmdDBSize(c *call) protocol.Reply { return protocol.Int(int64(c.e.store.Len())) }

// cmdFlushDB removes every key. ASYNC and SYNC are accepted for client
// compatibility; both run synchronously. The flush is logged, so it survives
// a restart: replay reaches the FLUSHDB record and clears what came before.
func cmdFlushDB(c *call) protocol.Reply {
	if len(c.args) > 2 {
		return errSyntax
	}
	if len(c.args) == 2 {
		if m := strings.ToUpper(string(c.args[1])); m != "ASYNC" && m != "SYNC" {
			return errSyntax
		}
	}
	c.e.store.Update(func(tx *store.Tx) {
		tx.Flush()
		c.log([]byte("FLUSHDB"))
	})
	return protocol.OK
}

func cmdAuth(c *call) protocol.Reply {
	pw := c.e.opts.Password
	if pw == "" {
		return protocol.Err("ERR AUTH <password> called without any password configured for the default user. Are you sure your configuration is correct?")
	}
	var user, pass []byte
	switch len(c.args) {
	case 2:
		pass = c.args[1]
	case 3:
		user, pass = c.args[1], c.args[2]
	default:
		return errSyntax
	}
	// Evaluate both checks before branching so timing doesn't reveal
	// which one failed.
	okUser := user == nil || string(user) == "default"
	okPass := checkPassword(pw, pass)
	if !okUser || !okPass {
		c.sess.authenticated = false
		return protocol.Err("WRONGPASS invalid username-password pair or user is disabled.")
	}
	c.sess.authenticated = true
	return protocol.OK
}

// cmdSelect accepts only database 0. Many client libraries send SELECT 0 on
// connect; supporting it keeps them working.
func cmdSelect(c *call) protocol.Reply {
	n, ok := protocol.ParseInt(c.args[1])
	if !ok {
		return errNotInteger
	}
	if n != 0 {
		return protocol.Err("ERR DB index is out of range")
	}
	return protocol.OK
}

func cmdQuit(c *call) protocol.Reply {
	c.sess.Quit = true
	return protocol.OK
}

func cmdBgRewriteAOF(c *call) protocol.Reply {
	if c.e.opts.AOF == nil {
		return protocol.Err("ERR append-only file persistence is disabled")
	}
	if err := c.e.opts.AOF.StartRewrite(); err != nil {
		return protocol.Err("ERR " + err.Error())
	}
	return protocol.Simple("Background append only file rewriting started")
}
