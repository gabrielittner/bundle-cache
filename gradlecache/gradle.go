package gradlecache

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/errors"
)

// gradleLayout is the Gradle implementation of Layout.
type gradleLayout struct {
	gradleUserHome string
	projectDir     string
	includedBuilds []string
}

// NewGradleLayout returns the layout of a Gradle build. gradleUserHome
// defaults to ~/.gradle and projectDir defaults to the working directory.
func NewGradleLayout(gradleUserHome, projectDir string, includedBuilds []string) Layout {
	if gradleUserHome == "" {
		home, _ := os.UserHomeDir()
		gradleUserHome = filepath.Join(home, ".gradle")
	}
	if projectDir == "" {
		projectDir = defaultProjectDir()
	}
	return gradleLayout{gradleUserHome: gradleUserHome, projectDir: projectDir, includedBuilds: includedBuilds}
}

func (l gradleLayout) StateDir() string { return l.gradleUserHome }

func (l gradleLayout) Sources() ([]TarSource, error) {
	cachesDir := filepath.Join(l.gradleUserHome, "caches")
	if _, err := os.Stat(cachesDir); err != nil {
		return nil, errors.Errorf("caches directory not found at %s: %w", cachesDir, err)
	}
	if err := validateProjectDir(l.projectDir); err != nil {
		return nil, err
	}
	sources := []TarSource{{BaseDir: l.gradleUserHome, Path: "./caches"}}
	if fi, err := os.Stat(filepath.Join(l.gradleUserHome, "wrapper")); err == nil && fi.IsDir() {
		sources = append(sources, TarSource{BaseDir: l.gradleUserHome, Path: "./wrapper"})
	}
	return append(sources, ProjectDirSources(l.projectDir, l.includedBuilds)...), nil
}

func (l gradleLayout) Exclusions() []string {
	return append(append([]string{}, CacheExclusions...), wrapperZipExclusion)
}

func (l gradleLayout) ExtractRules() []extractRule {
	return []extractRule{
		{prefix: "caches/", baseDir: l.gradleUserHome},
		{prefix: "wrapper/", baseDir: l.gradleUserHome},
		{prefix: "configuration-cache/", baseDir: filepath.Join(l.projectDir, ".gradle")},
	}
}

func (l gradleLayout) DefaultDir() string { return l.projectDir }

func (l gradleLayout) DeltaSources(since time.Time) ([]DeltaSource, error) {
	if err := validateProjectDir(l.projectDir); err != nil {
		return nil, err
	}
	cachesDir := filepath.Join(l.gradleUserHome, "caches")
	realCaches, err := filepath.EvalSymlinks(cachesDir)
	if err != nil {
		realCaches = cachesDir
	}
	newFiles, err := CollectNewFiles(realCaches, since, l.gradleUserHome)
	if err != nil {
		return nil, errors.Wrap(err, "walk caches dir")
	}
	sources := []DeltaSource{{BaseDir: l.gradleUserHome, RelPaths: newFiles}}

	// Collect new files from project-dir sources (configuration-cache, included builds).
	for _, src := range ProjectDirSources(l.projectDir, l.includedBuilds) {
		newProjectFiles := collectNewFilesSimple(filepath.Join(src.BaseDir, src.Path), src.BaseDir, since)
		if len(newProjectFiles) > 0 {
			sources = append(sources, DeltaSource{BaseDir: src.BaseDir, RelPaths: newProjectFiles})
		}
	}
	return sources, nil
}

// CacheExclusions are patterns for files and directories that should never be
// included in cache bundles (base or delta).
var CacheExclusions = []string{
	// daemon/ contains per-daemon state (registry, logs, pid files) that is tied
	// to a running Gradle daemon process. Daemons are never reused across CI runs,
	// so this directory is pure waste in the bundle.
	"daemon",

	// .tmp/ holds intermediate temp files created during builds. They are
	// irrelevant after the build finishes and would only inflate the archive.
	".tmp",

	// gc.properties records the last time Gradle ran its own cache cleanup GC. On
	// ephemeral CI workers this timestamp is meaningless and Gradle recreates the
	// file when needed.
	"gc.properties",

	// *.lock files are advisory filesystem locks held by running Gradle processes.
	// They are stale after the build and would cause spurious "already locked"
	// errors if restored on a different machine.
	"*.lock",

	// cc-keystore holds a randomly generated AES key used to encrypt configuration
	// cache entries. The key is unique to each machine, so restoring one machine's
	// keystore onto another causes decryption failures for any existing CC entries.
	// Gradle generates a fresh keystore (and re-encrypts) when the file is missing.
	// If the GRADLE_ENCRYPTION_KEY env var is set, Gradle uses that shared key
	// instead of the keystore, making this file unused entirely.
	"cc-keystore",

	// file-changes/ contains only last-build.bin, a 1-byte marker whose mtime
	// records when the previous build finished. Gradle uses it to distrust file
	// timestamps that match the previous build's end time, forcing a full content
	// hash instead. After tar extraction every file shares the same mtime, so
	// including this marker causes Gradle to needlessly rehash thousands of files.
	// Without it Gradle sets lastBuildTimestamp=0 and trusts all mtimes — strictly
	// better for restored caches.
	"file-changes",

	// journal-1/ holds file-access.bin, an indexed cache that maps absolute File
	// paths to last-access timestamps for Gradle's cache GC (evicts entries not
	// used in 30 days). The absolute-path keys are wrong on any machine with a
	// different workspace path, and cache GC is irrelevant on ephemeral CI workers.
	// Gradle recreates it on first use with inceptionTimestamp=now, so all entries
	// appear recently accessed and nothing gets prematurely evicted.
	"journal-1",

	// user-id.txt is a persistent random UUID identifying the Gradle user, written
	// once and never changed. Bundling it overwrites every CI worker's identity
	// with the bundle creator's UUID. Gradle generates a new one if missing.
	"user-id.txt",
}

