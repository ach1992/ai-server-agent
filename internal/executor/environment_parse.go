package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var quotedRequirementRe = regexp.MustCompile(`(?i)^[[:space:]]*([A-Za-z0-9_.-]+)[[:space:]]*=[[:space:]]*["']([^"']+)["']`)
var pyRequirementRe = regexp.MustCompile(`(?i)requires-python[[:space:]]*=[[:space:]]*["']([^"']+)["']`)
var rustChannelRe = regexp.MustCompile(`(?i)channel[[:space:]]*=[[:space:]]*["']([^"']+)["']`)
var makeTargetRe = regexp.MustCompile(`^([A-Za-z0-9_.-]+)[[:space:]]*:`)

func (b *environmentBuilder) discover(ctx context.Context) error {
	if data, ok := b.readDeclaration(ctx, "go.mod", "language_manifest"); ok {
		b.addLanguage("go", "go.mod")
		goVersion, toolchain := parseGoMod(data)
		b.addTool("Go", "go", "language", true, repositoryRequirement(goVersion, "minimum", "go.mod"))
		if toolchain != "" {
			b.addTool("Go", "go", "language", true, repositoryRequirement(strings.TrimPrefix(toolchain, "go"), "preferred", "go.mod"))
		}
	}
	b.recordPresence(ctx, "go.sum", "lockfile")

	nodeManagers := map[string]bool{}
	if data, ok := b.readDeclaration(ctx, "package.json", "language_manifest"); ok {
		b.addLanguage("node", "package.json")
		var pkg struct {
			Engines        map[string]string `json:"engines"`
			PackageManager string            `json:"packageManager"`
			Scripts        map[string]string `json:"scripts"`
		}
		if len(data) > 0 && json.Unmarshal(data, &pkg) == nil {
			b.addTool("Node.js", "node", "language", true, repositoryRequirement(pkg.Engines["node"], "constraint", "package.json"))
			if manager, version := parsePackageManager(pkg.PackageManager); manager != "" {
				nodeManagers[manager] = true
				b.addTool(manager, manager, "package_manager", true, repositoryRequirement(version, "exact", "package.json"))
			}
			names := make([]string, 0, len(pkg.Scripts))
			for name := range pkg.Scripts {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				b.addEntrypoint(name, "package_script", "package.json", pkg.Scripts[name])
			}
		} else {
			b.addTool("Node.js", "node", "language", true, nil)
			if len(data) > 0 {
				b.warnings = append(b.warnings, "package.json could not be parsed; Node.js presence was detected but detailed requirements/scripts are unknown")
			}
		}
	}
	for _, lock := range []struct {
		path    string
		manager string
	}{
		{"package-lock.json", "npm"},
		{"npm-shrinkwrap.json", "npm"},
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"bun.lock", "bun"},
		{"bun.lockb", "bun"},
	} {
		if b.recordPresence(ctx, lock.path, "lockfile") {
			nodeManagers[lock.manager] = true
			b.addLanguage("node", lock.path)
			b.addTool(lock.manager, lock.manager, "package_manager", true, nil)
		}
	}
	if len(nodeManagers) > 1 {
		b.requireSelection("multiple Node.js package-manager declarations/lockfiles disagree; no package-manager precedence was inferred")
	}

	pythonDeclared := false
	if data, ok := b.readDeclaration(ctx, ".python-version", "toolchain_pin"); ok {
		pythonDeclared = true
		b.addLanguage("python", ".python-version")
		b.addTool("Python", "python3", "language", true, repositoryRequirement(strings.TrimSpace(string(data)), "exact", ".python-version"))
	}
	if data, ok := b.readDeclaration(ctx, "pyproject.toml", "language_manifest"); ok {
		pythonDeclared = true
		b.addLanguage("python", "pyproject.toml")
		req := ""
		if match := pyRequirementRe.FindSubmatch(data); len(match) == 2 {
			req = string(match[1])
		}
		b.addTool("Python", "python3", "language", true, repositoryRequirement(req, "constraint", "pyproject.toml"))
	}
	for _, file := range []string{"requirements.txt", "Pipfile"} {
		if b.recordPresence(ctx, file, "language_manifest") {
			pythonDeclared = true
			b.addLanguage("python", file)
		}
	}
	for _, file := range []string{"poetry.lock", "uv.lock", "Pipfile.lock"} {
		b.recordPresence(ctx, file, "lockfile")
	}
	if pythonDeclared {
		b.addTool("Python", "python3", "language", true, nil)
	}

	rustDeclared := false
	if b.recordPresence(ctx, "Cargo.toml", "language_manifest") {
		rustDeclared = true
		b.addLanguage("rust", "Cargo.toml")
	}
	b.recordPresence(ctx, "Cargo.lock", "lockfile")
	if data, ok := b.readDeclaration(ctx, "rust-toolchain.toml", "toolchain_pin"); ok {
		rustDeclared = true
		req := ""
		if match := rustChannelRe.FindSubmatch(data); len(match) == 2 {
			req = string(match[1])
		}
		mode := requirementMode(req)
		b.addTool("Rust compiler", "rustc", "language", true, repositoryRequirement(req, mode, "rust-toolchain.toml"))
	}
	if data, ok := b.readDeclaration(ctx, "rust-toolchain", "toolchain_pin"); ok {
		rustDeclared = true
		req := strings.TrimSpace(string(data))
		b.addTool("Rust compiler", "rustc", "language", true, repositoryRequirement(req, requirementMode(req), "rust-toolchain"))
	}
	if rustDeclared {
		b.addLanguage("rust", "Cargo.toml")
		b.addTool("Rust compiler", "rustc", "language", true, nil)
		b.addTool("Cargo", "cargo", "package_manager", true, nil)
	}

	if data, ok := b.readDeclaration(ctx, "composer.json", "language_manifest"); ok {
		b.addLanguage("php", "composer.json")
		var composer struct {
			Require map[string]string `json:"require"`
		}
		req := ""
		if len(data) > 0 && json.Unmarshal(data, &composer) == nil {
			req = composer.Require["php"]
		}
		b.addTool("PHP", "php", "language", true, repositoryRequirement(req, "constraint", "composer.json"))
		b.addTool("Composer", "composer", "package_manager", true, nil)
	}
	b.recordPresence(ctx, "composer.lock", "lockfile")

	rubyDeclared := false
	if b.recordPresence(ctx, "Gemfile", "language_manifest") {
		rubyDeclared = true
		b.addLanguage("ruby", "Gemfile")
	}
	b.recordPresence(ctx, "Gemfile.lock", "lockfile")
	if data, ok := b.readDeclaration(ctx, ".ruby-version", "toolchain_pin"); ok {
		rubyDeclared = true
		b.addLanguage("ruby", ".ruby-version")
		b.addTool("Ruby", "ruby", "language", true, repositoryRequirement(strings.TrimSpace(string(data)), "exact", ".ruby-version"))
	}
	if rubyDeclared {
		b.addTool("Ruby", "ruby", "language", true, nil)
		b.addTool("Bundler", "bundle", "package_manager", true, nil)
	}

	if data, ok := b.readDeclaration(ctx, "global.json", "toolchain_pin"); ok {
		b.addLanguage("dotnet", "global.json")
		var global struct {
			SDK struct {
				Version string `json:"version"`
			} `json:"sdk"`
		}
		req := ""
		if len(data) > 0 && json.Unmarshal(data, &global) == nil {
			req = global.SDK.Version
		}
		b.addTool(".NET SDK", "dotnet", "language", true, repositoryRequirement(req, "exact", "global.json"))
	}
	b.discoverRootProjectFiles(ctx)

	javaDeclared := false
	if b.recordPresence(ctx, "pom.xml", "language_manifest") {
		javaDeclared = true
		b.addLanguage("java", "pom.xml")
		if b.regularFile("mvnw") {
			b.addEntrypoint("maven-wrapper", "build_wrapper", "mvnw", "./mvnw")
		} else {
			b.addTool("Maven", "mvn", "build_tool", true, nil)
		}
	}
	for _, file := range []string{"build.gradle", "build.gradle.kts"} {
		if b.recordPresence(ctx, file, "language_manifest") {
			javaDeclared = true
			b.addLanguage("java", file)
		}
	}
	if javaDeclared {
		b.addTool("Java", "java", "language", true, nil)
		if b.regularFile("gradlew") {
			b.addEntrypoint("gradle-wrapper", "build_wrapper", "gradlew", "./gradlew")
		} else if b.regularFile("build.gradle") || b.regularFile("build.gradle.kts") {
			b.addTool("Gradle", "gradle", "build_tool", true, nil)
		}
	}

	if b.recordPresence(ctx, "CMakeLists.txt", "language_manifest") {
		b.addLanguage("c-cpp", "CMakeLists.txt")
		b.addTool("CMake", "cmake", "build_tool", true, nil)
	}

	b.discoverEnvironmentMechanisms(ctx)
	b.discoverToolVersions(ctx)
	b.discoverEntrypoints(ctx)
	b.probeTools(ctx)
	return nil
}

