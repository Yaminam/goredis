package command

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"goredis/internal/protocol"
)

// infoOrder is the order of sections in INFO output.
var infoOrder = []string{"server", "clients", "memory", "persistence", "stats", "keyspace"}

// cmdInfo implements INFO [section ...]. Output is a bulk string of
// "# Section" headers and "key:value" lines separated by CRLF, the format
// Redis tooling expects. Field names are goredis-specific where Redis has no
// equivalent; this is not a byte-for-byte copy of Redis INFO.
func cmdInfo(c *call) protocol.Reply {
	want := map[string]bool{}
	for _, a := range c.args[1:] {
		want[strings.ToLower(string(a))] = true
	}
	all := len(want) == 0 || want["all"] || want["default"] || want["everything"]

	var b strings.Builder
	for _, section := range infoOrder {
		if !all && !want[section] {
			continue
		}
		fields := c.e.infoSection(section)
		if fields == nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString("# " + strings.ToUpper(section[:1]) + section[1:] + "\r\n")
		for _, f := range fields {
			b.WriteString(f.Key + ":" + f.Value + "\r\n")
		}
	}
	return protocol.BulkString(b.String())
}

func (e *Engine) infoSection(name string) []InfoField {
	var fields []InfoField
	switch name {
	case "server":
		uptime := time.Since(e.started)
		fields = []InfoField{
			{"goredis_version", e.opts.Version},
			{"go_version", runtime.Version()},
			{"os", runtime.GOOS + " " + runtime.GOARCH},
			{"process_id", strconv.Itoa(processID())},
			{"uptime_in_seconds", strconv.FormatInt(int64(uptime.Seconds()), 10)},
			{"uptime_in_days", strconv.FormatInt(int64(uptime.Hours()/24), 10)},
		}
	case "memory":
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		used := e.store.Stats().UsedBytes
		fields = []InfoField{
			{"used_memory_dataset", strconv.FormatInt(used, 10)},
			{"used_memory_dataset_human", humanBytes(used)},
			{"go_heap_inuse", strconv.FormatUint(ms.HeapInuse, 10)},
			{"go_heap_inuse_human", humanBytes(int64(ms.HeapInuse))},
			{"go_sys", strconv.FormatUint(ms.Sys, 10)},
			{"maxmemory", strconv.FormatInt(e.opts.MaxMemory, 10)},
			{"maxmemory_human", humanBytes(e.opts.MaxMemory)},
			{"maxmemory_policy", "noeviction"},
		}
	case "stats":
		st := e.store.Stats()
		fields = []InfoField{
			{"total_commands_processed", strconv.FormatUint(e.commandsProcessed.Load(), 10)},
			{"expired_keys", strconv.FormatUint(st.ExpiredKeys, 10)},
			{"deleted_keys", strconv.FormatUint(st.DeletedKeys, 10)},
		}
	case "keyspace":
		if n := e.store.Len(); n > 0 {
			fields = []InfoField{{"db0", fmt.Sprintf("keys=%d", n)}}
		} else {
			fields = []InfoField{}
		}
	}
	if extra, ok := e.opts.InfoSections[name]; ok {
		fields = append(fields, extra()...)
	}
	return fields
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

// cmdCommand implements COMMAND, COMMAND COUNT, COMMAND LIST,
// COMMAND INFO name... and COMMAND DOCS (always empty). This is enough for
// redis-cli and client libraries that probe the server on connect.
func cmdCommand(c *call) protocol.Reply {
	if len(c.args) == 1 {
		out := make([]protocol.Reply, 0, len(c.e.ordered))
		for _, sp := range c.e.ordered {
			out = append(out, commandInfo(sp))
		}
		return protocol.Array(out...)
	}
	switch strings.ToUpper(string(c.args[1])) {
	case "COUNT":
		return protocol.Int(int64(len(c.e.ordered)))
	case "LIST":
		out := make([]protocol.Reply, 0, len(c.e.ordered))
		for _, sp := range c.e.ordered {
			out = append(out, protocol.BulkString(sp.name))
		}
		return protocol.Array(out...)
	case "INFO":
		out := make([]protocol.Reply, 0, len(c.args)-2)
		for _, a := range c.args[2:] {
			if sp, ok := c.e.table[strings.ToLower(string(a))]; ok {
				out = append(out, commandInfo(sp))
			} else {
				out = append(out, protocol.NullArray())
			}
		}
		return protocol.Array(out...)
	case "DOCS":
		return protocol.Array()
	}
	return protocol.Err("ERR unknown subcommand '" + truncate(c.args[1]) + "'. Try COMMAND HELP.")
}

func commandInfo(sp *spec) protocol.Reply {
	flags := make([]protocol.Reply, len(sp.flags))
	for i, f := range sp.flags {
		flags[i] = protocol.Simple(f)
	}
	return protocol.Array(
		protocol.BulkString(sp.name),
		protocol.Int(int64(sp.arity)),
		protocol.Array(flags...),
		protocol.Int(int64(sp.firstKey)),
		protocol.Int(int64(sp.lastKey)),
		protocol.Int(int64(sp.step)),
	)
}
