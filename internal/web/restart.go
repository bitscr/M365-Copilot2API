package web

import (
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// restartDelay lets the in-flight HTTP response reach the browser before the
// process is replaced or asked to exit.
const restartDelay = 700 * time.Millisecond

// restartRequiredFields are the settings that are only read once, at process
// startup. Changing any of them has no effect until the gateway restarts.
var restartRequiredFields = []string{"listenAddress", "configPath", "tokenCachePath", "sessionCachePath", "outboundProxy", "proxyPool", "clientId", "authority", "redirectUri", "scope", "debugLogPath"}

// detectSystemdUnit returns the systemd unit that owns this process, or "" when
// the process is not managed by systemd. Works for both cgroup v1
// ("1:name=systemd:/system.slice/foo.service") and v2
// ("0::/system.slice/foo.service").
func detectSystemdUnit() string {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return ""
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return ""
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, ".service") {
			continue
		}
		if idx := strings.LastIndex(line, "/"); idx >= 0 {
			unit := strings.TrimSpace(line[idx+1:])
			if strings.HasSuffix(unit, ".service") {
				return unit
			}
		}
	}
	return ""
}

func inDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	// cgroup-based detection for runtimes that do not create /.dockerenv.
	b, err := os.ReadFile("/proc/self/cgroup")
	return err == nil && strings.Contains(string(b), "docker")
}

// RestartStrategy reports how the running gateway can restart itself.
func RestartStrategy() string {
	if unit := detectSystemdUnit(); unit != "" {
		return "systemd:" + unit
	}
	if inDocker() {
		return "docker"
	}
	return "exec"
}

// scheduleRestart restarts the gateway shortly after the current HTTP response
// is flushed. The restart command follows how the process was installed:
//   - systemd: `systemctl --no-block restart <unit>` (the install-time unit)
//   - docker:  exit 0 and let the container restart policy bring it back
//   - other:   re-exec the binary in place
func scheduleRestart() {
	go func() {
		time.Sleep(restartDelay)
		strategy := RestartStrategy()
		log.Printf("[restart] strategy=%s", strategy)
		switch {
		case strings.HasPrefix(strategy, "systemd:"):
			unit := strings.TrimPrefix(strategy, "systemd:")
			// --no-block enqueues the job with PID 1 and returns immediately, so
			// the restart still happens even though this process (and the
			// systemctl child in its cgroup) is about to be signalled.
			if err := exec.Command("systemctl", "--no-block", "restart", unit).Run(); err != nil {
				log.Printf("[restart] systemctl restart %s failed: %v", unit, err)
			}
		case strategy == "docker":
			log.Printf("[restart] exiting 0 for the container restart policy")
			StopPersistLoop()
			os.Exit(0)
		default:
			exe, err := os.Executable()
			if err != nil {
				log.Printf("[restart] cannot resolve executable: %v", err)
				return
			}
			log.Printf("[restart] re-exec %s", exe)
			if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
				log.Printf("[restart] re-exec failed: %v", err)
			}
		}
	}()
}

// adminRestart schedules a restart and answers before the process goes away.
func (s *Server) adminRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	jsonOut(w, map[string]any{"ok": true, "strategy": RestartStrategy()})
	scheduleRestart()
}
