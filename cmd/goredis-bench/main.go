// Command goredis-bench is a load generator for goredis (or any RESP2
// server supporting GET/SET). It reports throughput, latency percentiles and
// server memory before and after the run, plus the conditions of the test so
// results can be reproduced.
//
//	goredis-bench -addr 127.0.0.1:6379 -clients 50 -requests 200000 -ratio 1:1
//
// With -pipeline N each client sends N commands per round trip; latency is
// then reported per round trip (batch), not per command.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"goredis/internal/protocol"
)

type options struct {
	addr      string
	password  string
	clients   int
	requests  int
	pipeline  int
	keyspace  int
	valueSize int
	gets      int // ratio numerator
	sets      int // ratio denominator
	prefill   bool
	timeout   time.Duration
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "goredis-bench:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	var o options
	var ratio string
	fs := flag.NewFlagSet("goredis-bench", flag.ContinueOnError)
	fs.StringVar(&o.addr, "addr", "127.0.0.1:6379", "server address")
	fs.StringVar(&o.password, "password", os.Getenv("GOREDIS_BENCH_PASSWORD"), "AUTH password (or env GOREDIS_BENCH_PASSWORD)")
	fs.IntVar(&o.clients, "clients", 50, "concurrent connections")
	fs.IntVar(&o.requests, "requests", 100000, "total commands across all clients")
	fs.IntVar(&o.pipeline, "pipeline", 1, "commands per round trip")
	fs.IntVar(&o.keyspace, "keyspace", 10000, "number of distinct keys")
	fs.IntVar(&o.valueSize, "value-size", 64, "SET value size in bytes")
	fs.StringVar(&ratio, "ratio", "1:1", "GET:SET ratio, e.g. 1:0 (GET only), 0:1 (SET only), 9:1")
	fs.BoolVar(&o.prefill, "prefill", true, "SET every key once before measuring, so GETs hit")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Second, "per-round-trip timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var err error
	if o.gets, o.sets, err = parseRatio(ratio); err != nil {
		return err
	}
	if o.clients < 1 || o.requests < o.clients || o.pipeline < 1 || o.keyspace < 1 || o.valueSize < 0 {
		return errors.New("need clients >= 1, requests >= clients, pipeline >= 1, keyspace >= 1, value-size >= 0")
	}

	before, _ := serverInfo(o)
	if o.prefill && o.gets > 0 {
		if err := prefill(o); err != nil {
			return fmt.Errorf("prefill: %w", err)
		}
	}

	res, err := bench(o)
	if err != nil {
		return err
	}
	after, _ := serverInfo(o)
	report(out, o, ratio, res, before, after)
	if res.errors > 0 {
		return fmt.Errorf("%d commands returned errors", res.errors)
	}
	return nil
}

func parseRatio(s string) (int, int, error) {
	g, st, ok := strings.Cut(s, ":")
	gets, err1 := strconv.Atoi(g)
	sets, err2 := strconv.Atoi(st)
	if !ok || err1 != nil || err2 != nil || gets < 0 || sets < 0 || gets+sets == 0 {
		return 0, 0, fmt.Errorf("invalid -ratio %q (want GET:SET, e.g. 1:1)", s)
	}
	return gets, sets, nil
}

type conn struct {
	c net.Conn
	r *protocol.Reader
}

