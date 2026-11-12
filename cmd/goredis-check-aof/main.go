// Command goredis-check-aof validates an append-only file and can repair a
// damaged one by truncating it at the last valid record.
//
//	goredis-check-aof appendonly.aof        # report only, exit 1 if damaged
//	goredis-check-aof -fix appendonly.aof   # back up, then truncate
//
// Validation replays every record into a throwaway in-memory store using the
// same command engine the server uses, so "valid" here means exactly "the
// server will load it". Run it only while the server is stopped.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"goredis/internal/command"
	"goredis/internal/persistence"
	"goredis/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("goredis-check-aof", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fix := fs.Bool("fix", false, "truncate the file at the last valid record (a backup is written first)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: goredis-check-aof [-fix] <file>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	path := fs.Arg(0)

	st := store.New(nil)
	st.SetLoading(true)
	eng := command.New(st, command.Options{})
	res, err := persistence.Scan(path, eng.Replay)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	fmt.Fprintf(stdout, "file:          %s\n", path)
	fmt.Fprintf(stdout, "size:          %d bytes\n", res.Size)
	fmt.Fprintf(stdout, "valid records: %d\n", res.Records)
	fmt.Fprintf(stdout, "valid bytes:   %d\n", res.ValidOffset)
	if res.Problem == persistence.ProblemNone {
		fmt.Fprintln(stdout, "status:        OK")
		return 0
	}
	fmt.Fprintf(stdout, "status:        %s\n", res.Problem)
	fmt.Fprintf(stdout, "detail:        %v\n", res.Err)
	fmt.Fprintf(stdout, "would discard: %d bytes after offset %d\n", res.Size-res.ValidOffset, res.ValidOffset)
	if !*fix {
		fmt.Fprintln(stdout, "run with -fix to truncate (a backup is written first)")
		return 1
	}

	backup := fmt.Sprintf("%s.bak-%s", path, time.Now().UTC().Format("20060102T150405Z"))
	if err := copyFile(path, backup); err != nil {
		fmt.Fprintln(stderr, "error: backup failed, file not modified:", err)
		return 2
	}
	if err := os.Truncate(path, res.ValidOffset); err != nil {
		fmt.Fprintln(stderr, "error: truncate:", err)
		return 2
	}
	fmt.Fprintf(stdout, "backup:        %s\n", backup)
	fmt.Fprintf(stdout, "fixed:         truncated to %d bytes\n", res.ValidOffset)
	return 0
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
