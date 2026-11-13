package blob

import (
	"bufio"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// The encrypted file format, version 1.
//
//	header:  "RMB1"            4 bytes   magic and version
//	         chunk size        4 bytes   big endian; always 65536 for now
//	         nonce prefix     19 bytes   random, new for every file
//	chunks:  the plaintext cut into pieces of chunk size, each sealed with
//	         XChaCha20-Poly1305 and so 16 bytes longer than its plaintext
//
// Only the last chunk may be shorter than chunk size. There is always a last
// chunk: an empty plaintext is one chunk of 16 bytes, and a plaintext of
// exactly n chunks ends with a full one.
//
// The 24-byte nonce of chunk i is
//
//	nonce prefix (19) | i as big endian uint32 (4) | 1 for the last chunk, else 0 (1)
//
// and every chunk has the same associated data:
//
//	header (27) | account id | 0x00 | sha256 of the plaintext in hex
//
// What this gives (the STREAM construction of Hoang, Reyhanitabar, Rogaway
// and Vizár): a changed byte, a chunk moved to another position or taken from
// another file, and a file cut short or extended all fail authentication,
// because the position and the "last" mark are part of the nonce. A file
// renamed to another account or hash fails because of the associated data.
// A reader holds one chunk in memory, whatever the size of the blob.
//
// The file does not depend on which node wrote it or where it lies, so nodes
// can copy it between them as it is.
const (
	// KeySize is the size of an account key.
	KeySize = chacha20poly1305.KeySize

	magic          = "RMB1"
	chunkSize      = 64 << 10
	noncePrefixLen = chacha20poly1305.NonceSizeX - 5
	headerLen      = len(magic) + 4 + noncePrefixLen
	tagLen         = chacha20poly1305.Overhead
)

// ErrCorrupt is returned when a blob file does not decrypt: it was damaged,
// cut short or tampered with, belongs to another account or hash, or the
// key is wrong.
var ErrCorrupt = errors.New("blob: file is corrupt or not encrypted with this key")

// stream is what the encrypting and the decrypting reader share.
type stream struct {
	aead    cipher.AEAD
	ad      []byte
	nonce   [chacha20poly1305.NonceSizeX]byte
	counter uint32
	done    bool   // the last chunk has been produced
	out     []byte // produced but not yet read
	err     error
}

func newStream(key, header []byte, account, sha256hex string) (*stream, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("blob: key of %d bytes", len(key))
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	s := &stream{aead: aead}
	copy(s.nonce[:], header[len(magic)+4:])
	s.ad = append(append(append(append([]byte{}, header...), account...), 0), sha256hex...)
	return s, nil
}

// next returns the nonce for the next chunk and moves on.
func (s *stream) next(last bool) ([]byte, error) {
	if s.counter == 1<<32-1 {
		return nil, errors.New("blob: too many chunks")
	}
	binary.BigEndian.PutUint32(s.nonce[noncePrefixLen:], s.counter)
	s.nonce[len(s.nonce)-1] = 0
	if last {
		s.nonce[len(s.nonce)-1] = 1
	}
	s.counter++
	s.done = last
	return s.nonce[:], nil
}

// read hands out s.out, calling fill for more until the stream is done.
func (s *stream) read(p []byte, fill func() error) (int, error) {
	for len(s.out) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		if s.done {
			return 0, io.EOF
		}
		if s.err = fill(); s.err != nil {
			return 0, s.err
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

// readChunk reads up to size bytes and reports whether src ends there.
func readChunk(src *bufio.Reader, buf []byte) (n int, last bool, err error) {
	n, err = io.ReadFull(src, buf)
	switch err {
	case io.EOF, io.ErrUnexpectedEOF:
		return n, true, nil
	case nil:
		_, err = src.Peek(1)
		if err == io.EOF {
			return n, true, nil
		}
		return n, false, err
	}
	return n, false, err
}

type encrypter struct {
	stream
	src   *bufio.Reader
	plain []byte
	buf   []byte
}

// Encrypt returns a reader that yields the encrypted file for the plaintext
// read from src. account and sha256hex are the blob's name; sha256hex must be
// the hash of the plaintext, which the caller computed.
func Encrypt(key []byte, account, sha256hex string, src io.Reader) (io.Reader, error) {
	header := make([]byte, headerLen)
	copy(header, magic)
	binary.BigEndian.PutUint32(header[len(magic):], chunkSize)
	if _, err := rand.Read(header[len(magic)+4:]); err != nil {
		return nil, err
	}
	s, err := newStream(key, header, account, sha256hex)
	if err != nil {
		return nil, err
	}
	e := &encrypter{stream: *s, src: bufio.NewReaderSize(src, chunkSize),
		plain: make([]byte, chunkSize), buf: make([]byte, 0, chunkSize+tagLen)}
	e.out = header
	return e, nil
}

func (e *encrypter) Read(p []byte) (int, error) {
	return e.read(p, func() error {
		n, last, err := readChunk(e.src, e.plain)
		if err != nil {
			return err
		}
		nonce, err := e.next(last)
		if err != nil {
			return err
		}
		e.out = e.aead.Seal(e.buf[:0], nonce, e.plain[:n], e.ad)
		return nil
	})
}

type decrypter struct {
	stream
	src    *bufio.Reader
	closer io.Closer
	sealed []byte
	buf    []byte
}

// Decrypt returns a reader that yields the plaintext of an encrypted file.
// It closes src when closed. Every byte it returns has been authenticated,
// but only a read up to io.EOF proves the plaintext is complete: a file cut
// short ends in ErrCorrupt instead.
func Decrypt(key []byte, account, sha256hex string, src io.ReadCloser) (io.ReadCloser, error) {
	br := bufio.NewReaderSize(src, chunkSize+tagLen)
	header := make([]byte, headerLen)
	if _, err := io.ReadFull(br, header); err != nil {
		src.Close()
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, ErrCorrupt
		}
		return nil, err
	}
	if string(header[:len(magic)]) != magic || binary.BigEndian.Uint32(header[len(magic):]) != chunkSize {
		src.Close()
		return nil, ErrCorrupt
	}
	s, err := newStream(key, header, account, sha256hex)
	if err != nil {
		src.Close()
		return nil, err
	}
	return &decrypter{stream: *s, src: br, closer: src,
		sealed: make([]byte, chunkSize+tagLen), buf: make([]byte, 0, chunkSize)}, nil
}

func (d *decrypter) Read(p []byte) (int, error) {
	return d.read(p, func() error {
		n, last, err := readChunk(d.src, d.sealed)
		if err != nil {
			return err
		}
		if n < tagLen {
			return ErrCorrupt
		}
		nonce, err := d.next(last)
		if err != nil {
			return err
		}
		out, err := d.aead.Open(d.buf[:0], nonce, d.sealed[:n], d.ad)
		if err != nil {
			return ErrCorrupt
		}
		d.out = out
		return nil
	})
}

func (d *decrypter) Close() error { return d.closer.Close() }
