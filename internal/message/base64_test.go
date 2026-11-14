package message

import (
	"bytes"
	"encoding/base64"
	"io"
)

// newBase64Reader decodes base64 with line breaks, as a mail body has it.
func newBase64Reader(r io.Reader) io.Reader {
	data, _ := io.ReadAll(r)
	data = bytes.ReplaceAll(bytes.ReplaceAll(data, []byte("\r"), nil), []byte("\n"), nil)
	return base64.NewDecoder(base64.StdEncoding, bytes.NewReader(data))
}
