package pigcc

import (
	"maps"
	"slices"
	"strings"
)

// The tags PiG appends to a summary it generated itself. The extension writes the
// same block so the summary reads the same either way, and so the list survives
// the next compaction: PiG seeds a compaction's file operations from the previous
// entry only when that entry did not come from a hook, and a takeover entry
// always does.
const (
	readFilesOpen      = "<read-files>"
	readFilesClose     = "</read-files>"
	modifiedFilesOpen  = "<modified-files>"
	modifiedFilesClose = "</modified-files>"
)

// fileOperationsBlock renders the trailing file list for a summary, mirroring
// compaction.FormatFileOperations byte for byte, and returns the two lists it
// rendered so the compaction entry can record them in its details.
//
// previousSummary carries the lists written by the previous takeover. PiG does
// not seed FileOps from a hook-written entry, so without merging them here each
// compaction would forget every file the earlier ones had seen.
func fileOperationsBlock(ops FileOps, previousSummary string) (block string, readFiles, modifiedFiles []string) {
	// Modified wins over read: a file that was edited is not "read only", which
	// is the same rule ComputeFileLists applies.
	modified := make(map[string]bool, len(ops.Written)+len(ops.Edited))
	read := make(map[string]bool, len(ops.Read))
	for _, path := range ops.Written {
		if path != "" {
			modified[path] = true
		}
	}
	for _, path := range ops.Edited {
		if path != "" {
			modified[path] = true
		}
	}
	for _, path := range ops.Read {
		if path != "" {
			read[path] = true
		}
	}
	for _, path := range taggedList(previousSummary, modifiedFilesOpen, modifiedFilesClose) {
		modified[path] = true
	}
	for _, path := range taggedList(previousSummary, readFilesOpen, readFilesClose) {
		read[path] = true
	}
	for path := range modified {
		delete(read, path)
	}

	readFiles = slices.Sorted(maps.Keys(read))
	modifiedFiles = slices.Sorted(maps.Keys(modified))
	return formatFileOperations(readFiles, modifiedFiles), readFiles, modifiedFiles
}

// taggedList reads one tag pair out of a summary. The block is one path per
// line, as PiG writes it; blank lines and surrounding space are dropped.
func taggedList(summary, open, close string) []string {
	start := strings.Index(summary, open)
	if start < 0 {
		return nil
	}
	start += len(open)
	end := strings.Index(summary[start:], close)
	if end < 0 {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(summary[start:start+end], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

// formatFileOperations is compaction.FormatFileOperations: a blank line, then
// each non-empty tag block, separated by a blank line.
func formatFileOperations(readFiles, modifiedFiles []string) string {
	var sections []string
	if len(readFiles) > 0 {
		sections = append(sections, readFilesOpen+"\n"+strings.Join(readFiles, "\n")+"\n"+readFilesClose)
	}
	if len(modifiedFiles) > 0 {
		sections = append(sections, modifiedFilesOpen+"\n"+strings.Join(modifiedFiles, "\n")+"\n"+modifiedFilesClose)
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(sections, "\n\n")
}
