package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// VDFNode is a parsed Valve KeyValues block. Values are string or VDFNode.
// Keys are stored lower-case, since Valve isn't consistent about casing.
type VDFNode map[string]any

// Get walks nested blocks and returns the string at the end of path.
func (n VDFNode) Get(path ...string) (string, bool) {
	cur := n
	for i, k := range path {
		v, ok := cur[strings.ToLower(k)]
		if !ok {
			return "", false
		}
		if i == len(path)-1 {
			s, ok := v.(string)
			return s, ok
		}
		if cur, ok = v.(VDFNode); !ok {
			return "", false
		}
	}
	return "", false
}

type vdfTokenKind int

const (
	vdfEOF vdfTokenKind = iota
	vdfString
	vdfOpen
	vdfClose
)

type vdfToken struct {
	kind vdfTokenKind
	text string
}

type vdfParser struct {
	s string
	i int
}

// ParseVDF parses a whole KeyValues document, such as an appmanifest .acf file.
func ParseVDF(s string) (VDFNode, error) {
	p := &vdfParser{s: s}
	return p.object(false)
}

// ExtractAppInfo finds the `"<appid>" { ... }` block in `app_info_print` output
// and parses only that block, ignoring SteamCMD's chatter around it.
func ExtractAppInfo(output string, appID int) (VDFNode, error) {
	key := `"` + strconv.Itoa(appID) + `"`
	for off := 0; ; {
		i := strings.Index(output[off:], key)
		if i < 0 {
			return nil, fmt.Errorf("app %d info not found in steamcmd output", appID)
		}
		start := off + i
		rest := strings.TrimLeft(output[start+len(key):], " \t\r\n")
		if strings.HasPrefix(rest, "{") {
			p := &vdfParser{s: output[start:]}
			p.next() // the key
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			node, _ := v.(VDFNode)
			return node, nil
		}
		off = start + len(key)
	}
}

func (p *vdfParser) object(nested bool) (VDFNode, error) {
	node := VDFNode{}
	for {
		t, err := p.next()
		if err != nil {
			return nil, err
		}
		switch t.kind {
		case vdfEOF:
			if nested {
				return nil, errors.New("vdf: unexpected end of input")
			}
			return node, nil
		case vdfClose:
			if !nested {
				return nil, fmt.Errorf("vdf: unexpected '}' at offset %d", p.i)
			}
			return node, nil
		case vdfOpen:
			return nil, fmt.Errorf("vdf: expected a key, got '{' at offset %d", p.i)
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		node[strings.ToLower(t.text)] = v
	}
}

func (p *vdfParser) value() (any, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	switch t.kind {
	case vdfString:
		return t.text, nil
	case vdfOpen:
		return p.object(true)
	default:
		return nil, fmt.Errorf("vdf: expected a value at offset %d", p.i)
	}
}

func (p *vdfParser) next() (vdfToken, error) {
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			p.i++
		case c == '/' && p.i+1 < len(p.s) && p.s[p.i+1] == '/':
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				p.i++
			}
		case c == '{':
			p.i++
			return vdfToken{kind: vdfOpen}, nil
		case c == '}':
			p.i++
			return vdfToken{kind: vdfClose}, nil
		case c == '"':
			return p.quoted()
		default:
			t := p.bare()
			if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
				continue // platform conditional such as [$WIN32]
			}
			return vdfToken{kind: vdfString, text: t}, nil
		}
	}
	return vdfToken{kind: vdfEOF}, nil
}

func (p *vdfParser) quoted() (vdfToken, error) {
	p.i++ // opening quote
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		p.i++
		switch c {
		case '"':
			return vdfToken{kind: vdfString, text: b.String()}, nil
		case '\\':
			// Only real escapes. Windows paths like game\bin keep their backslashes.
			if p.i < len(p.s) {
				switch e := p.s[p.i]; e {
				case '"', '\\':
					b.WriteByte(e)
					p.i++
					continue
				case 'n':
					b.WriteByte('\n')
					p.i++
					continue
				case 't':
					b.WriteByte('\t')
					p.i++
					continue
				}
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return vdfToken{}, errors.New("vdf: unterminated string")
}

func (p *vdfParser) bare() string {
	start := p.i
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\r', '\n', '{', '}', '"':
			return p.s[start:p.i]
		}
		p.i++
	}
	return p.s[start:]
}
