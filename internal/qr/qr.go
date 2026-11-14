// Package qr draws QR codes (ISO/IEC 18004), as far as robinmail needs
// them: bytes in, a square of dark and light modules out. It exists to
// show the otpauth URI of a TOTP secret to a phone's camera without
// handing the secret to somebody else's library or web service.
//
// The subset: byte mode, error correction level L, versions 1 to 10 (up
// to 271 bytes). An otpauth URI is some 130 bytes. Level L recovers 7% of
// the codewords, which is what a code read from a screen needs: there is
// no dirt on it.
package qr

import "errors"

// ErrTooLong is returned for data that does not fit in a version 10 code.
var ErrTooLong = errors.New("qr: more than 271 bytes")

// Code is a QR code: Size modules on each side.
type Code struct {
	Size    int
	modules [][]bool // [row][column], true is dark
}

// Dark reports whether the module in the given row and column is dark.
func (c *Code) Dark(row, col int) bool { return c.modules[row][col] }

// Rows returns the code as one string per row, "1" for a dark module and
// "0" for a light one. The quiet zone, four light modules on every side,
// is for whoever draws it.
func (c *Code) Rows() []string {
	out := make([]string, c.Size)
	for r, row := range c.modules {
		b := make([]byte, c.Size)
		for i, dark := range row {
			b[i] = '0'
			if dark {
				b[i] = '1'
			}
		}
		out[r] = string(b)
	}
	return out
}

// version describes one version at level L: the total number of codewords,
// the error correction codewords per block and the number of blocks (ISO
// 18004 table 9), and where the alignment patterns sit (annex E).
type version struct {
	total, ecPerBlock, blocks int
	align                     []int
}

var versions = [...]version{
	1:  {26, 7, 1, nil},
	2:  {44, 10, 1, []int{6, 18}},
	3:  {70, 15, 1, []int{6, 22}},
	4:  {100, 20, 1, []int{6, 26}},
	5:  {134, 26, 1, []int{6, 30}},
	6:  {172, 18, 2, []int{6, 34}},
	7:  {196, 20, 2, []int{6, 22, 38}},
	8:  {242, 24, 2, []int{6, 24, 42}},
	9:  {292, 30, 2, []int{6, 26, 46}},
	10: {346, 18, 4, []int{6, 28, 50}},
}

const maxVersion = len(versions) - 1

func (v version) dataCodewords() int { return v.total - v.ecPerBlock*v.blocks }

// countBits is the length of the character count in byte mode (table 3).
func countBits(ver int) int {
	if ver <= 9 {
		return 8
	}
	return 16
}

// Capacity is how many bytes a code of the given version holds.
func Capacity(ver int) int {
	return (versions[ver].dataCodewords()*8 - 4 - countBits(ver)) / 8
}

// Encode makes the smallest code that holds data.
func Encode(data []byte) (*Code, error) {
	ver := 1
	for ; ver <= maxVersion && len(data) > Capacity(ver); ver++ {
	}
	if ver > maxVersion {
		return nil, ErrTooLong
	}
	v := versions[ver]
	codewords := interleave(v, dataCodewords(ver, data))

	c := newMatrix(ver)
	c.placeData(codewords)
	// The mask with the lowest penalty (7.8.3): every one of the eight
	// reads back the same, some are easier on a camera.
	best, bestPenalty := 0, -1
	for mask := 0; mask < 8; mask++ {
		c.applyMask(mask)
		c.drawFormat(mask)
		if p := c.penalty(); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = mask, p
		}
		c.applyMask(mask) // XOR again: undone
	}
	c.applyMask(best)
	c.drawFormat(best)
	return &Code{Size: c.size, modules: c.dark}, nil
}

// dataCodewords builds the data codewords (7.4): the mode indicator for
// bytes (0100), the count, the bytes, a terminator of up to four zero
// bits, zero bits to the next codeword, then the pad codewords 0xEC and
// 0x11 in turn.
func dataCodewords(ver int, data []byte) []byte {
	n := versions[ver].dataCodewords()
	var w bitWriter
	w.write(0b0100, 4)
	w.write(uint32(len(data)), countBits(ver))
	for _, b := range data {
		w.write(uint32(b), 8)
	}
	if rest := n*8 - w.n; rest > 0 {
		w.write(0, min(4, rest))
	}
	if w.n%8 != 0 {
		w.write(0, 8-w.n%8)
	}
	for pad := byte(0xEC); len(w.b) < n; pad ^= 0xEC ^ 0x11 {
		w.b = append(w.b, pad)
	}
	return w.b
}

type bitWriter struct {
	b []byte
	n int // bits written
}

func (w *bitWriter) write(v uint32, bits int) {
	for i := bits - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.b[w.n/8] |= 0x80 >> uint(w.n%8)
		}
		w.n++
	}
}

