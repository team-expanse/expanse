package tuikit

// ParseKeys turns one read's bytes into keys: named keys ("enter", "up", ...) or runs of
// printable text (a paste arrives as one run).
func ParseKeys(b []byte) []string {
	var keys []string
	text := ""
	flush := func() {
		if text != "" {
			keys = append(keys, text)
			text = ""
		}
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c >= 0x20 && c != 0x7f {
			text += string(c)
			continue
		}
		flush()
		switch c {
		case '\r', '\n':
			if c == '\r' && i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
			keys = append(keys, "enter")
		case 0x7f, 0x08:
			keys = append(keys, "backspace")
		case 0x03, 0x04:
			keys = append(keys, "ctrl-c")
		case '\t':
			keys = append(keys, "tab")
		case 0x1b:
			i = skipEscape(b, i, &keys)
		}
	}
	flush()
	return keys
}

// skipEscape consumes the escape sequence at b[i], recording up/down, and returns its last index.
func skipEscape(b []byte, i int, keys *[]string) int {
	if i+1 >= len(b) || b[i+1] != '[' {
		return i
	}
	j := i + 2
	for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
		j++
	}
	if j < len(b) {
		switch b[j] {
		case 'A':
			*keys = append(*keys, "up")
		case 'B':
			*keys = append(*keys, "down")
		}
	}
	return j
}
