// Command shim is PID 1 of every game container and the helper for the
// prepare, probe and install Job containers. See ARCHITECTURE.md section 6.4.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Claiyc/pelican-k8s/internal/shim/cgroup"
	"github.com/Claiyc/pelican-k8s/internal/shim/prepare"
	"github.com/Claiyc/pelican-k8s/internal/shim/supervisor"
	"github.com/Claiyc/pelican-k8s/internal/version"
)

func usage() {
	fmt.Fprintf(os.Stderr, `pelican-k8s shim %s

Usage:
  shim run       --socket PATH [--argv-file PATH] [--dir DIR] [--tmp DIR] [--ring-size N] -- [argv...]
  shim prepare   --bin PATH --shared DIR --data DIR --uuid UUID
  shim probe     --out PATH [--passwd PATH --group PATH --name NAME --home DIR --uid N --gid N] [-- argv...]
  shim install-run --log PATH --exit PATH [--chown UID:GID --chown-path DIR] -- command args...
  shim version
`, version.Version)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:], logger)
	case "prepare":
		err = prepareCmd(os.Args[2:])
	case "probe":
		err = probeCmd(os.Args[2:])
	case "install-run":
		err = installRunCmd(os.Args[2:])
	case "version":
		fmt.Println(version.Version)
	default:
		usage()
	}
	if err != nil {
		logger.Error("shim failed", "command", os.Args[1], "error", err)
		os.Exit(1)
	}
}

func splitDashDash(args []string) (flags, rest []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func runCmd(args []string, logger *slog.Logger) error {
	flags, rest := splitDashDash(args)
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	socket := fs.String("socket", "/pelican/run/shim.sock", "unix socket path")
	argvFile := fs.String("argv-file", "/pelican/etc/argv", "JSON array with the image entrypoint, used when no argv follows --")
	dir := fs.String("dir", "/home/container", "working directory of the process")
	tmp := fs.String("tmp", "/tmp", "directory emptied before each start (empty disables)")
	ring := fs.Int("ring-size", 1<<20, "output ring buffer size in bytes")
	stats := fs.Duration("stats-interval", 2*time.Second, "resource sampling interval")
	grace := fs.Duration("kill-grace", 10*time.Second, "SIGTERM to SIGKILL grace when the shim is terminated")
	noStats := fs.Bool("no-stats", false, "disable cgroup sampling")
	_ = fs.Parse(flags)

	o := supervisor.Options{Socket: *socket, Argv: rest, ArgvFile: *argvFile, Dir: *dir, TmpDir: *tmp, RingSize: *ring, StatsInterval: *stats, KillGrace: *grace, Stdout: os.Stdout, Logger: logger}
	if !*noStats {
		cg := cgroup.New()
		if cg.Available() {
			o.Cgroup = cg
		} else {
			logger.Warn("cgroup v2 not available, stats disabled")
		}
	}
	return supervisor.New(o).Run(context.Background())
}

func prepareCmd(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	bin := fs.String("bin", "/pelican/bin/shim", "destination for the shim binary")
	shared := fs.String("shared", "/pelican", "shared emptyDir")
	data := fs.String("data", "/data", "PVC root")
	uuid := fs.String("uuid", os.Getenv("PELICAN_SERVER_UUID"), "server uuid")
	_ = fs.Parse(args)
	return prepare.Layout{Bin: *bin, Shared: *shared, Data: *data, UUID: *uuid}.Run()
}

func probeCmd(args []string) error {
	flags, rest := splitDashDash(args)
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	out := fs.String("out", "/pelican/etc/argv", "argv output file")
	passwd := fs.String("passwd", "", "generated passwd file (empty skips)")
	group := fs.String("group", "", "generated group file (empty skips)")
	name := fs.String("name", "container", "user name")
	home := fs.String("home", "/home/container", "home directory")
	uid := fs.Int("uid", os.Getuid(), "uid")
	gid := fs.Int("gid", os.Getgid(), "gid")
	root := fs.String("root", "/", "filesystem root to inspect")
	_ = fs.Parse(flags)
	return prepare.Probe{Root: *root, ArgvOut: *out, PasswdOut: *passwd, GroupOut: *group, Name: *name, Home: *home, UID: *uid, GID: *gid, Argv: rest}.Run()
}

func installRunCmd(args []string) error {
	flags, rest := splitDashDash(args)
	fs := flag.NewFlagSet("install-run", flag.ExitOnError)
	logf := fs.String("log", "/pelican/install/output.log", "output log file")
	exitf := fs.String("exit", "/pelican/install/exit-code", "exit code file")
	chown := fs.String("chown", "", "UID:GID to chown the server directory to after the script")
	chownPath := fs.String("chown-path", "/mnt/server", "directory to chown")
	dir := fs.String("dir", "/", "working directory")
	_ = fs.Parse(flags)
	r := prepare.InstallRun{Argv: rest, LogFile: *logf, ExitFile: *exitf, Dir: *dir, Stdout: os.Stdout}
	if *chown != "" {
		u, g, ok := strings.Cut(*chown, ":")
		if !ok {
			return fmt.Errorf("invalid --chown %q", *chown)
		}
		var err error
		if r.UID, err = strconv.Atoi(u); err != nil {
			return err
		}
		if r.GID, err = strconv.Atoi(g); err != nil {
			return err
		}
		r.ChownPath = *chownPath
	}
	code, err := r.Run(context.Background())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "install script exited with code %d\n", code)
	return nil
}
