package persistence

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"goredis/internal/protocol"
)

// replayLimits are generous on purpose: the AOF is trusted local state, and
// refusing a record because the server's client limits were lowered since it
// was written would lose data.
var replayLimits = protocol.Limits{
	MaxBulkBytes:    512 << 20,
	MaxArgs:         1 << 20,
	MaxInlineBytes:  64 << 10,
	MaxRequestBytes: 1 << 30,
}

// Problem classifies what a scan found at the end of the valid data.
type Problem int

const (
	ProblemNone      Problem = iota
	ProblemTruncated         // file ends in the middle of a record
	ProblemCorrupt           // malformed RESP before the end of the file
	ProblemInvalid           // well-formed record that is not a valid command
)

func (p Problem) String() string {
	switch p {
	case ProblemTruncated:
		return "truncated"
	case ProblemCorrupt:
		return "corrupt"
	case ProblemInvalid:
		return "invalid command"
	}
	return "ok"
}

// ScanResult is the outcome of reading an AOF from start to finish.
type ScanResult struct {
	Records     int
	ValidOffset int64 // bytes from the start that hold complete, valid records
	Size        int64
	Problem     Problem
	Err         error // detail for Problem != ProblemNone
}

// Scan reads every record in path and passes it to apply. It stops at the
// first problem. A missing file is an empty, valid AOF.
func Scan(path string, apply func(args [][]byte) error) (ScanResult, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ScanResult{}, nil
	}
	if err != nil {
		return ScanResult{}, fmt.Errorf("open AOF: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ScanResult{}, fmt.Errorf("stat AOF: %w", err)
	}

	res := ScanResult{Size: fi.Size()}
	cr := &countingReader{r: f}
	pr := protocol.NewReader(bufio.NewReaderSize(cr, 256<<10), replayLimits)
	for {
		// Records must be RESP arrays; anything else would otherwise be
		// parsed as an inline command.
		if b, err := pr.PeekByte(); err == nil && b != '*' {
			res.Problem = ProblemCorrupt
			res.Err = fmt.Errorf("expected '*' at offset %d, found %q", res.ValidOffset, b)
			return res, nil
		}
		args, err := pr.ReadCommand()
		switch {
		case errors.Is(err, io.EOF):
			return res, nil
		case errors.Is(err, io.ErrUnexpectedEOF):
			res.Problem = ProblemTruncated
			res.Err = fmt.Errorf("incomplete record at offset %d (%d trailing bytes)", res.ValidOffset, res.Size-res.ValidOffset)
			return res, nil
		case protocol.IsProtocolError(err):
			res.Problem = ProblemCorrupt
			res.Err = fmt.Errorf("malformed record at offset %d: %w", res.ValidOffset, err)
			return res, nil
		case err != nil:
			return res, fmt.Errorf("read AOF: %w", err)
		}
		if args != nil {
			if err := apply(args); err != nil {
				res.Problem = ProblemInvalid
				res.Err = fmt.Errorf("invalid record at offset %d: %w", res.ValidOffset, err)
				return res, nil
			}
			res.Records++
		}
		res.ValidOffset = cr.n - int64(pr.Buffered())
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// load replays path at startup. An incomplete final record is truncated if
// allowTruncated is set; any other problem refuses startup, because silently
// skipping a corrupt record could resurrect deleted keys or lose writes.
func load(path string, apply func(args [][]byte) error, allowTruncated bool, log *slog.Logger) (LoadResult, error) {
	start := time.Now()
	res, err := Scan(path, apply)
	out := LoadResult{Commands: res.Records, Bytes: res.ValidOffset}
	if err != nil {
		return out, err
	}
	switch res.Problem {
	case ProblemNone:
		out.Bytes = res.Size
	case ProblemTruncated:
		if !allowTruncated {
			return out, fmt.Errorf("AOF %s: %w; set aof-load-truncated=true to discard it, or run goredis-check-aof -fix", path, res.Err)
		}
		if err := os.Truncate(path, res.ValidOffset); err != nil {
			return out, fmt.Errorf("truncate incomplete AOF record: %w", err)
		}
		out.TruncatedBytes = res.Size - res.ValidOffset
		log.Warn("AOF ended with an incomplete record; truncated it",
			"path", path, "offset", res.ValidOffset, "discarded_bytes", out.TruncatedBytes)
	default:
		return out, fmt.Errorf("AOF %s is %s: %w; refusing to start. Inspect it with goredis-check-aof (with -fix to truncate at the last valid record, after taking a backup)", path, res.Problem, res.Err)
	}
	out.Duration = time.Since(start)
	return out, nil
}