// isHexHash returns true if s is a non-empty string of hex characters (0-9, a-f).
func isHexHash(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			return false
		}
	}
	return true
}

// DeltaExclusions are additional file/directory names excluded only from delta
// bundles. These files are already present in the base bundle and get rewritten
// every build (Gradle's embedded BTree DB flushes on close even for read-only
// access), so the delta copy adds negligible value at significant size cost.
var DeltaExclusions = []string{
	// fileHashes/ is an indexed cache mapping absolute file paths to (hash, length,
	// mtime) tuples, letting Gradle skip re-hashing unchanged files. The base
	// bundle already provides the bulk of entries, and the DB is rewritten on every
	// build close even for read-only access (BTree compaction), so it always
	// appears "new". The incremental entries from a single build aren't worth the
	// transfer cost.
	"fileHashes",

	// module-metadata.bin is the dependency resolution metadata cache, mapping
	// (repositoryId, moduleComponentId) to parsed POM/module metadata. Portable
	// across machines but rewritten on every build due to DB compaction. The base
	// bundle already has it and a single build rarely adds new dependencies.
	"module-metadata.bin",
}

// wrapperZipExclusion excludes the downloaded Gradle distribution zip from
// wrapper/dists/. After extraction Gradle only needs the unpacked distribution
// directory; the zip is kept for offline re-extraction but is never read during
// normal builds. It's typically 100–150 MB and easily re-downloaded if needed.
const wrapperZipExclusion = "wrapper/dists/*/*/*.zip"

// IsExcludedCache reports whether a file or directory name matches any cache exclusion pattern.
func IsExcludedCache(name string) bool {
	return matchesAny(name, CacheExclusions)
}

// IsDeltaExcluded reports whether a name matches delta-only exclusion patterns.
func IsDeltaExcluded(name string) bool {
	return matchesAny(name, DeltaExclusions)
}

// collectNewFilesSimple walks dir for regular files with mtime after since,
// returning paths relative to baseDir.
func collectNewFilesSimple(dir, baseDir string, since time.Time) []string {
	var files []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(since) {
			rel, _ := filepath.Rel(baseDir, path)
			files = append(files, rel)
		}
		return nil
	})
	return files
}

// AtomicCacheParents lists directory names whose descendants include
// hash-keyed workspace directories that must be captured atomically.
// The walker recurses through intermediate subdirectories until it finds
// children whose names look like hex hashes, then treats those as atomic
// units: if ANY file inside has mtime > marker, ALL files are included.
//
// These directories contain hash-keyed subdirectories where Gradle expects
// all files to be present together. Some are true immutable workspaces
// (transforms, groovy-dsl, kotlin-dsl, dependencies-accessors) created via
// atomic directory renames. Others are persistent caches with receipt-based
// completion markers (jars-9). In both cases, a partial directory (e.g.
// metadata.bin without output files, or a .receipt without the transformed
// classes) causes Gradle to crash.
//
// Mtime skew across delta cycles can produce partial directories: after
// restoring base + delta, the base provides output files (old mtime) while
// the delta overwrites metadata/receipt files (new mtime). A per-file mtime
// check captures only the newer files, producing a partial directory in the
// next delta. When applied to a base that lacks the hash, Gradle crashes.
var AtomicCacheParents = map[string]bool{
	"transforms":             true,
	"groovy-dsl":             true,
	"kotlin-dsl":             true,
	"dependencies-accessors": true,
	"jars-9":                 true,
}

