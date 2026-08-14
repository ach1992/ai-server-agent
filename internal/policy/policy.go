package policy

import (
	"regexp"
	"strings"
)

type Decision struct {
	Allowed          bool   `json:"allowed"`
	RequiresApproval bool   `json:"requires_approval"`
	Category         string `json:"category"`
	Reason           string `json:"reason"`
}

type Guard struct {
	protected []string
	critical  []*regexp.Regexp
	dangerous []*regexp.Regexp
}

func New(protected []string) *Guard {
	compile := func(xs []string) []*regexp.Regexp {
		out := make([]*regexp.Regexp, 0, len(xs))
		for _, x := range xs {
			out = append(out, regexp.MustCompile(x))
		}
		return out
	}
	return &Guard{
		protected: protected,
		critical: compile([]string{
			`(?i)\bsystemctl\s+(stop|disable|mask)\s+ai-server-agent`,
			`(?i)\bpkill\b.*ai-server-agent`, `(?i)\bkillall\b.*ai-server-agent`,
			`(?i)\b(reboot|shutdown|poweroff|halt)\b`,
			`(?i)\b(iptables|nft)\b.*\b(flush|-F)\b`,
			`(?i)\bufw\s+(disable|reset)\b`,
			`(?i)\b(systemctl\s+(stop|disable)\s+ssh|sshd)\b`,
		}),
		dangerous: compile([]string{
			`(?i)\brm\s+(-[^ ]*r[^ ]*f|-[^ ]*f[^ ]*r)\b`,
			`(?i)\bmkfs(\.|\s)`, `(?i)\b(fdisk|parted|wipefs)\b`,
			`(?i)\bDROP\s+(DATABASE|SCHEMA|TABLE)\b`,
			`(?i)\bdocker\s+(system|volume)\s+prune\b`,
			`(?i)\buserdel\b`, `(?i)\bgroupdel\b`,
		}),
	}
}

func (g *Guard) Evaluate(command string, root bool) Decision {
	s := strings.TrimSpace(command)
	if s == "" {
		return Decision{Allowed: false, Category: "invalid", Reason: "empty command"}
	}
	if !root {
		return Decision{Allowed: true, Category: "worker", Reason: "runs as the unprivileged worker user"}
	}
	for _, p := range g.protected {
		if p != "" && strings.Contains(s, p) {
			return Decision{Allowed: true, RequiresApproval: true, Category: "connection-risk", Reason: "command references an AI Server Agent protected resource"}
		}
	}
	for _, r := range g.critical {
		if r.MatchString(s) {
			return Decision{Allowed: true, RequiresApproval: true, Category: "connection-risk", Reason: "command can interrupt connectivity or the control plane"}
		}
	}
	for _, r := range g.dangerous {
		if r.MatchString(s) {
			return Decision{Allowed: true, RequiresApproval: true, Category: "destructive", Reason: "command contains a potentially destructive operation"}
		}
	}
	return Decision{Allowed: true, Category: "root", Reason: "privileged command allowed"}
}