// interleave splits the data codewords into the version's blocks, the
// shorter ones first, computes each block's error correction codewords,
// and writes out first the data and then the error correction codewords
// column by column across the blocks (7.6).
func interleave(v version, data []byte) []byte {
	short := len(data) / v.blocks
	longBlocks := len(data) % v.blocks
	blocks := make([][]byte, v.blocks)
	ec := make([][]byte, v.blocks)
	gen := generator(v.ecPerBlock)
	for i, off := 0, 0; i < v.blocks; i++ {
		n := short
		if i >= v.blocks-longBlocks {
			n++
		}
		blocks[i] = data[off : off+n]
		ec[i] = remainder(blocks[i], gen)
		off += n
	}
	out := make([]byte, 0, v.total)
	for col := 0; col <= short; col++ {
		for _, b := range blocks {
			if col < len(b) {
				out = append(out, b[col])
			}
		}
	}
	for col := 0; col < v.ecPerBlock; col++ {
		for _, e := range ec {
			out = append(out, e[col])
		}
	}
	return out
}

// GF(256) with the polynomial x^8 + x^4 + x^3 + x^2 + 1 (0x11D), which is
// the field of the code's Reed-Solomon error correction (7.5.2).
var gfExp, gfLog = func() (exp [510]byte, log [256]byte) {
	x := 1
	for i := 0; i < 255; i++ {
		exp[i], exp[i+255] = byte(x), byte(x)
		log[x] = byte(i)
		if x <<= 1; x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	return
}()

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

// generator is the generator polynomial of degree n: the product of
// (x - 2^i) for i from 0 to n-1 (annex A). The leading coefficient, which
// is 1, is left out; the rest is in order of falling powers.
func generator(n int) []byte {
	g := make([]byte, n)
	g[n-1] = 1
	root := byte(1)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			g[j] = gfMul(g[j], root)
			if j+1 < n {
				g[j] ^= g[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return g
}

// remainder is the remainder of data times x^n divided by the generator:
// the error correction codewords of a block.
func remainder(data, gen []byte) []byte {
	rem := make([]byte, len(gen))
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[len(rem)-1] = 0
		for i, g := range gen {
			rem[i] ^= gfMul(g, factor)
		}
	}
	return rem
}

// matrix is a code being drawn. function marks the modules that are not
// data: the patterns and the format and version information.
type matrix struct {
	ver, size int
	dark      [][]bool
	function  [][]bool
}

func newMatrix(ver int) *matrix {
	size := 17 + 4*ver
	m := &matrix{ver: ver, size: size, dark: make([][]bool, size), function: make([][]bool, size)}
	for i := range m.dark {
		m.dark[i], m.function[i] = make([]bool, size), make([]bool, size)
	}
	// The timing patterns: row 6 and column 6, alternating (6.3.5).
	for i := 0; i < size; i++ {
		m.set(6, i, i%2 == 0)
		m.set(i, 6, i%2 == 0)
	}
	// The finder patterns in three corners, each with its light separator
	// (6.3.3, 6.3.4).
	m.finder(3, 3)
	m.finder(3, size-4)
	m.finder(size-4, 3)
	// The alignment patterns, at every pair of the version's positions
	// that is not on a finder pattern (6.3.6).
	a := versions[ver].align
	for i, r := range a {
		for j, c := range a {
			if (i == 0 && j == 0) || (i == 0 && j == len(a)-1) || (i == len(a)-1 && j == 0) {
				continue
			}
			for dr := -2; dr <= 2; dr++ {
				for dc := -2; dc <= 2; dc++ {
					m.set(r+dr, c+dc, max(abs(dr), abs(dc)) != 1)
				}
			}
		}
	}
	// Room for the format information, drawn once the mask is chosen, and
	// the version information of versions 7 and up.
	m.drawFormat(0)
	m.drawVersion()
	return m
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// set draws a function module.
func (m *matrix) set(row, col int, dark bool) {
	m.dark[row][col] = dark
	m.function[row][col] = true
}

// finder draws a finder pattern around its centre, with the separator.
func (m *matrix) finder(row, col int) {
	for dr := -4; dr <= 4; dr++ {
		for dc := -4; dc <= 4; dc++ {
			r, c := row+dr, col+dc
			if r < 0 || r >= m.size || c < 0 || c >= m.size {
				continue
			}
			d := max(abs(dr), abs(dc))
			m.set(r, c, d != 2 && d != 4)
		}
	}
}

// drawFormat writes the format information (7.9): two bits for the level
// (L is 01) and three for the mask, protected by a BCH (15,5) code with
// the generator 10100110111 and XORed with 101010000010010, twice: around
// the top left finder pattern, and split over the other two.
func (m *matrix) drawFormat(mask int) {
	data := uint32(0b01<<3 | mask)
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>uint(i)&1 == 1 }

	for i := 0; i <= 5; i++ {
		m.set(i, 8, bit(i))
	}
	m.set(7, 8, bit(6))
	m.set(8, 8, bit(7))
	m.set(8, 7, bit(8))
	for i := 9; i < 15; i++ {
		m.set(8, 14-i, bit(i))
	}
	for i := 0; i < 8; i++ {
		m.set(8, m.size-1-i, bit(i))
	}
	for i := 8; i < 15; i++ {
		m.set(m.size-15+i, 8, bit(i))
	}
	m.set(m.size-8, 8, true) // the module that is always dark
}

// drawVersion writes the version information of versions 7 and up (7.10):
// six bits of version, protected by a BCH (18,6) code with the generator
// 1111100100101, in two blocks of 3 by 6 modules.
func (m *matrix) drawVersion() {
	if m.ver < 7 {
		return
	}
	rem := uint32(m.ver)
	for i := 0; i < 12; i++ {
		rem = rem<<1 ^ (rem>>11)*0x1F25
	}
	bits := uint32(m.ver)<<12 | rem
	for i := 0; i < 18; i++ {
		dark := bits>>uint(i)&1 == 1
		a, b := m.size-11+i%3, i/3
		m.set(b, a, dark)
		m.set(a, b, dark)
	}
}

// placeData writes the codewords into the modules that are free (7.7.3):
// in columns two modules wide, from the right edge to the left, going up
// and down in turn, the most significant bit first. Column 6 holds a
// timing pattern and is skipped. Modules left over stay light.
func (m *matrix) placeData(data []byte) {
	i := 0
	for right := m.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < m.size; vert++ {
			for j := 0; j < 2; j++ {
				col := right - j
				row := vert
				if (right+1)&2 == 0 {
					row = m.size - 1 - vert
				}
				if m.function[row][col] || i >= len(data)*8 {
					continue
				}
				m.dark[row][col] = data[i/8]>>uint(7-i%8)&1 == 1
				i++
			}
		}
	}
}

// applyMask flips the data modules the mask pattern selects (7.8.2,
// table 10). Applying it twice gives back what was there.
func (m *matrix) applyMask(mask int) {
	for r := 0; r < m.size; r++ {
		for c := 0; c < m.size; c++ {
			if m.function[r][c] {
				continue
			}
			var flip bool
			switch mask {
			case 0:
				flip = (r+c)%2 == 0
			case 1:
				flip = r%2 == 0
			case 2:
				flip = c%3 == 0
			case 3:
				flip = (r+c)%3 == 0
			case 4:
				flip = (r/2+c/3)%2 == 0
			case 5:
				flip = r*c%2+r*c%3 == 0
			case 6:
				flip = (r*c%2+r*c%3)%2 == 0
			case 7:
				flip = ((r+c)%2+r*c%3)%2 == 0
			}
			if flip {
				m.dark[r][c] = !m.dark[r][c]
			}
		}
	}
}

// penalty scores a masked code (7.8.3.1, table 11): runs of five or more
// modules of one colour, blocks of 2 by 2, the 1:1:3:1:1 pattern of a
// finder with four light modules beside it, and how far the share of dark
// modules is from half.
func (m *matrix) penalty() int {
	p := 0
	at := func(i, j int, byRow bool) bool {
		if byRow {
			return m.dark[i][j]
		}
		return m.dark[j][i]
	}
	finderLike := [2][11]bool{
		{true, false, true, true, true, false, true, false, false, false, false},
		{false, false, false, false, true, false, true, true, true, false, true},
	}
	for _, byRow := range []bool{true, false} {
		for i := 0; i < m.size; i++ {
			run := 1
			for j := 1; j < m.size; j++ {
				if at(i, j, byRow) == at(i, j-1, byRow) {
					run++
					if run == 5 {
						p += 3
					} else if run > 5 {
						p++
					}
				} else {
					run = 1
				}
			}
			for j := 0; j+11 <= m.size; j++ {
				for _, pat := range finderLike {
					match := true
					for k := 0; k < 11 && match; k++ {
						match = at(i, j+k, byRow) == pat[k]
					}
					if match {
						p += 40
					}
				}
			}
		}
	}
	dark := 0
	for r := 0; r < m.size; r++ {
		for c := 0; c < m.size; c++ {
			if m.dark[r][c] {
				dark++
			}
			if r+1 < m.size && c+1 < m.size && m.dark[r][c] == m.dark[r][c+1] &&
				m.dark[r][c] == m.dark[r+1][c] && m.dark[r][c] == m.dark[r+1][c+1] {
				p += 3
			}
		}
	}
	// Ten points for every 5% away from 50%.
	p += abs(dark*20-m.size*m.size*10) / (m.size * m.size) * 10
	return p
}
