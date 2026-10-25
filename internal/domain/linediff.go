package domain

// LineEdit is one line of a shortest edit script between two line lists.
type LineEdit struct {
	// Mark is ' ' for a line in both lists, '-' for a base-only line and
	// '+' for a head-only line.
	Mark byte
	Text string
	// Base and Head are the 1-based line numbers at this position.
	Base, Head int
}

// LineEdits returns a shortest edit script between two line lists: their
// common prefix and suffix, and a longest common subsequence of the lines
// between. It returns false when the differing middle has more than
// maxCells line pairs.
func LineEdits(b, a []string, maxCells int) ([]LineEdit, bool) {
	prefix := 0
	for prefix < len(b) && prefix < len(a) && b[prefix] == a[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(b)-prefix && suffix < len(a)-prefix && b[len(b)-1-suffix] == a[len(a)-1-suffix] {
		suffix++
	}
	mb, ma := b[prefix:len(b)-suffix], a[prefix:len(a)-suffix]
	if len(mb)*len(ma) > maxCells {
		return nil, false
	}
	// lcs[i][j] is the LCS length of mb[i:] and ma[j:].
	width := len(ma) + 1
	lcs := make([]int32, (len(mb)+1)*width)
	for i := len(mb) - 1; i >= 0; i-- {
		for j := len(ma) - 1; j >= 0; j-- {
			if mb[i] == ma[j] {
				lcs[i*width+j] = lcs[(i+1)*width+j+1] + 1
			} else {
				lcs[i*width+j] = max(lcs[(i+1)*width+j], lcs[i*width+j+1])
			}
		}
	}
	ops := make([]LineEdit, 0, len(b)+len(a))
	bi, ai := 0, 0
	emit := func(mark byte, text string) {
		ops = append(ops, LineEdit{Mark: mark, Text: text, Base: bi + 1, Head: ai + 1})
		if mark != '+' {
			bi++
		}
		if mark != '-' {
			ai++
		}
	}
	for _, line := range b[:prefix] {
		emit(' ', line)
	}
	i, j := 0, 0
	for i < len(mb) || j < len(ma) {
		switch {
		case i < len(mb) && j < len(ma) && mb[i] == ma[j]:
			emit(' ', mb[i])
			i++
			j++
		case i < len(mb) && (j == len(ma) || lcs[(i+1)*width+j] >= lcs[i*width+j+1]):
			// Removed lines come before the lines that replace them.
			emit('-', mb[i])
			i++
		default:
			emit('+', ma[j])
			j++
		}
	}
	for _, line := range b[len(b)-suffix:] {
		emit(' ', line)
	}
	return ops, true
}