// CollectNewFiles walks realCaches in parallel and returns paths of regular files
// with mtime strictly after since. For directories listed in AtomicCacheParents,
// if any file in a child hash directory is newer than since, all files in that
// directory are included to prevent partial restores.
func CollectNewFiles(realCaches string, since time.Time, gradleHome string) ([]string, error) {
	workers := min(8, runtime.GOMAXPROCS(0))
	sem := make(chan struct{}, workers)

	var mu sync.Mutex
	var allFiles []string
	var firstErr error
	var wg sync.WaitGroup

	// collectAll recursively collects ALL regular files under dir, regardless of mtime.
	collectAll := func(dir, rel string) []string {
		var files []string
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if IsExcludedCache(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() && !IsExcludedCache(d.Name()) {
				childRel, _ := filepath.Rel(dir, path)
				files = append(files, filepath.Join("caches", rel, childRel))
			}
			return nil
		})
		return files
	}

	// walkWorkspaceParent handles directories like transforms/ whose children
	// are atomic workspace directories. It auto-detects whether children are
	// hash-keyed workspaces (hex names) or intermediate directories (like
	// kotlin-dsl/scripts/). For intermediates, it recurses; for hash dirs,
	// it checks if any file is new and captures all files atomically.
	var walkWorkspaceParent func(dir, rel string)
	walkWorkspaceParent = func(dir, rel string) {
		defer wg.Done()

		entries, err := os.ReadDir(dir)
		<-sem

		if err != nil {
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
			return
		}

		for _, entry := range entries {
			if !entry.IsDir() || IsExcludedCache(entry.Name()) {
				continue
			}
			childDir := filepath.Join(dir, entry.Name())
			childRel := rel + "/" + entry.Name()

			if !isHexHash(entry.Name()) {
				// Intermediate directory (e.g. kotlin-dsl/scripts/) — recurse.
				sem <- struct{}{}
				wg.Add(1)
				go walkWorkspaceParent(childDir, childRel)
				continue
			}

			hasNew := false
			_ = filepath.WalkDir(childDir, func(_ string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if fi, e := d.Info(); e == nil && fi.ModTime().After(since) {
					hasNew = true
					return filepath.SkipAll
				}
				return nil
			})

			if hasNew {
				files := collectAll(childDir, childRel)
				// Skip workspace stubs that contain only metadata files
				// (metadata.bin, results.bin, *.receipt) with no actual
				// build output. These stubs provide no caching value and
				// can create broken workspace dirs when applied to a base
				// that lacks the workspace hash.
				hasOutput := false
				for _, f := range files {
					base := filepath.Base(f)
					if base != "metadata.bin" && base != "results.bin" &&
						filepath.Ext(base) != ".receipt" {
						hasOutput = true
						break
					}
				}
				if hasOutput {
					mu.Lock()
					allFiles = append(allFiles, files...)
					mu.Unlock()
				}
			}
		}
	}

	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		defer wg.Done()

		entries, err := os.ReadDir(dir)
		<-sem

		if err != nil {
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
			return
		}

		var localFiles []string
		for _, entry := range entries {
			name := entry.Name()
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			if entry.IsDir() {
				if IsExcludedCache(name) || IsDeltaExcluded(name) {
					continue
				}
				if AtomicCacheParents[name] {
					sem <- struct{}{}
					wg.Add(1)
					go walkWorkspaceParent(filepath.Join(dir, name), childRel)
				} else {
					sem <- struct{}{}
					wg.Add(1)
					go walk(filepath.Join(dir, name), childRel)
				}
			} else if entry.Type().IsRegular() {
				if IsExcludedCache(name) || IsDeltaExcluded(name) {
					continue
				}
				if fi, err := entry.Info(); err == nil && fi.ModTime().After(since) {
					localFiles = append(localFiles, filepath.Join("caches", childRel))
				}
			}
		}
		if len(localFiles) > 0 {
			mu.Lock()
			allFiles = append(allFiles, localFiles...)
			mu.Unlock()
		}
	}

	sem <- struct{}{}
	wg.Add(1)
	go walk(realCaches, "")
	wg.Wait()
	return allFiles, firstErr
}

// ProjectDirSources returns tarSource entries for project-specific dirs.
func ProjectDirSources(projectDir string, includedBuilds []string) []TarSource {
	var sources []TarSource

	gradleDir := filepath.Join(projectDir, ".gradle")
	if _, err := os.Stat(filepath.Join(gradleDir, "configuration-cache")); err == nil {
		sources = append(sources, TarSource{BaseDir: gradleDir, Path: "./configuration-cache"})
	}

	for _, rel := range ConventionBuildDirs(projectDir, includedBuilds) {
		sources = append(sources, TarSource{BaseDir: projectDir, Path: "./" + rel})
	}

	return sources
}

func validateProjectDir(projectDir string) error {
	if projectDir == "" {
		return errors.New("project directory is required")
	}
	info, err := os.Stat(projectDir)
	if err != nil {
		return errors.Errorf("project directory %q does not exist: %w", projectDir, err)
	}
	if !info.IsDir() {
		return errors.Errorf("project directory %q is not a directory", projectDir)
	}
	return nil
}

// ConventionBuildDirs returns the relative paths of included build output directories.
func ConventionBuildDirs(root string, includedBuilds []string) []string {
	var result []string
	for _, entry := range includedBuilds {
		if strings.HasSuffix(entry, "/*") {
			parent := strings.TrimSuffix(entry, "/*")
			entries, err := os.ReadDir(filepath.Join(root, parent))
			if err != nil {
				continue
			}
			for _, sub := range entries {
				if !sub.IsDir() {
					continue
				}
				rel := parent + "/" + sub.Name() + "/build"
				if info, err := os.Stat(filepath.Join(root, rel)); err == nil && info.IsDir() {
					result = append(result, rel)
				}
			}
		} else {
			rel := entry + "/build"
			if info, err := os.Stat(filepath.Join(root, rel)); err == nil && info.IsDir() {
				result = append(result, rel)
			}
		}
	}
	return result
}
