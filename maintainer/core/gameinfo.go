package core

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// EnsureGameInfoSearchPaths makes sure each path is a "Game" search path in
// gameinfo.gi. CS2 updates (and SteamCMD validate) replace that file, which
// silently drops lines like "Game csgo/addons/metamod" and with them every
// plugin. It returns the paths it had to add; the file is only rewritten
// when something was missing.
func EnsureGameInfoSearchPaths(file string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	info, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	out, added, err := AddGameInfoSearchPaths(string(data), paths)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if len(added) == 0 {
		return nil, nil
	}
	if err := writeFileAtomic(file, []byte(out)); err != nil {
		return nil, err
	}
	os.Chmod(file, info.Mode().Perm()) // the temp file was created 0600
	return added, nil
}

// AddGameInfoSearchPaths inserts a "Game <path>" line for every path missing
// from the SearchPaths block. New lines go right after Game_LowViolence, where
// Metamod's install guide puts them, so they load before the stock "Game csgo".
// Line endings and indentation follow the file.
func AddGameInfoSearchPaths(text string, paths []string) (string, []string, error) {
	nl := "\n"
	if strings.Contains(text, "\r\n") {
		nl = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	open, close, err := searchPathsBlock(lines)
	if err != nil {
		return "", nil, err
	}

	present := map[string]bool{}
	lowViolence, firstGame := -1, -1
	for i := open + 1; i < close; i++ {
		key, val := gameInfoEntry(lines[i])
		switch {
		case strings.EqualFold(key, "Game_LowViolence") && lowViolence < 0:
			lowViolence = i
		case strings.EqualFold(key, "Game"):
			present[normSearchPath(val)] = true
			if firstGame < 0 {
				firstGame = i
			}
		}
	}

	var missing []string
	for _, p := range paths {
		if n := normSearchPath(p); !present[n] {
			present[n] = true // also skips duplicates in paths
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return text, nil, nil
	}

	var at int
	var indent string
	switch {
	case lowViolence >= 0:
		at, indent = lowViolence+1, leadingSpace(lines[lowViolence])
	case firstGame >= 0:
		at, indent = firstGame, leadingSpace(lines[firstGame])
	default:
		at, indent = open+1, leadingSpace(lines[open])+"\t"
	}
	added := make([]string, len(missing))
	for i, p := range missing {
		added[i] = indent + "Game\t" + p
	}
	lines = slices.Insert(lines, at, added...)
	return strings.Join(lines, nl), missing, nil
}

// searchPathsBlock returns the line indexes of the braces around SearchPaths.
func searchPathsBlock(lines []string) (open, close int, err error) {
	open = -1
	for i, l := range lines {
		f := strings.Fields(stripComment(l))
		if len(f) == 0 || !strings.EqualFold(strings.Trim(f[0], `"`), "SearchPaths") {
			continue
		}
		if strings.Contains(stripComment(l), "{") {
			open = i
		} else {
			for j := i + 1; j < len(lines); j++ {
				if t := strings.TrimSpace(stripComment(lines[j])); t != "" {
					if strings.HasPrefix(t, "{") {
						open = j
					}
					break
				}
			}
		}
		break
	}
	if open < 0 {
		return 0, 0, errors.New("no SearchPaths block found")
	}
	depth := 0
	for i := open; i < len(lines); i++ {
		l := stripComment(lines[i])
		depth += strings.Count(l, "{") - strings.Count(l, "}")
		if depth == 0 {
			return open, i, nil
		}
	}
	return 0, 0, errors.New("SearchPaths block is not closed")
}

func gameInfoEntry(line string) (key, val string) {
	f := strings.Fields(stripComment(line))
	if len(f) < 2 {
		return "", ""
	}
	return strings.Trim(f[0], `"`), strings.Trim(f[1], `"`)
}

func stripComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

func normSearchPath(p string) string {
	p = strings.ReplaceAll(strings.Trim(strings.TrimSpace(p), `"`), `\`, "/")
	return strings.ToLower(strings.TrimRight(p, "/"))
}

func leadingSpace(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}