func parseGoMod(data []byte) (goVersion, toolchain string) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "go ") && goVersion == "" {
			goVersion = strings.TrimSpace(strings.TrimPrefix(line, "go "))
		}
		if strings.HasPrefix(line, "toolchain ") && toolchain == "" {
			toolchain = strings.TrimSpace(strings.TrimPrefix(line, "toolchain "))
		}
	}
	return goVersion, toolchain
}

func parsePackageManager(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	at := strings.LastIndex(value, "@")
	if at <= 0 {
		return value, ""
	}
	name := value[:at]
	version := value[at+1:]
	if plus := strings.Index(version, "+"); plus >= 0 {
		version = version[:plus]
	}
	return name, version
}

func (b *environmentBuilder) recordPresence(ctx context.Context, rel, kind string) bool {
	_, ok := b.readDeclaration(ctx, rel, kind)
	return ok
}

func (b *environmentBuilder) regularFile(rel string) bool {
	info, err := os.Lstat(filepath.Join(b.root, filepath.FromSlash(rel)))
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}

func (b *environmentBuilder) discoverRootProjectFiles(ctx context.Context) {
	entries, err := os.ReadDir(b.root)
	if err != nil {
		b.warnings = append(b.warnings, "repository root could not be enumerated for bounded project-file discovery")
		return
	}
	if len(entries) > 4096 {
		b.warnings = append(b.warnings, "repository root contains more than 4096 entries; root-level project-file discovery was bounded")
		entries = entries[:4096]
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(strings.ToLower(name), ".csproj") {
			if b.recordPresence(ctx, name, "language_manifest") {
				b.addLanguage("dotnet", name)
				b.addTool(".NET SDK", "dotnet", "language", true, nil)
			}
		}
	}
}

