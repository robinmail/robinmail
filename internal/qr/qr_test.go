package qr

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ISO/IEC 18004 table 9 at level L: how many data codewords a version
// has, and with them how many bytes fit in byte mode (table 7: 17, 32,
// 53, 78, 106, 134, 154, 192, 230, 271).
func TestCapacity(t *testing.T) {
	for ver, want := range []int{0, 17, 32, 53, 78, 106, 134, 154, 192, 230, 271} {
		if ver == 0 {
			continue
		}
		if got := Capacity(ver); got != want {
			t.Errorf("version %d holds %d bytes, want %d", ver, got, want)
		}
	}
}

// Annex A's generator polynomials, written as exponents of the field's
// generating element: for 7 error correction codewords
// x^7 + a^87 x^6 + a^229 x^5 + a^146 x^4 + a^149 x^3 + a^238 x^2 + a^102 x + a^21,
// for 10: 251, 67, 46, 61, 118, 70, 64, 94, 32, 45.
func TestGeneratorPolynomials(t *testing.T) {
	for n, exps := range map[int][]int{
		7:  {87, 229, 146, 149, 238, 102, 21},
		10: {251, 67, 46, 61, 118, 70, 64, 94, 32, 45},
	} {
		g := generator(n)
		for i, e := range exps {
			if g[i] != gfExp[e] {
				t.Errorf("generator(%d): coefficient %d is a^%d, want a^%d", n, i, gfLog[g[i]], e)
			}
		}
	}
}

// The worked example of ISO/IEC 18004 annex I (2006) and of every QR
// tutorial since: "01234567" in a version 1 code at level M has the data
// codewords 10 20 0C 56 61 80 EC 11 EC 11 EC 11 EC 11 EC 11 and the error
// correction codewords A5 24 D4 C1 ED 36 C7 87 2C 55. That example is in
// numeric mode, which this package does not write, so the test takes its
// data codewords as given and checks the Reed-Solomon half.
func TestReedSolomonAnnexI(t *testing.T) {
	data, _ := hex.DecodeString("10200C5661" + "80EC11EC11EC11EC11EC11")
	want, _ := hex.DecodeString("A524D4C1ED36C7872C55")
	if got := remainder(data, generator(10)); !bytes.Equal(got, want) {
		t.Errorf("error correction codewords %X, want %X", got, want)
	}
}

// 7.4: mode 0100, an 8-bit count, the bytes, the terminator, then the pad
// codewords EC and 11 in turn.
func TestDataCodewords(t *testing.T) {
	got := dataCodewords(1, []byte("hi"))
	want, _ := hex.DecodeString("402686" + "90" + "EC11EC11EC11EC11EC11EC11EC11EC")
	if !bytes.Equal(got, want) {
		t.Errorf("codewords %X, want %X", got, want)
	}
	// Full to the last bit: no room for a terminator, and none is needed.
	if got := dataCodewords(1, bytes.Repeat([]byte{'x'}, 17)); len(got) != 19 || got[18] != 0x80 {
		t.Errorf("a full version 1: %X", got)
	}
	// From version 10 on the count has 16 bits.
	if got := dataCodewords(10, []byte("A")); got[0] != 0x40 || got[1] != 0x00 || got[2] != 0x14 || got[3] != 0x10 {
		t.Errorf("version 10: % X", got[:4])
	}
}

// The fixed patterns: three finder patterns, the timing patterns, the
// module that is always dark, and the size of each version (17 + 4v).
func TestFunctionPatterns(t *testing.T) {
	finder := []string{"1111111", "1000001", "1011101", "1011101", "1011101", "1000001", "1111111"}
	for _, n := range []int{1, 17, 18, 60, 107, 135, 155, 200, 271} {
		c, err := Encode(bytes.Repeat([]byte{'a'}, n))
		if err != nil {
			t.Fatal(err)
		}
		rows := c.Rows()
		if len(rows) != c.Size || (c.Size-17)%4 != 0 {
			t.Fatalf("%d bytes: size %d", n, c.Size)
		}
		for i, f := range finder {
			if rows[i][:8] != f+"0" || rows[i][c.Size-8:] != "0"+f || rows[c.Size-7+i][:8] != f+"0" {
				t.Errorf("%d bytes: finder patterns, row %d", n, i)
			}
		}
		if rows[7][:8] != "00000000" || rows[c.Size-8][:8] != "00000000" || rows[7][c.Size-8:] != "00000000" {
			t.Errorf("%d bytes: separators", n)
		}
		for i := 8; i < c.Size-8; i++ {
			if c.Dark(6, i) != (i%2 == 0) || c.Dark(i, 6) != (i%2 == 0) {
				t.Errorf("%d bytes: timing pattern at %d", n, i)
			}
		}
		if !c.Dark(c.Size-8, 8) {
			t.Errorf("%d bytes: the dark module", n)
		}
	}
	if _, err := Encode(make([]byte, 272)); err != ErrTooLong {
		t.Errorf("272 bytes: %v", err)
	}
	if c, err := Encode(nil); err != nil || c.Size != 21 {
		t.Errorf("no bytes: %v %v", c, err)
	}
}

