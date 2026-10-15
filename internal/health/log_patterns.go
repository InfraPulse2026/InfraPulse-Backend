package health

import (
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"net"
	"regexp"
	"strconv"
	"strings"
)

var oomKillPattern = regexp.MustCompile(`^(Out of memory|Memory cgroup out of memory): Killed process [1-9][0-9]* \([^\r\n()]{1,64}\) total-vm:[0-9]+kB, anon-rss:[0-9]+kB, file-rss:[0-9]+kB, shmem-rss:[0-9]+kB, UID:[0-9]+ pgtables:[0-9]+kB oom_score_adj:-?[0-9]+$`)
var sshPasswordPattern = regexp.MustCompile(`^Failed password for (?:invalid user )?[^\s\r\n]{1,100} from ([0-9A-Fa-f:.]{2,200}) port ([0-9]{1,5}) ssh2(?: \[preauth\])?$`)

// Closed catalog over a parsed message and exact process tag. Text is data:
// names, addresses and PID captures are never exported as incident facts.
func LogPatternRule(r logbook.Record) string {
	if r.Truncated || len(r.Message) > 8192 || strings.ContainsAny(r.Message, "\r\n\x00") {
		return ""
	}
	if r.Process == "kernel" && oomKillPattern.MatchString(r.Message) {
		return "log.kernel_oom_kill"
	}
	if r.Process == "sshd" || r.Process == "sshd-session" {
		match := sshPasswordPattern.FindStringSubmatch(r.Message)
		if len(match) == 3 && net.ParseIP(match[1]) != nil {
			port, err := strconv.Atoi(match[2])
			if err == nil && port > 0 && port <= 65535 {
				return "log.ssh_password_failure"
			}
		}
	}
	return ""
}
