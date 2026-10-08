package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// mariadbName bounds database and user names so they never need quoting beyond backticks.
var mariadbName = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// mariadbSetup is everything one db/mariadb instance needs.
type mariadbSetup struct {
	DataDir, Port, RootPassword string
	Database, User, Password    string
	BufferPool                  string
	MaxConnections              int
}

// mariadbSetupFrom applies the block's defaults to spec.config and validates it.
func mariadbSetupFrom(mountPath string, cfg map[string]any) (mariadbSetup, error) {
	s := mariadbSetup{
		DataDir:        filepath.Join(mountPath, "mariadb"),
		Port:           cfgPortOr(cfg, "13306"),
		RootPassword:   cfgStr(cfg, "rootPassword"),
		Database:       cfgStr(cfg, "database"),
		User:           cfgStr(cfg, "user"),
		Password:       cfgStr(cfg, "password"),
		BufferPool:     cfgStr(cfg, "bufferPool"),
		MaxConnections: cfgInt(cfg, "maxConnections", 151),
	}
	if s.BufferPool == "" {
		s.BufferPool = "128M"
	}
	switch {
	case s.RootPassword == "":
		return mariadbSetup{}, errors.New("db/mariadb: rootPassword is required")
	case s.Database != "" && !mariadbName.MatchString(s.Database):
		return mariadbSetup{}, fmt.Errorf("db/mariadb: database %q must match %s", s.Database, mariadbName)
	case s.User != "" && !mariadbName.MatchString(s.User):
		return mariadbSetup{}, fmt.Errorf("db/mariadb: user %q must match %s", s.User, mariadbName)
	case s.User != "" && (s.Password == "" || s.Database == ""):
		return mariadbSetup{}, errors.New("db/mariadb: user needs a password and a database")
	case s.User == "" && s.Password != "":
		return mariadbSetup{}, errors.New("db/mariadb: password is set without a user")
	}
	return s, nil
}

// sqlString quotes s as a MariaDB string literal (default sql_mode).
func sqlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\x00", `\0`, "\n", `\n`, "\r", `\r`, "\x1a", `\Z`)
	return "'" + r.Replace(s) + "'"
}

// mariadbConfig renders my.cnf: state on the volume, sockets and scratch in work.
func mariadbConfig(s mariadbSetup, work string) string {
	return fmt.Sprintf(`[mariadbd]
datadir=%s
port=%s
bind-address=0.0.0.0
socket=%s
pid-file=%s
tmpdir=%s
init-file=%s
skip-name-resolve
innodb_buffer_pool_size=%s
max_connections=%d
# Every commit reaches the volume before it is acknowledged: failover is a crash.
innodb_flush_log_at_trx_commit=1
innodb_doublewrite=1
character-set-server=utf8mb4
`, s.DataDir, s.Port, filepath.Join(work, "mariadbd.sock"), filepath.Join(work, "mariadbd.pid"),
		filepath.Join(work, "tmp"), filepath.Join(work, "init.sql"), s.BufferPool, s.MaxConnections)
}

// mariadbInitSQL converges accounts and the database on every start, one statement per line.
func mariadbInitSQL(s mariadbSetup) string {
	var b strings.Builder
	// The installer adds passwordless root@127.0.0.1, root@::1 and root@<hostname>.
	b.WriteString("DELETE FROM mysql.global_priv WHERE User IN ('root', '') AND Host NOT IN ('localhost', '%');\n")
	b.WriteString("FLUSH PRIVILEGES;\n")
	account := func(user, host, password string) {
		acct := sqlString(user) + "@" + sqlString(host)
		fmt.Fprintf(&b, "CREATE USER IF NOT EXISTS %s;\n", acct)
		fmt.Fprintf(&b, "ALTER USER %s IDENTIFIED BY %s;\n", acct, sqlString(password))
	}
	for _, host := range []string{"localhost", "%"} {
		account("root", host, s.RootPassword)
		fmt.Fprintf(&b, "GRANT ALL PRIVILEGES ON *.* TO 'root'@%s WITH GRANT OPTION;\n", sqlString(host))
	}
	if s.Database != "" {
		fmt.Fprintf(&b, "CREATE DATABASE IF NOT EXISTS `%s`;\n", s.Database)
	}
	if s.User != "" {
		account(s.User, "%", s.Password)
		fmt.Fprintf(&b, "GRANT ALL PRIVILEGES ON `%s`.* TO %s@'%%';\n", s.Database, sqlString(s.User))
	}
	return b.String()
}

// ensureMariaDBDataDir creates the system tables once, in a side directory
// renamed into place, so a crash mid-install never leaves a half datadir.
func ensureMariaDBDataDir(dataDir string, install func(dir string) error) error {
	if _, err := os.Stat(filepath.Join(dataDir, "mysql")); err == nil {
		return nil
	}
	tmp := dataDir + ".init"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := install(tmp); err != nil {
		return err
	}
	if err := os.RemoveAll(dataDir); err != nil {
		return err
	}
	if err := os.Rename(tmp, dataDir); err != nil {
		return err
	}
	return syncFS(filepath.Dir(dataDir))
}

// runMariaDB serves db/mariadb from the volume; the SINGLETON strategy keeps one writer.
func runMariaDB(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	mountPath := firstMount(mountPaths(args))
	if mountPath == "" {
		return errors.New("db/mariadb: no bound storage mount yet")
	}
	if err := waitForMount(mountPath, 30*time.Second, 500*time.Millisecond, realStat); err != nil {
		return fmt.Errorf("db/mariadb: %w", err)
	}
	s, err := mariadbSetupFrom(mountPath, cfg)
	if err != nil {
		return err
	}
	bin, err := resolveBin("mariadbd")
	if err != nil {
		return fmt.Errorf("block runtime: mariadbd not found in PATH (ship the package): %w", err)
	}
	basedir := filepath.Dir(filepath.Dir(bin))
	install := func(dir string) error {
		cmd := exec.CommandContext(ctx, filepath.Join(basedir, "bin", "mariadb-install-db"), "--no-defaults",
			"--basedir="+basedir, "--datadir="+dir, "--auth-root-authentication-method=normal",
			"--skip-test-db", "--skip-name-resolve")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	if err := mkdirAllRetrying(os.MkdirAll, mountPath, 0o700, 10, 500*time.Millisecond); err != nil {
		return fmt.Errorf("db/mariadb: %w", err)
	}
	if err := ensureMariaDBDataDir(s.DataDir, install); err != nil {
		return fmt.Errorf("db/mariadb: initialising %s: %w", s.DataDir, err)
	}
	work := "/tmp/expblk-" + instance
	if err := os.MkdirAll(filepath.Join(work, "tmp"), 0o700); err != nil {
		return err
	}
	conf := filepath.Join(work, "my.cnf")
	if err := os.WriteFile(conf, []byte(mariadbConfig(s, work)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(work, "init.sql"), []byte(mariadbInitSQL(s)), 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "--defaults-file="+conf)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// A deliberate stop shuts down cleanly; SIGKILL only if that hangs.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Minute
	fmt.Printf("expanse-block-run: mariadb serving on :%s (database %q)\n", s.Port, s.Database)
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil //nolint:nilerr // deliberate stop
	}
	return err
}