func dial(o options) (*conn, error) {
	c, err := net.DialTimeout("tcp", o.addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	cn := &conn{c: c, r: protocol.NewReader(c, protocol.Limits{
		MaxBulkBytes: 512 << 20, MaxArgs: 1 << 20, MaxInlineBytes: 1 << 20, MaxRequestBytes: 1 << 30,
	})}
	if o.password != "" {
		rep, err := cn.roundTrip(o.timeout, protocol.AppendCommand(nil, []byte("AUTH"), []byte(o.password)), 1)
		if err != nil {
			c.Close()
			return nil, err
		}
		if rep[0].IsError() {
			c.Close()
			return nil, fmt.Errorf("AUTH: %s", rep[0].Str)
		}
	}
	return cn, nil
}

func (cn *conn) roundTrip(timeout time.Duration, payload []byte, n int) ([]protocol.Reply, error) {
	_ = cn.c.SetDeadline(time.Now().Add(timeout))
	if _, err := cn.c.Write(payload); err != nil {
		return nil, err
	}
	out := make([]protocol.Reply, 0, n)
	for i := 0; i < n; i++ {
		rep, err := cn.r.ReadReply()
		if err != nil {
			return nil, err
		}
		out = append(out, rep)
	}
	return out, nil
}

func key(i int) []byte { return []byte("bench:key:" + strconv.Itoa(i)) }

func prefill(o options) error {
	cn, err := dial(o)
	if err != nil {
		return err
	}
	defer cn.c.Close()
	value := make([]byte, o.valueSize)
	const batch = 1000
	for start := 0; start < o.keyspace; start += batch {
		var buf []byte
		n := 0
		for i := start; i < min(start+batch, o.keyspace); i++ {
			buf = protocol.AppendCommand(buf, []byte("SET"), key(i), value)
			n++
		}
		if _, err := cn.roundTrip(o.timeout, buf, n); err != nil {
			return err
		}
	}
	return nil
}

type result struct {
	elapsed   time.Duration
	commands  int
	errors    int64
	latencies []time.Duration // per round trip
}

func bench(o options) (result, error) {
	conns := make([]*conn, o.clients)
	for i := range conns {
		cn, err := dial(o)
		if err != nil {
			for _, c := range conns[:i] {
				c.c.Close()
			}
			return result{}, fmt.Errorf("connect client %d: %w", i, err)
		}
		conns[i] = cn
	}
	defer func() {
		for _, c := range conns {
			c.c.Close()
		}
	}()

	perClient := o.requests / o.clients
	value := make([]byte, o.valueSize)
	for i := range value {
		value[i] = 'x'
	}
	var (
		wg       sync.WaitGroup
		errCount atomic.Int64
		mu       sync.Mutex
		all      []time.Duration
		firstErr error
	)
	startGate := make(chan struct{})
	for w, cn := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 42))
			lat := make([]time.Duration, 0, perClient/o.pipeline+1)
			var buf []byte
			<-startGate
			for sent := 0; sent < perClient; {
				n := min(o.pipeline, perClient-sent)
				buf = buf[:0]
				for i := 0; i < n; i++ {
					k := key(rng.IntN(o.keyspace))
					if rng.IntN(o.gets+o.sets) < o.gets {
						buf = protocol.AppendCommand(buf, []byte("GET"), k)
					} else {
						buf = protocol.AppendCommand(buf, []byte("SET"), k, value)
					}
				}
				t0 := time.Now()
				reps, err := cn.roundTrip(o.timeout, buf, n)
				lat = append(lat, time.Since(t0))
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("client %d: %w", w, err)
					}
					mu.Unlock()
					return
				}
				for _, r := range reps {
					if r.IsError() {
						errCount.Add(1)
					}
				}
				sent += n
			}
			mu.Lock()
			all = append(all, lat...)
			mu.Unlock()
		}()
	}
	start := time.Now()
	close(startGate)
	wg.Wait()
	elapsed := time.Since(start)
	if firstErr != nil {
		return result{}, firstErr
	}
	slices.Sort(all)
	return result{elapsed: elapsed, commands: perClient * o.clients, errors: errCount.Load(), latencies: all}, nil
}

// serverInfo fetches the fields we report from INFO.
func serverInfo(o options) (map[string]string, error) {
	cn, err := dial(o)
	if err != nil {
		return nil, err
	}
	defer cn.c.Close()
	reps, err := cn.roundTrip(o.timeout, protocol.AppendCommand(nil, []byte("INFO")), 1)
	if err != nil || reps[0].Kind != protocol.KindBulk {
		return nil, fmt.Errorf("INFO failed: %v", err)
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(reps[0].Bulk), "\r\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			m[k] = v
		}
	}
	return m, nil
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func report(w io.Writer, o options, ratio string, r result, before, after map[string]string) {
	unit := "per command"
	if o.pipeline > 1 {
		unit = fmt.Sprintf("per round trip of %d commands", o.pipeline)
	}
	host, _ := os.Hostname()
	fmt.Fprintf(w, "== goredis-bench ==\n")
	fmt.Fprintf(w, "target:        %s (server %s, %s)\n", o.addr, before["goredis_version"], before["go_version"])
	fmt.Fprintf(w, "client host:   %s, %s/%s, %d CPUs, %s\n", host, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	fmt.Fprintf(w, "workload:      %d clients, %d commands, pipeline %d, GET:SET %s, keyspace %d, value %d B\n",
		o.clients, r.commands, o.pipeline, ratio, o.keyspace, o.valueSize)
	fmt.Fprintf(w, "server fsync:  %s\n", orDash(before["aof_fsync"]))
	fmt.Fprintf(w, "elapsed:       %s\n", r.elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "throughput:    %.0f commands/s\n", float64(r.commands)/r.elapsed.Seconds())
	fmt.Fprintf(w, "errors:        %d\n", r.errors)
	fmt.Fprintf(w, "latency (%s):\n", unit)
	for _, p := range []struct {
		name string
		q    float64
	}{{"p50", .50}, {"p90", .90}, {"p99", .99}, {"p99.9", .999}, {"max", 1}} {
		fmt.Fprintf(w, "  %-6s %v\n", p.name, pct(r.latencies, p.q).Round(time.Microsecond))
	}
	fmt.Fprintf(w, "server memory: dataset %s -> %s, Go heap in use %s -> %s\n",
		orDash(before["used_memory_dataset_human"]), orDash(after["used_memory_dataset_human"]),
		orDash(before["go_heap_inuse_human"]), orDash(after["go_heap_inuse_human"]))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
