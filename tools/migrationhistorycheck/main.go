package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	defaultBaseRef      = "origin/main"
	defaultMigrationDir = "internal/db/migrations"
	baselineResetMarker = defaultMigrationDir + "/BASELINE_RESET"
)

var migrationFilename = regexp.MustCompile(`^(\d{6})_([a-z0-9][a-z0-9_]*)\.(up|down)\.sql$`)

type migrationIdentity struct {
	number    string
	name      string
	direction string
}

func main() {
	ctx := context.Background()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	baseRef := getenvDefault("BENCHDB_MIGRATION_BASE_REF", defaultBaseRef)
	comparisonRef := baseRef
	if strings.HasPrefix(os.Getenv("GITHUB_REF"), "refs/pull/") &&
		strings.HasSuffix(os.Getenv("GITHUB_REF"), "/merge") &&
		gitSucceeds(ctx, "rev-parse", "--verify", "--quiet", "HEAD^1") {
		comparisonRef = "HEAD^1"
	}
	if !gitSucceeds(ctx, "rev-parse", "--verify", "--quiet", comparisonRef+"^{commit}") {
		return fmt.Errorf(
			"migration comparison ref %s is unavailable; fetch it or set BENCHDB_MIGRATION_BASE_REF",
			comparisonRef,
		)
	}

	baseFiles, err := gitLines(ctx, "ls-tree", "-r", "--name-only", comparisonRef, "--", defaultMigrationDir)
	if err != nil {
		return fmt.Errorf("list base migrations: %w", err)
	}

	candidateFiles, err := gitLines(ctx, "ls-files", "--cached", "--", defaultMigrationDir)
	if err != nil {
		return fmt.Errorf("inspect candidate migrations: %w", err)
	}
	changedPaths, err := gitLines(ctx, "diff", "--name-only", comparisonRef, "--", defaultMigrationDir)
	if err != nil {
		return fmt.Errorf("inspect candidate migrations: %w", err)
	}
	if err := validateMigrationHistory(baseFiles, candidateFiles, changedPaths); err != nil {
		return fmt.Errorf("migration history check failed: %w", err)
	}
	return nil
}

func validateMigrationHistory(baseFiles, candidateFiles, changedPaths []string) error {
	basePaths := make(map[string]struct{}, len(baseFiles))
	baseNames := make(map[string]struct{})
	for _, file := range baseFiles {
		basePaths[file] = struct{}{}
		if file == baselineResetMarker {
			continue
		}
		if identity, ok := parseMigrationIdentity(file); ok {
			baseNames[identity.name] = struct{}{}
		}
	}
	baselineReset := slices.Contains(candidateFiles, baselineResetMarker) &&
		!slices.Contains(baseFiles, baselineResetMarker)

	for _, changed := range changedPaths {
		if _, exists := basePaths[changed]; exists && !baselineReset {
			return fmt.Errorf("migration %s already exists on the comparison base and is immutable", changed)
		}
	}

	type directions struct{ up, down bool }
	byName := make(map[string]directions)
	namesByNumber := make(map[string]map[string]struct{})
	for _, file := range candidateFiles {
		if file == baselineResetMarker {
			continue
		}
		identity, ok := parseMigrationIdentity(file)
		if !ok {
			return fmt.Errorf("migration filename %s must match NNNNNN_description.(up|down).sql", file)
		}
		entry := byName[identity.name]
		if identity.direction == "up" {
			entry.up = true
		} else {
			entry.down = true
		}
		byName[identity.name] = entry
		if namesByNumber[identity.number] == nil {
			namesByNumber[identity.number] = make(map[string]struct{})
		}
		namesByNumber[identity.number][identity.name] = struct{}{}
	}

	for number, names := range namesByNumber {
		if len(names) > 1 {
			return fmt.Errorf("migration number %s is assigned to multiple names", number)
		}
	}
	numbers := make([]string, 0, len(namesByNumber))
	for number := range namesByNumber {
		numbers = append(numbers, number)
	}
	slices.Sort(numbers)
	for index, number := range numbers {
		expected := fmt.Sprintf("%06d", index+1)
		if number != expected {
			return fmt.Errorf("expected migration %s, found %s", expected, number)
		}
	}
	for name, entry := range byName {
		if !entry.up || !entry.down {
			return fmt.Errorf("migration %s must have matching .up.sql and .down.sql files", name)
		}
	}
	if baselineReset {
		if len(byName) != 1 || len(numbers) != 1 || numbers[0] != "000001" {
			return errors.New("baseline reset must leave exactly one 000001 migration pair")
		}
		return nil
	}

	var newNames []string
	for name := range byName {
		if _, exists := baseNames[name]; !exists {
			newNames = append(newNames, name)
		}
	}
	slices.Sort(newNames)
	if len(baseNames) > 0 && len(newNames) > 1 {
		return fmt.Errorf("only one new migration is allowed per pull request; found %s", strings.Join(newNames, ", "))
	}
	return nil
}

func parseMigrationIdentity(file string) (migrationIdentity, bool) {
	matches := migrationFilename.FindStringSubmatch(filepath.Base(file))
	if matches == nil {
		return migrationIdentity{}, false
	}
	return migrationIdentity{
		number:    matches[1],
		name:      matches[1] + "_" + matches[2],
		direction: matches[3],
	}, true
}

func gitLines(ctx context.Context, args ...string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	var lines []string
	for line := range strings.SplitSeq(string(output), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func gitSucceeds(ctx context.Context, args ...string) bool {
	cmd := exec.CommandContext(ctx, "git", args...)
	return cmd.Run() == nil
}

func getenvDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
