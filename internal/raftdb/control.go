package raftdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/robinmail/robinmail/internal/db"
)

// A command run on a node while its server runs cannot open the node: the
// server holds it. It reaches the server's node instead, over a control
// listener on loopback that cmd/robinmail runs with TLS and accepts only
// the node's own certificate, which is in the data directory: whoever can
// read that can open the database files anyway.
//
//	POST   /v1/db/execute           {"stmts": [...]}             -> reply
//	POST   /v1/db/query             {"level": "weak", "stmts": [...]} -> reply
//	GET    /v1/db/backup            the database file
//	POST   /v1/db/load              the database file            -> 204
//	GET    /v1/raft/status          Status
//	POST   /v1/raft/members         Member                       -> 204
//	DELETE /v1/raft/members/{id}    (id in hex)                  -> 204
//
// A reply is {"results": [...]} or {"error": {db.Error}}; a failure that
// is not about a statement is {"failure": "...", "unknown": bool, "stale":
// bool} with status 503.

// Admin is what the commands need of a node, open here or reached through
// its server.
type Admin interface {
	db.DB
	ClusterStatus(ctx context.Context) (Status, error)
	AddMember(ctx context.Context, m Member) error
	RemoveMember(ctx context.Context, id uint64) error
}

// ClusterStatus is Status for Admin.
func (d *DB) ClusterStatus(context.Context) (Status, error) { return d.Status(), nil }

var _ Admin = (*DB)(nil)

type controlRequest struct {
	Level db.Level       `json:"level,omitempty"`
	Stmts []db.Statement `json:"stmts"`
}

type controlReply struct {
	Results []db.Result `json:"results,omitempty"`
	Err     *db.Error   `json:"error,omitempty"`
	Failure string      `json:"failure,omitempty"`
	Unknown bool        `json:"unknown,omitempty"`
	Stale   bool        `json:"stale,omitempty"`
}

// ControlHandler serves the control paths for the commands of this node.
func (d *DB) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, res []db.Result, err error) {
		var r controlReply
		var e *db.Error
		switch {
		case err == nil:
			r.Results = res
		case errors.As(err, &e):
			r.Err = e
		default:
			r.Failure = err.Error()
			r.Unknown = errors.Is(err, db.ErrUnknownOutcome)
			r.Stale = errors.Is(err, db.ErrStale)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(r)
	}
	decode := func(w http.ResponseWriter, r *http.Request) (controlRequest, bool) {
		var req controlRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMessages)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return req, false
		}
		return req, true
	}
	mux.HandleFunc("POST /v1/db/execute", func(w http.ResponseWriter, r *http.Request) {
		if req, ok := decode(w, r); ok {
			res, err := d.Execute(r.Context(), req.Stmts...)
			reply(w, res, err)
		}
	})
	mux.HandleFunc("POST /v1/db/query", func(w http.ResponseWriter, r *http.Request) {
		if req, ok := decode(w, r); ok {
			res, err := d.Query(r.Context(), req.Level, req.Stmts...)
			reply(w, res, err)
		}
	})
	mux.HandleFunc("GET /v1/db/backup", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Barrier(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.sqlite3")
		d.writeCopy(r.Context(), w)
	})
	mux.HandleFunc("POST /v1/db/load", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.Load(r.Context(), bytes.NewReader(data)); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/raft/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(d.Status())
	})
	mux.HandleFunc("POST /v1/raft/members", func(w http.ResponseWriter, r *http.Request) {
		var m Member
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.AddMember(r.Context(), m); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/raft/members/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseUint(r.PathValue("id"), 16, 64)
		if err != nil {
			http.Error(w, "not a member id", http.StatusBadRequest)
			return
		}
		if err := d.RemoveMember(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// Remote is a node reached through its server's control listener. It
// implements Admin.
type Remote struct {
	// URL is the listener, "https://127.0.0.1:port".
	URL    string
	Client *http.Client
}

var _ Admin = (*Remote)(nil)

func (c *Remote) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the server of this node: %w", err)
	}
	return resp, nil
}

func (c *Remote) call(ctx context.Context, path string, req controlRequest) ([]db.Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return nil, answer(resp)
	}
	var r controlReply
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("the server of this node: %w", err)
	}
	switch {
	case r.Err != nil:
		return nil, r.Err
	case r.Unknown:
		return nil, fmt.Errorf("%w: %s", db.ErrUnknownOutcome, r.Failure)
	case r.Stale:
		return nil, db.ErrStale
	case r.Failure != "":
		return nil, errors.New(r.Failure)
	}
	for i := range r.Results {
		for _, row := range r.Results[i].Values {
			for j, v := range row {
				if n, ok := v.(json.Number); ok {
					row[j] = db.Number(n)
				}
			}
		}
	}
	return r.Results, nil
}

// answer turns an unexpected answer into an error.
func answer(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1000))
	return fmt.Errorf("the server of this node answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}

func (c *Remote) Execute(ctx context.Context, stmts ...db.Statement) ([]db.Result, error) {
	return c.call(ctx, "/v1/db/execute", controlRequest{Stmts: stmts})
}

func (c *Remote) ExecuteIdempotent(ctx context.Context, stmts ...db.Statement) ([]db.Result, error) {
	return c.Execute(ctx, stmts...)
}

func (c *Remote) Query(ctx context.Context, level db.Level, stmts ...db.Statement) ([]db.Result, error) {
	return c.call(ctx, "/v1/db/query", controlRequest{Level: level, Stmts: stmts})
}

func (c *Remote) Backup(ctx context.Context, w io.Writer) error {
	resp, err := c.do(ctx, http.MethodGet, "/v1/db/backup", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return answer(resp)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func (c *Remote) Load(ctx context.Context, r io.ReadSeeker) error {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/db/load", r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return answer(resp)
	}
	return nil
}

func (c *Remote) ClusterStatus(ctx context.Context) (Status, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/raft/status", nil)
	if err != nil {
		return Status{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Status{}, answer(resp)
	}
	var s Status
	err = json.NewDecoder(resp.Body).Decode(&s)
	return s, err
}

func (c *Remote) AddMember(ctx context.Context, m Member) error {
	body, _ := json.Marshal(m)
	resp, err := c.do(ctx, http.MethodPost, "/v1/raft/members", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return answer(resp)
	}
	return nil
}

func (c *Remote) RemoveMember(ctx context.Context, id uint64) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/raft/members/"+strconv.FormatUint(id, 16), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return answer(resp)
	}
	return nil
}
