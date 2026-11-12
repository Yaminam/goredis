// Package benchmarks measures goredis end to end over loopback TCP, through
// the real server stack (protocol, engine, store, AOF). Run with:
//
//	go test -run=^$ -bench=. -benchmem ./benchmarks
//
// Results depend heavily on hardware, OS and fsync policy; record them with
// the conditions (see docs/benchmarks.md). Do not compare against Redis
// unless both run on the same machine with equivalent settings.
package benchmarks

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"goredis/internal/app"
	"goredis/internal/config"
	"goredis/internal/persistence"
	"goredis/internal/protocol"
)

func startServer(b *testing.B, aof bool, fsync persistence.FsyncPolicy) string {
	b.Helper()
	cfg := config.Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.AdminAddr = ""
	cfg.AOFEnabled = aof
	cfg.AOFFsync = fsync
	cfg.AOFPath = filepath.Join(b.TempDir(), "appendonly.aof")
	cfg.MaxClients = 10000
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "bench")
	if err != nil {
		b.Fatal(err)
	}
	a.Start()
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	})
	return a.Addr()
}

type client struct {
	c net.Conn
	r *protocol.Reader
}

func dial(b *testing.B, addr string) *client {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { c.Close() })
	return &client{c: c, r: protocol.NewReader(c, protocol.DefaultLimits())}
}

func (cl *client) roundTrip(payload []byte, n int) error {
	if _, err := cl.c.Write(payload); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		rep, err := cl.r.ReadReply()
		if err != nil {
			return err
		}
		if rep.IsError() {
			return fmt.Errorf("server error: %s", rep.Str)
		}
	}
	return nil
}

var value = make([]byte, 64)

func setCmd(i int) []byte {
	return protocol.AppendCommand(nil, []byte("SET"), []byte("key:"+strconv.Itoa(i%10000)), value)
}

func getCmd(i int) []byte {
	return protocol.AppendCommand(nil, []byte("GET"), []byte("key:"+strconv.Itoa(i%10000)))
}

func prefill(b *testing.B, addr string) {
	cl := dial(b, addr)
	var buf []byte
	for i := 0; i < 10000; i++ {
		buf = append(buf, setCmd(i)...)
	}
	if err := cl.roundTrip(buf, 10000); err != nil {
		b.Fatal(err)
	}
}

// One client, one command per round trip: dominated by loopback latency.
func BenchmarkGET(b *testing.B) {
	addr := startServer(b, false, 0)
	prefill(b, addr)
	cl := dial(b, addr)
	i := 0
	for b.Loop() {
		if err := cl.roundTrip(getCmd(i), 1); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

func BenchmarkSET(b *testing.B) {
	for _, tc := range []struct {
		name  string
		aof   bool
		fsync persistence.FsyncPolicy
	}{
		{"aof=off", false, 0},
		{"aof=no", true, persistence.FsyncNo},
		{"aof=everysec", true, persistence.FsyncEverySec},
		{"aof=always", true, persistence.FsyncAlways},
	} {
		b.Run(tc.name, func(b *testing.B) {
			cl := dial(b, startServer(b, tc.aof, tc.fsync))
			i := 0
			for b.Loop() {
				if err := cl.roundTrip(setCmd(i), 1); err != nil {
					b.Fatal(err)
				}
				i++
			}
		})
	}
}

// Many concurrent clients, 80% GET / 20% SET, AOF everysec.
func BenchmarkMixedParallel(b *testing.B) {
	addr := startServer(b, true, persistence.FsyncEverySec)
	prefill(b, addr)
	var id atomic.Int64
	b.SetParallelism(8) // 8 x GOMAXPROCS client goroutines
	b.RunParallel(func(pb *testing.PB) {
		cl := dial(b, addr)
		i := int(id.Add(1)) * 7919
		for pb.Next() {
			cmd := getCmd(i)
			if i%5 == 0 {
				cmd = setCmd(i)
			}
			if err := cl.roundTrip(cmd, 1); err != nil {
				b.Error(err)
				return
			}
			i++
		}
	})
}

// One client sending 100 GETs per round trip: shows the cost of the server
// itself once network round trips are amortised.
func BenchmarkPipelinedGET100(b *testing.B) {
	addr := startServer(b, false, 0)
	prefill(b, addr)
	cl := dial(b, addr)
	var batch []byte
	for i := 0; i < 100; i++ {
		batch = append(batch, getCmd(i*97)...)
	}
	for b.Loop() {
		if err := cl.roundTrip(batch, 100); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*100)/b.Elapsed().Seconds(), "cmds/s")
}
