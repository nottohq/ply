package main

// Which files are not versioned: .gitignore and .plyignore, with git's rules.

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

type rule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

type ignorer struct {
	dir   string
	rules map[string][]rule // directory (slash, "" = top) -> its rules
}

func loadIgnore(dir string) *ignorer { return &ignorer{dir: dir, rules: map[string][]rule{}} }

func (ig *ignorer) rulesFor(d string) []rule {
	if rs, ok := ig.rules[d]; ok {
		return rs
	}
	var rs []rule
	for _, name := range []string{".gitignore", ".plyignore"} {
		b, err := os.ReadFile(filepath.Join(ig.dir, filepath.FromSlash(d), name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if r, ok := parseRule(line); ok {
				rs = append(rs, r)
			}
		}
	}
	ig.rules[d] = rs
	return rs
}

func parseRule(line string) (rule, bool) {
	line = strings.TrimRight(line, "\r")
	if !strings.HasSuffix(line, "\\ ") {
		line = strings.TrimRight(line, " ")
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return rule{}, false
	}
	var r rule
	if strings.HasPrefix(line, "!") {
		r.negate, line = true, line[1:]
	}
	if strings.HasPrefix(line, "\\") {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly, line = true, strings.TrimSuffix(line, "/")
	}
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return rule{}, false
	}
	var b strings.Builder
	if anchored {
		b.WriteString("^")
	} else {
		b.WriteString("^(?:.*/)?")
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case strings.HasPrefix(line[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(line[i:], "/**") && i+3 == len(line):
			b.WriteString("/.*")
			i += 2
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			if j := strings.IndexByte(line[i:], ']'); j > 1 {
				class := line[i : i+j+1]
				if strings.HasPrefix(class, "[!") {
					class = "[^" + class[2:]
				}
				b.WriteString(class)
				i += j
			} else {
				b.WriteString("\\[")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return rule{}, false
	}
	r.re = re
	return r, true
}

// skip reports whether a path (slash separated, relative to the workspace) is ignored.
func (ig *ignorer) skip(rel string, isDir bool) bool {
	base := path.Base(rel)
	if base == ".git" || rel == ".ply" || strings.HasPrefix(base, ".ply-tmp-") {
		return true
	}
	ignored := false
	// Rules in deeper directories are read later, so they override shallower ones.
	dirs := []string{""}
	parts := strings.Split(path.Dir(rel), "/")
	if path.Dir(rel) != "." {
		for i := range parts {
			dirs = append(dirs, strings.Join(parts[:i+1], "/"))
		}
	}
	for _, d := range dirs {
		sub := rel
		if d != "" {
			sub = strings.TrimPrefix(rel, d+"/")
		}
		for _, r := range ig.rulesFor(d) {
			if r.dirOnly && !isDir {
				continue
			}
			if r.re.MatchString(sub) {
				ignored = !r.negate
			}
		}
	}
	return ignored
}
