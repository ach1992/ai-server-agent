package executor

import (
	"regexp"
	"strconv"
	"strings"
)

var environmentSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)
var environmentVersionRe = regexp.MustCompile(`(?i)(?:^|[^0-9])v?([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?`)

type environmentVersion struct {
	parts [3]int
	count int
}

func fullEnvironmentSHA(value string) bool {
	return environmentSHARe.MatchString(value)
}

func parseEnvironmentVersion(value string) (environmentVersion, bool) {
	match := environmentVersionRe.FindStringSubmatch(value)
	if len(match) == 0 {
		return environmentVersion{}, false
	}
	var out environmentVersion
	for i := 1; i <= 3; i++ {
		if match[i] == "" {
			continue
		}
		n, err := strconv.Atoi(match[i])
		if err != nil {
			return environmentVersion{}, false
		}
		out.parts[i-1] = n
		out.count = i
	}
	return out, out.count > 0
}

func compareEnvironmentVersion(a, b environmentVersion) int {
	for i := 0; i < 3; i++ {
		if a.parts[i] < b.parts[i] {
			return -1
		}
		if a.parts[i] > b.parts[i] {
			return 1
		}
	}
	return 0
}

func sameEnvironmentPrefix(host, required environmentVersion) bool {
	for i := 0; i < required.count; i++ {
		if host.parts[i] != required.parts[i] {
			return false
		}
	}
	return true
}

func evaluateEnvironmentRequirements(hostVersion string, requirements []EnvironmentRequirement) (string, string) {
	if len(requirements) == 0 {
		return "compatible", ""
	}
	host, ok := parseEnvironmentVersion(hostVersion)
	if !ok {
		return "unknown", "host_version_unrecognized"
	}

	exact := map[string]bool{}
	unknown := false
	for _, requirement := range requirements {
		if requirement.Mode == "exact" {
			if v, ok := parseEnvironmentVersion(requirement.Value); ok {
				key := strconv.Itoa(v.parts[0]) + "." + strconv.Itoa(v.parts[1]) + "." + strconv.Itoa(v.parts[2])
				exact[key] = true
			}
		}
	}
	if len(exact) > 1 {
		return "conflict", "conflicting_exact_repository_requirements"
	}

	for _, requirement := range requirements {
		match, known := environmentRequirementMatches(host, requirement)
		if !known {
			unknown = true
			continue
		}
		if !match {
			return "incompatible", "host_version_does_not_satisfy_repository_requirement"
		}
	}
	if unknown {
		return "unknown", "repository_version_constraint_not_fully_evaluated"
	}
	return "compatible", ""
}

func environmentRequirementMatches(host environmentVersion, requirement EnvironmentRequirement) (bool, bool) {
	value := strings.TrimSpace(requirement.Value)
	if value == "" {
		return true, true
	}
	switch requirement.Mode {
	case "minimum", "preferred":
		required, ok := parseEnvironmentVersion(value)
		if !ok {
			return false, false
		}
		return compareEnvironmentVersion(host, required) >= 0, true
	case "exact":
		required, ok := parseEnvironmentVersion(value)
		if !ok {
			return false, false
		}
		return sameEnvironmentPrefix(host, required), true
	case "constraint":
		return matchEnvironmentConstraint(host, value)
	case "channel":
		return false, false
	default:
		return false, false
	}
}

func matchEnvironmentConstraint(host environmentVersion, raw string) (bool, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		return true, true
	}
	if strings.Contains(raw, "||") || strings.Contains(raw, " - ") {
		return false, false
	}
	raw = strings.ReplaceAll(raw, ",", " ")
	tokens := strings.Fields(raw)
	if len(tokens) == 0 {
		return false, false
	}
	knownAny := false
	for _, token := range tokens {
		match, known := matchEnvironmentConstraintToken(host, token)
		if !known {
			return false, false
		}
		knownAny = true
		if !match {
			return false, true
		}
	}
	return true, knownAny
}

func matchEnvironmentConstraintToken(host environmentVersion, token string) (bool, bool) {
	token = strings.TrimSpace(token)
	if token == "" || token == "*" {
		return true, true
	}
	operator := ""
	value := token
	for _, candidate := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(token, candidate) {
			operator = candidate
			value = strings.TrimSpace(strings.TrimPrefix(token, candidate))
			break
		}
	}

	lower := strings.ToLower(value)
	if strings.Contains(lower, "x") || strings.Contains(lower, "*") {
		parts := strings.Split(strings.ReplaceAll(lower, "*", "x"), ".")
		for i, part := range parts {
			if part == "x" {
				return true, true
			}
			n, err := strconv.Atoi(strings.TrimPrefix(part, "v"))
			if err != nil || i >= len(host.parts) {
				return false, false
			}
			if host.parts[i] != n {
				return false, true
			}
		}
		return true, true
	}

	required, ok := parseEnvironmentVersion(value)
	if !ok {
		return false, false
	}
	cmp := compareEnvironmentVersion(host, required)
	switch operator {
	case ">=":
		return cmp >= 0, true
	case ">":
		return cmp > 0, true
	case "<=":
		return cmp <= 0, true
	case "<":
		return cmp < 0, true
	case "=":
		return sameEnvironmentPrefix(host, required), true
	case "^":
		if cmp < 0 {
			return false, true
		}
		if required.parts[0] > 0 {
			return host.parts[0] == required.parts[0], true
		}
		if required.parts[1] > 0 {
			return host.parts[0] == 0 && host.parts[1] == required.parts[1], true
		}
		return host.parts[0] == 0 && host.parts[1] == 0 && host.parts[2] == required.parts[2], true
	case "~":
		if cmp < 0 {
			return false, true
		}
		if required.count >= 2 {
			return host.parts[0] == required.parts[0] && host.parts[1] == required.parts[1], true
		}
		return host.parts[0] == required.parts[0], true
	case "":
		return sameEnvironmentPrefix(host, required), true
	default:
		return false, false
	}
}