func (b *environmentBuilder) discoverEnvironmentMechanisms(ctx context.Context) {
	var miseFiles []string
	for _, file := range []string{"mise.toml", ".mise.toml"} {
		if data, ok := b.readDeclaration(ctx, file, "environment"); ok {
			miseFiles = append(miseFiles, file)
			if len(data) > 0 {
				b.parseMiseTools(data, file)
			}
		}
	}
	if b.recordPresence(ctx, "mise.lock", "environment_lock") {
		miseFiles = append(miseFiles, "mise.lock")
	}
	if len(miseFiles) > 0 {
		b.addMechanism("mise", "mise", miseFiles...)
	}

	var devenvFiles []string
	for _, file := range []string{"devenv.nix", "devenv.yaml", "devenv.lock"} {
		if b.recordPresence(ctx, file, "environment") {
			devenvFiles = append(devenvFiles, file)
		}
	}
	if len(devenvFiles) > 0 {
		b.addMechanism("devenv", "devenv", devenvFiles...)
	}

	var devcontainerFiles []string
	for _, file := range []string{".devcontainer/devcontainer.json", "devcontainer.json"} {
		if b.recordPresence(ctx, file, "environment") {
			devcontainerFiles = append(devcontainerFiles, file)
		}
	}
	if len(devcontainerFiles) > 0 {
		b.addMechanism("devcontainer", "devcontainer", devcontainerFiles...)
	}

	if b.recordPresence(ctx, "dagger.json", "environment") {
		b.addMechanism("dagger", "dagger", "dagger.json")
	}
}

func (b *environmentBuilder) parseMiseTools(data []byte, file string) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	inTools := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inTools = line == "[tools]"
			continue
		}
		if !inTools {
			continue
		}
		match := quotedRequirementRe.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		executable := normalizeEnvironmentToolAlias(match[1])
		value := strings.TrimSpace(match[2])
		b.addTool(match[1], executable, "repository_toolchain", true, repositoryRequirement(value, requirementMode(value), file))
		b.addLanguageForTool(executable, file)
	}
}

