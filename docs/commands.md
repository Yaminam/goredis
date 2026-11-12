# Command compatibility

goredis implements a **subset** of Redis commands over RESP2. This page lists
exactly what is supported, how replies are encoded, and where behaviour
differs from Redis 7. Anything not listed here is not implemented and returns:

```
-ERR unknown command 'NAME', with args beginning with: ...
```

Command names are case-insensitive. All keys and values are binary-safe byte
strings. There is a single database (0) and a single data type (string).

## Supported commands

| Command | Reply | Notes |
|---------|-------|-------|
| `PING [message]` | `+PONG`, or the message as a bulk string | Same as Redis. |
| `ECHO message` | bulk string | Same as Redis. |
| `SET key value [NX\|XX] [GET] [EX s\|PX ms\|EXAT ts\|PXAT ms\|KEEPTTL]` | `+OK`; null bulk if NX/XX blocked the write; with `GET`, the old value or null | See [SET details](#set). |
| `GET key` | bulk string, or null bulk | Same as Redis. |
| `DEL key [key ...]` | integer: keys removed | Same as Redis. |
| `EXISTS key [key ...]` | integer | A key named twice is counted twice (same as Redis). |
| `EXPIRE key seconds [NX\|XX\|GT\|LT]` | `:1` set, `:0` not set or key missing | Redis 7 options. A non-positive TTL deletes the key and returns `:1`. |
| `PEXPIRE key ms [NX\|XX\|GT\|LT]` | same | |
| `EXPIREAT key unix-seconds [NX\|XX\|GT\|LT]` | same | A time in the past deletes the key. |
| `PEXPIREAT key unix-ms [NX\|XX\|GT\|LT]` | same | |
| `TTL key` | integer seconds; `-1` no expiry; `-2` no key | Rounded to the nearest second (`(ms+500)/1000`), as Redis does. |
| `PTTL key` | integer ms; `-1`; `-2` | |
| `PERSIST key` | `:1` TTL removed, `:0` otherwise | |
| `DBSIZE` | integer | Counts live keys only. See [differences](#differences-from-redis). |
| `FLUSHDB [ASYNC\|SYNC]` | `+OK` | Both modes run synchronously. Logged to the AOF. Can be disabled with `GOREDIS_DISABLED_COMMANDS=FLUSHDB`. |
| `INFO [section ...]` | bulk string | Sections: `server`, `clients`, `memory`, `persistence`, `stats`, `keyspace`. Field names are goredis-specific where Redis has no equivalent. |
| `COMMAND` | array of `[name, arity, flags, first-key, last-key, step]` | Also `COMMAND COUNT`, `COMMAND LIST`, `COMMAND INFO name ...`, `COMMAND DOCS` (always an empty array). |
| `AUTH password` / `AUTH default password` | `+OK` or `-WRONGPASS ...` | Only the `default` user exists. Returns an error if no password is configured. |
| `SELECT index` | `+OK` for `0` only | Other indexes return `-ERR DB index is out of range`. |
| `QUIT` | `+OK`, then the server closes the connection | |
| `BGREWRITEAOF` | `+Background append only file rewriting started` | Compacts the AOF. See [persistence.md](persistence.md). |

### SET

- `NX` and `XX` are mutually exclusive; giving both is `-ERR syntax error`.
- Only one of `EX`, `PX`, `EXAT`, `PXAT`, `KEEPTTL` may be given.
- `EX`/`PX` must be positive: `0` or a negative number is
  `-ERR invalid expire time in 'set' command`. A non-numeric value is
  `-ERR value is not an integer or out of range`. An expiry that would
  overflow a 64-bit millisecond timestamp is also rejected as invalid.
- `EXAT`/`PXAT` in the past are accepted and leave the key deleted.
- A plain `SET` clears any existing TTL. `KEEPTTL` keeps it.
- `GET` returns the previous value (or null). Combined with `NX`, a blocked
  write still returns the existing value.
- Unknown options are `-ERR syntax error`.
- When `maxmemory` is set and the estimated dataset size is at or above it,
  `SET` returns `-OOM command not allowed when used memory > 'maxmemory'.`
  (the `noeviction` policy; nothing is ever evicted).

### EXPIRE options

As in Redis 7, a key **without** a TTL is treated as having an infinite one:

| Option | Sets the TTL only if |
|--------|----------------------|
| `NX` | the key has no TTL |
| `XX` | the key has a TTL |
| `GT` | the new expiry is later than the current one (never for a key without a TTL) |
| `LT` | the new expiry is earlier than the current one (always for a key without a TTL) |

`NX` with any of `XX`/`GT`/`LT`, or `GT` with `LT`, returns an error.

## Error replies

| Situation | Reply |
|-----------|-------|
| Unknown command | `-ERR unknown command '<name>', with args beginning with: ...` (each argument truncated to 64 bytes) |
| Wrong argument count | `-ERR wrong number of arguments for '<cmd>' command` |
| Non-integer where an integer is expected | `-ERR value is not an integer or out of range` |
| Invalid option combination | `-ERR syntax error` |
| Not authenticated | `-NOAUTH Authentication required.` |
| Command disabled by config | `-ERR command 'FLUSHDB' is disabled by server configuration` |
| AOF failing (writes refused) | `-MISCONF Errors writing to the AOF file: <cause>` |
| AOF failed after the change was applied | `-ERR AOF write failed; the change is applied in memory but is not durable: <cause>` |
| Over maxmemory | `-OOM command not allowed when used memory > 'maxmemory'.` |
| Too many clients | `-ERR max number of clients reached` (connection then closed) |
| Malformed or oversized request | `-ERR Protocol error: <detail>` (connection then closed) |

## Protocol

- **RESP2 only.** Requests are arrays of bulk strings. Inline commands
  (`PING\r\n`, `SET k v\n`) are accepted for telnet/nc use; inline mode splits
  on spaces and tabs and does **not** support quoting.
- `HELLO` (RESP3) is not implemented. Clients that try `HELLO 3` get an
  unknown-command error; most clients (including go-redis and redis-py) then
  fall back to RESP2.
- Pipelining is fully supported. Replies come back in order, and the server
  batches them into as few writes as possible.

## Differences from Redis

| Area | goredis | Redis |
|------|---------|-------|
| Data types | strings only | strings, lists, hashes, sets, sorted sets, streams, ... |
| Databases | `0` only | 16 by default |
| `DBSIZE` | live keys only (expired-but-not-reclaimed keys excluded) | may include expired keys not yet reclaimed |
| `INFO` | goredis-specific fields (e.g. `goredis_version`, `go_heap_inuse`) | Redis fields |
| Persistence | AOF only (no RDB snapshots); AOF records are RESP arrays | RDB and/or AOF (multi-part AOF in Redis 7) |
| Users / ACL | single password (`default` user) | full ACL system |
| Eviction | `noeviction` only | LRU/LFU/random/TTL policies |
| Not implemented | transactions (`MULTI`/`EXEC`), pub/sub, Lua, replication, cluster, `KEYS`/`SCAN`, `INCR`, `APPEND`, `MSET`, `CLIENT`, `CONFIG`, `HELLO`, TLS | all supported |
