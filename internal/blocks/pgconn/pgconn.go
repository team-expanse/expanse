// Package pgconn builds db/postgres replication connection strings that name a libpq passfile, so
// the replication password stays out of postgresql.auto.conf and postgres's log.
package pgconn

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RuntimeDir is the block units' RuntimeDirectory root: tmpfs, per replica, removed when it stops.
const RuntimeDir = "/run/expanse-block"

// PassfilePath is the replica's passfile, in its runtime directory so the password never reaches disk.
func PassfilePath(instance string) string {
	return filepath.Join(RuntimeDir, instance, "pgpass")
}

// PassfileLine is the pgpass entry for the replicator role, matching any host, port and database.
func PassfileLine(password string) string {
	esc := strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(password)
	return "*:*:*:replicator:" + esc + "\n"
}

// Replication is the primary_conninfo for streaming from host:port as the replicator role.
func Replication(host string, port int32, passfile string) string {
	return fmt.Sprintf("host=%s port=%d user=replicator passfile=%s application_name=expanse",
		Quote(host), port, Quote(passfile))
}

// Quote renders v as a libpq connection-string value.
func Quote(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\n'\\") {
		return v
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// Scrub drops conninfo's password and names passfile in place of any other; changed reports either.
func Scrub(conninfo, passfile string) (out string, changed bool) {
	var kept []string
	hasPassfile := false
	want := "passfile=" + Quote(passfile)
	for _, tok := range tokens(conninfo) {
		switch tok.key {
		case "password":
			changed = true
			continue
		case "passfile":
			hasPassfile = true
			if tok.raw != want {
				tok.raw, changed = want, true
			}
		}
		kept = append(kept, tok.raw)
	}
	if !hasPassfile {
		kept = append(kept, want)
		changed = true
	}
	if !changed {
		return conninfo, false
	}
	return strings.Join(kept, " "), true
}

// ScrubAutoConf applies Scrub to the primary_conninfo line of a postgresql.auto.conf.
func ScrubAutoConf(conf, passfile string) (string, bool) {
	lines := strings.SplitAfter(conf, "\n")
	changed := false
	for i, line := range lines {
		name, _, _ := strings.Cut(strings.TrimSpace(line), "=")
		first, last := strings.Index(line, "'"), strings.LastIndex(line, "'")
		if strings.TrimSpace(name) != "primary_conninfo" || first < 0 || last <= first {
			continue
		}
		scrubbed, ok := Scrub(unquoteConf(line[first+1:last]), passfile)
		if !ok {
			continue
		}
		lines[i] = "primary_conninfo = '" + quoteConf(scrubbed) + "'" + line[last+1:]
		changed = true
	}
	return strings.Join(lines, ""), changed
}

// unquoteConf undoes postgres's config-file quoting, which doubles ' and \.
func unquoteConf(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if (s[i] == '\'' || s[i] == '\\') && i+1 < len(s) && s[i+1] == s[i] {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func quoteConf(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s)
}

type token struct{ key, raw string }

// tokens splits a libpq key=value connection string, keeping each pair's original text.
func tokens(s string) []token {
	var out []token
	i := 0
	for {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return out
		}
		start := i
		for i < len(s) && s[i] != '=' && !isSpace(s[i]) {
			i++
		}
		key := s[start:i]
		for i < len(s) && (isSpace(s[i]) || s[i] == '=') {
			i++
		}
		if i < len(s) && s[i] == '\'' {
			for i++; i < len(s) && s[i] != '\''; i++ {
				if s[i] == '\\' {
					i++
				}
			}
			i++
		} else {
			for ; i < len(s) && !isSpace(s[i]); i++ {
				if s[i] == '\\' {
					i++
				}
			}
		}
		i = min(i, len(s))
		out = append(out, token{key: key, raw: s[start:i]})
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
