package gradlecache

import "time"

// Layout describes the tool-specific parts of a bundle: what goes into it,
// where it is extracted to, and which files go into a branch delta.
type Layout interface {
	// StateDir holds the restore marker and the base commit file. If it is
	// empty before a restore, extraction does not check for existing files.
	StateDir() string
	// Sources returns the directories to archive in a base bundle.
	Sources() ([]TarSource, error)
	// Exclusions returns the tar --exclude patterns for a base bundle.
	Exclusions() []string
	// ExtractRules maps bundle entry prefixes to destination directories.
	ExtractRules() []extractRule
	// DefaultDir receives the entries that match no extract rule.
	DefaultDir() string
	// DeltaSources returns the files to put in a branch delta: the files that
	// changed after since.
	DeltaSources(since time.Time) ([]DeltaSource, error)
	// BeforeSave runs before a base save (delta is false) or a delta save
	// (delta is true) collects its files.
	BeforeSave(delta bool) error
	// AfterRestore runs after a restore extracted the base and the delta.
	AfterRestore() error
}