// 7.9: the format information is the same in both places it is written,
// and decodes, after the XOR with 101010000010010, to level L (01) and a
// mask, with a remainder of zero under the generator 10100110111.
func TestFormatInformation(t *testing.T) {
	c, _ := Encode([]byte("otpauth://totp/x"))
	n := c.Size
	var first, second uint32
	for i := 0; i <= 5; i++ {
		first |= bit(c.Dark(i, 8)) << uint(i)
	}
	first |= bit(c.Dark(7, 8))<<6 | bit(c.Dark(8, 8))<<7 | bit(c.Dark(8, 7))<<8
	for i := 9; i < 15; i++ {
		first |= bit(c.Dark(8, 14-i)) << uint(i)
	}
	for i := 0; i < 8; i++ {
		second |= bit(c.Dark(8, n-1-i)) << uint(i)
	}
	for i := 8; i < 15; i++ {
		second |= bit(c.Dark(n-15+i, 8)) << uint(i)
	}
	if first != second {
		t.Fatalf("the two copies differ: %015b %015b", first, second)
	}
	word := first ^ 0x5412
	if word>>13 != 0b01 {
		t.Errorf("level bits %02b, want 01 (L)", word>>13)
	}
	rem := word
	for i := 14; i >= 10; i-- {
		if rem>>uint(i)&1 == 1 {
			rem ^= 0x537 << uint(i-10)
		}
	}
	if rem != 0 {
		t.Errorf("format word %015b is not a BCH codeword", word)
	}
}

func bit(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// The codes are read back by a decoder that shares nothing with this
// package: jsQR, a development dependency of the webmail. Skipped where
// the webmail's packages are not installed ("cd web && npm ci").
func TestAgainstDecoder(t *testing.T) {
	web, _ := filepath.Abs("../../web")
	if _, err := os.Stat(filepath.Join(web, "node_modules", "jsqr")); err != nil {
		t.Skip("web/node_modules/jsqr is not installed")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	inputs := [][]byte{
		[]byte("a"),
		[]byte("otpauth://totp/mail.example.org:anna@example.org?algorithm=SHA1&digits=6&issuer=mail.example.org&period=30&secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"),
		{0, 1, 2, 0xfe, 0xff},
	}
	// Every version, full to the last byte and with one byte to spare.
	for ver := 1; ver <= maxVersion; ver++ {
		for _, n := range []int{Capacity(ver), Capacity(ver) - 1} {
			b := make([]byte, n)
			for i := range b {
				b[i] = byte('!' + (i*7+ver)%90)
			}
			inputs = append(inputs, b)
		}
	}
	var codes [][]string
	for _, in := range inputs {
		c, err := Encode(in)
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, c.Rows())
	}
	payload, _ := json.Marshal(codes)
	script, _ := filepath.Abs("testdata_decode.mjs")
	cmd := exec.Command(node, script)
	cmd.Dir = web
	cmd.Stdin = bytes.NewReader(payload)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var got []*string
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(inputs) {
		t.Fatalf("decoder output: %v %s", err, out)
	}
	for i, in := range inputs {
		switch {
		case got[i] == nil:
			t.Errorf("code %d (%d bytes, size %d) was not read", i, len(in), len(codes[i]))
		case *got[i] != hex.EncodeToString(in):
			t.Errorf("code %d (%d bytes) read as %s", i, len(in), *got[i])
		}
	}
}
