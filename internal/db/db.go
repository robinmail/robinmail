// Package db is what the store needs from a database: a batch of
// statements run atomically, and reads at a chosen consistency.
//
// There are no interactive transactions. A write is one batch that SQLite
// runs as one transaction: a statement that fails rolls the whole batch
// back. internal/raftdb is the implementation, Raft and SQLite inside the
// process.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// DB runs batches and reads. Implementations are safe for concurrent use.
type DB interface {
	// Execute runs the statements as one transaction and returns one
	// result per statement. If a statement fails, nothing is applied and
	// the error is an *Error naming it. If the database cannot say whether
	// the batch was applied, the error wraps ErrUnknownOutcome.
	Execute(ctx context.Context, stmts ...Statement) ([]Result, error)
	// ExecuteIdempotent is Execute for a batch that is safe to run twice;
	// an implementation may send it again after an unknown outcome.
	ExecuteIdempotent(ctx context.Context, stmts ...Statement) ([]Result, error)
	// Query runs read-only statements at the given level, all against the
	// same snapshot.
	Query(ctx context.Context, level Level, stmts ...Statement) ([]Result, error)
	// Backup writes a copy of the whole database to w, as one SQLite file
	// of one moment.
	Backup(ctx context.Context, w io.Writer) error
	// Load replaces the database by the SQLite file in r.
	Load(ctx context.Context, r io.ReadSeeker) error
}

// Level is a read's consistency.
type Level string

const (
	// None reads the local copy, which may miss the newest writes. It
	// refuses with ErrStale when the node has been without a leader for
	// a few seconds.
	None Level = "none"
	// Weak is what most reads use. In internal/raftdb it is as strong as
	// Linearizable; the name stays from rqlite, where it was a read on
	// the leader without a quorum check.
	Weak Level = "weak"
	// Linearizable sees every write that was acknowledged before it began.
	Linearizable Level = "linearizable"
	// Strong is Linearizable; rqlite sent it through the log.
	Strong Level = "strong"
)

// Statement is one SQL statement with its parameters: positional (Args, for
// "?" and "?N") or named (Named, for ":name"), not both. A value is nil,
// an integer, a float, a string, a bool or []byte; see Normalize.
type Statement struct {
	SQL   string
	Args  []any
	Named map[string]any
}

// Stmt builds a statement with positional parameters.
func Stmt(sql string, args ...any) Statement { return Statement{SQL: sql, Args: args} }

// MarshalJSON encodes the statement as ["SQL", arg, ...] or
// ["SQL", {"name": arg, ...}], the form rqlite took and the Raft log keeps.
func (s Statement) MarshalJSON() ([]byte, error) {
	if len(s.Named) > 0 {
		if len(s.Args) > 0 {
			return nil, errors.New("db: statement has both positional and named parameters")
		}
		return json.Marshal([]any{s.SQL, s.Named})
	}
	return json.Marshal(append([]any{s.SQL}, s.Args...))
}

// UnmarshalJSON reads what MarshalJSON wrote. Numbers become int64 when
// they are integers and float64 otherwise, as Normalize makes them.
func (s *Statement) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("db: empty statement")
	}
	if err := json.Unmarshal(raw[0], &s.SQL); err != nil {
		return err
	}
	s.Args, s.Named = nil, nil
	if len(raw) == 2 && len(raw[1]) > 0 && raw[1][0] == '{' {
		var named map[string]json.RawMessage
		if err := json.Unmarshal(raw[1], &named); err != nil {
			return err
		}
		s.Named = make(map[string]any, len(named))
		for k, v := range named {
			x, err := decodeValue(v)
			if err != nil {
				return err
			}
			s.Named[k] = x
		}
		return nil
	}
	for _, v := range raw[1:] {
		x, err := decodeValue(v)
		if err != nil {
			return err
		}
		s.Args = append(s.Args, x)
	}
	return nil
}

func decodeValue(v json.RawMessage) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(v)))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return nil, err
	}
	if n, ok := x.(json.Number); ok {
		return Number(n), nil
	}
	if _, ok := x.(map[string]any); ok {
		return nil, errors.New("db: an object is not a parameter value")
	}
	if _, ok := x.([]any); ok {
		return nil, errors.New("db: an array is not a parameter value")
	}
	return x, nil
}

// Number keeps integers exact: encoding/json would turn them into float64.
func Number(n json.Number) any {
	if i, err := n.Int64(); err == nil {
		return i
	}
	f, _ := n.Float64()
	return f
}

// Normalize brings a statement's values to what a trip through JSON
// makes of them, which is what was stored while rqlite was the
// database: []byte becomes its base64 text, integers int64, other
// numbers float64. Every node applies the same values, whatever Go type
// the caller used.
func Normalize(stmts []Statement) ([]Statement, error) {
	data, err := json.Marshal(stmts)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	var out []Statement
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	return out, nil
}

// Result is the outcome of one statement. A value is nil, int64, float64
// or string.
type Result struct {
	Columns      []string `json:"columns,omitempty"`
	Values       [][]any  `json:"values,omitempty"`
	LastInsertID int64    `json:"last_insert_id,omitempty"`
	RowsAffected int64    `json:"rows_affected,omitempty"`
}

// SQLite extended result codes for constraint violations.
const (
	CodeConstraintCheck      = 275
	CodeConstraintPrimaryKey = 1555
	CodeConstraintUnique     = 2067
)

// Error is a statement that SQLite refused. The batch it was part of has
// been rolled back.
type Error struct {
	Index   int    `json:"index"`   // position of the statement in the batch
	Message string `json:"message"` // SQLite's message
	Code    int    `json:"code"`    // SQLite's extended result code, 0 if unknown
}

func (e *Error) Error() string { return fmt.Sprintf("db: statement %d: %s", e.Index, e.Message) }

// Unique reports whether the statement broke a UNIQUE or PRIMARY KEY
// constraint.
func (e *Error) Unique() bool {
	if e.Code != 0 {
		return e.Code == CodeConstraintUnique || e.Code == CodeConstraintPrimaryKey
	}
	return strings.HasPrefix(e.Message, "UNIQUE constraint failed")
}

// Check reports whether the statement broke a CHECK constraint.
func (e *Error) Check() bool {
	if e.Code != 0 {
		return e.Code == CodeConstraintCheck
	}
	return strings.HasPrefix(e.Message, "CHECK constraint failed")
}

// IsUnique reports whether err is a UNIQUE or PRIMARY KEY violation.
func IsUnique(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Unique()
}

// ErrUnknownOutcome wraps the error of a write that may or may not have
// been applied.
var ErrUnknownOutcome = errors.New("the outcome of the write is unknown")

// ErrStale is returned by a Query at level None when the node has been
// without a leader for too long to answer from its own copy.
var ErrStale = errors.New("db: this node's copy is stale")