func (b *environmentBuilder) discoverToolVersions(ctx context.Context) {
	if data, ok := b.readDeclaration(ctx, ".tool-versions", "toolchain_pin"); ok && len(data) > 0 {
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			executable := normalizeEnvironmentToolAlias(fields[0])
			value := fields[1]
			b.addTool(fields[0], executable, "repository_toolchain", true, repositoryRequirement(value, requirementMode(value), ".tool-versions"))
			b.addLanguageForTool(executable, ".tool-versions")
		}
	}
	for _, pin := range []struct {
		file       string
		name       string
		executable string
		language   string
	}{
		{".node-version", "Node.js", "node", "node"},
		{".nvmrc", "Node.js", "node", "node"},
		{".java-version", "Java", "java", "java"},
	} {
		if data, ok := b.readDeclaration(ctx, pin.file, "toolchain_pin"); ok {
			value := strings.TrimSpace(string(data))
			b.addLanguage(pin.language, pin.file)
			b.addTool(pin.name, pin.executable, "language", true, repositoryRequirement(value, requirementMode(value), pin.file))
		}
	}
}

func normalizeEnvironmentToolAlias(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "nodejs", "node":
		return "node"
	case "python", "python3":
		return "python3"
	case "golang", "go":
		return "go"
	case "rust", "rustc":
		return "rustc"
	case "ruby":
		return "ruby"
	case "php":
		return "php"
	case "java", "openjdk":
		return "java"
	case "dotnet":
		return "dotnet"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

func (b *environmentBuilder) addLanguageForTool(executable, declaredBy string) {
	switch executable {
	case "go":
		b.addLanguage("go", declaredBy)
	case "node":
		b.addLanguage("node", declaredBy)
	case "python3":
		b.addLanguage("python", declaredBy)
	case "rustc":
		b.addLanguage("rust", declaredBy)
	case "ruby":
		b.addLanguage("ruby", declaredBy)
	case "php":
		b.addLanguage("php", declaredBy)
	case "java":
		b.addLanguage("java", declaredBy)
	case "dotnet":
		b.addLanguage("dotnet", declaredBy)
	}
}

func requirementMode(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "constraint"
	}
	for _, channel := range []string{"latest", "system", "stable", "nightly", "lts", "lts/*"} {
		if value == channel {
			return "channel"
		}
	}
	if strings.ContainsAny(value, "<>=^~*|,") || strings.Contains(strings.ToLower(value), "x") {
		return "constraint"
	}
	return "exact"
}

func (b *environmentBuilder) discoverEntrypoints(ctx context.Context) {
	if data, ok := b.readDeclaration(ctx, "Makefile", "task_entrypoints"); ok {
		b.addTool("Make", "make", "task_runner", true, nil)
		if len(data) > 0 {
			scanner := bufio.NewScanner(strings.NewReader(string(data)))
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "\t") || strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				match := makeTargetRe.FindStringSubmatch(line)
				if len(match) != 2 || strings.Contains(match[1], "%") || strings.HasPrefix(match[1], ".") {
					continue
				}
				b.addEntrypoint(match[1], "make_target", "Makefile", "make "+match[1])
			}
		}
	}
	for _, taskfile := range []string{"Taskfile.yml", "Taskfile.yaml"} {
		if b.recordPresence(ctx, taskfile, "task_entrypoints") {
			b.addTool("Task", "task", "task_runner", true, nil)
			b.addEntrypoint("taskfile", "task_runner", taskfile, "task")
		}
	}
	for _, justfile := range []string{"justfile", "Justfile"} {
		if b.recordPresence(ctx, justfile, "task_entrypoints") {
			b.addTool("just", "just", "task_runner", true, nil)
			b.addEntrypoint("justfile", "task_runner", justfile, "just")
		}
	}
	for _, script := range []string{
		"scripts/setup.sh",
		"scripts/bootstrap.sh",
		"scripts/install.sh",
		"scripts/test.sh",
		"scripts/build.sh",
		"scripts/lint.sh",
		"scripts/check.sh",
		"scripts/ci.sh",
	} {
		if b.recordPresence(ctx, script, "task_entrypoint") {
			name := strings.TrimSuffix(filepath.Base(script), filepath.Ext(script))
			b.addEntrypoint(name, "script", script, "bash "+script)
		}
	}
}
