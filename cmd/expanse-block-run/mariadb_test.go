package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMariaDBSetupDefaults(t *testing.T) {
	s, err := mariadbSetupFrom("/var/lib/db", map[string]any{"rootPassword": "rootsecret"})
	if err != nil {
		t.Fatal(err)
	}
	want := mariadbSetup{
		DataDir: "/var/lib/db/mariadb", Port: "13306", RootPassword: "rootsecret",
		BufferPool: "128M", MaxConnections: 151,
	}
	if s != want {
		t.Fatalf("setup = %+v, want %+v", s, want)
	}
}

func TestMariaDBSetupFromConfig(t *testing.T) {
	s, err := mariadbSetupFrom("/m", map[string]any{
		"rootPassword": "rootsecret", "port": 3307.0, "database": "app", "user": "app",
		"password": "apppass", "bufferPool": "1G", "maxConnections": 500.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != "3307" || s.Database != "app" || s.User != "app" || s.Password != "apppass" ||
		s.BufferPool != "1G" || s.MaxConnections != 500 {
		t.Fatalf("setup = %+v", s)
	}
}

func TestMariaDBSetupRejects(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"no root password":        {},
		"user without password":   {"rootPassword": "rootsecret", "user": "app", "database": "app"},
		"user without database":   {"rootPassword": "rootsecret", "user": "app", "password": "apppass"},
		"password without user":   {"rootPassword": "rootsecret", "password": "apppass"},
		"database name injection": {"rootPassword": "rootsecret", "database": "a`; DROP"},
		"user name injection":     {"rootPassword": "rootsecret", "user": "a'@'%", "password": "p", "database": "app"},
	} {
		if _, err := mariadbSetupFrom("/m", cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSQLString(t *testing.T) {
	for in, want := range map[string]string{
		"plain":    `'plain'`,
		`it's`:     `'it\'s'`,
		`back\sl`:  `'back\\sl'`,
		"nul\x00x": `'nul\0x'`,
		"nl\nx":    `'nl\nx'`,
	} {
		if got := sqlString(in); got != want {
			t.Errorf("sqlString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestMariaDBConfigIsDurable(t *testing.T) {
	s := mariadbSetup{DataDir: "/m/mariadb", Port: "13306", BufferPool: "256M", MaxConnections: 80}
	conf := mariadbConfig(s, "/tmp/w")
	for _, line := range []string{
		"[mariadbd]", "datadir=/m/mariadb", "port=13306", "bind-address=0.0.0.0",
		"socket=/tmp/w/mariadbd.sock", "init-file=/tmp/w/init.sql", "tmpdir=/tmp/w/tmp",
		"skip-name-resolve", "innodb_buffer_pool_size=256M", "max_connections=80",
		"innodb_flush_log_at_trx_commit=1", "innodb_doublewrite=1", "character-set-server=utf8mb4",
	} {
		if !strings.Contains(conf, line+"\n") {
			t.Errorf("config lacks %q:\n%s", line, conf)
		}
	}
}

func TestMariaDBInitSQL(t *testing.T) {
	sql := mariadbInitSQL(mariadbSetup{RootPassword: "r'oot", Database: "app", User: "web", Password: "pw"})
	// mariadb-install-db leaves passwordless root@127.0.0.1, root@::1 and root@<hostname>.
	if !strings.HasPrefix(sql, "DELETE FROM mysql.global_priv WHERE User IN ('root', '') AND Host NOT IN ('localhost', '%');\n"+
		"FLUSH PRIVILEGES;\n") {
		t.Errorf("init SQL does not first drop the installer's extra root and anonymous accounts:\n%s", sql)
	}
	for _, stmt := range []string{
		`CREATE USER IF NOT EXISTS 'root'@'localhost';`,
		`ALTER USER 'root'@'localhost' IDENTIFIED BY 'r\'oot';`,
		`CREATE USER IF NOT EXISTS 'root'@'%';`,
		`ALTER USER 'root'@'%' IDENTIFIED BY 'r\'oot';`,
		"GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' WITH GRANT OPTION;",
		"CREATE DATABASE IF NOT EXISTS `app`;",
		`CREATE USER IF NOT EXISTS 'web'@'%';`,
		`ALTER USER 'web'@'%' IDENTIFIED BY 'pw';`,
		"GRANT ALL PRIVILEGES ON `app`.* TO 'web'@'%';",
	} {
		if !strings.Contains(sql, stmt+"\n") {
			t.Errorf("init SQL lacks %q:\n%s", stmt, sql)
		}
	}
	// MariaDB reads an init file one statement per line.
	for _, line := range strings.Split(strings.TrimSpace(sql), "\n") {
		if !strings.HasSuffix(line, ";") {
			t.Errorf("line %q is not one whole statement", line)
		}
	}
}

func TestMariaDBInitSQLWithoutAppUser(t *testing.T) {
	sql := mariadbInitSQL(mariadbSetup{RootPassword: "rootsecret", Database: "app"})
	if !strings.Contains(sql, "CREATE DATABASE IF NOT EXISTS `app`;") {
		t.Errorf("database missing:\n%s", sql)
	}
	if strings.Count(sql, "CREATE USER") != 2 {
		t.Errorf("want only the two root accounts:\n%s", sql)
	}
}

func TestEnsureMariaDBDataDirInstallsOnce(t *testing.T) {
	data := filepath.Join(t.TempDir(), "mariadb")
	calls := 0
	install := func(dir string) error {
		calls++
		if dir != data+".init" {
			t.Errorf("installed into %s, want %s.init", dir, data)
		}
		return os.MkdirAll(filepath.Join(dir, "mysql"), 0o700)
	}
	for range 2 {
		if err := ensureMariaDBDataDir(data, install); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("installer ran %d times, want 1", calls)
	}
	if _, err := os.Stat(filepath.Join(data, "mysql")); err != nil {
		t.Fatalf("system tables not in place: %v", err)
	}
}

func TestEnsureMariaDBDataDirDiscardsTornInstall(t *testing.T) {
	data := filepath.Join(t.TempDir(), "mariadb")
	stale := filepath.Join(data+".init", "half-written")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	install := func(dir string) error {
		if _, err := os.Stat(stale); err == nil {
			t.Error("installer saw the torn install")
		}
		return os.MkdirAll(filepath.Join(dir, "mysql"), 0o700)
	}
	if err := ensureMariaDBDataDir(data, install); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureMariaDBDataDirInstallFails(t *testing.T) {
	data := filepath.Join(t.TempDir(), "mariadb")
	boom := errors.New("boom")
	if err := ensureMariaDBDataDir(data, func(string) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data dir exists after a failed install: %v", err)
	}
}
