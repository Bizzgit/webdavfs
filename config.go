
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// maxConfigFileDepth guards against a configfile= line (directly or via
// a chain of files) referencing itself, which would otherwise recurse
// until the process crashes instead of failing with a clear error.
// Parsing only ever happens single-threaded at mount-time, before any
// FUSE request is served, so a package-level counter is safe here.
const maxConfigFileDepth = 8

var configFileDepth = 0

// ConfigFileError wraps an error encountered while loading a config
// file (via -C or configfile=), so callers can distinguish "this config
// file is broken" from "this option name is unknown" - see
// parseMountOptions, which must never silently swallow the former under
// -s/sloppy: doing so would let a mount proceed with empty/missing
// credentials instead of failing loudly.
type ConfigFileError struct {
	err error
}

func (e *ConfigFileError) Error() string { return e.err.Error() }
func (e *ConfigFileError) Unwrap() error { return e.err }

// parseConfigFile reads a simple "key=value" (or bare "key" for booleans)
// per-line config file into mo, using the same option set as "-o". Blank
// lines and lines starting with "#" are ignored. This is meant mainly for
// credentials (username/password/cookie) so they don't have to be passed
// on the command line or in fstab, where they'd be visible to anyone who
// can run "ps" or read /etc/fstab. Because that's the whole point, a
// config file that's itself readable by group/other defeats it just as
// thoroughly as fstab would - refuse to use one.
func parseConfigFile(mo *MountOptions, path string) error {
	configFileDepth++
	defer func() { configFileDepth-- }()
	if configFileDepth > maxConfigFileDepth {
		return &ConfigFileError{fmt.Errorf("%s: configfile= nested too deeply (possible self-reference)", path)}
	}

	f, err := os.Open(path)
	if err != nil {
		return &ConfigFileError{err}
	}
	defer f.Close()

	fi, statErr := f.Stat()
	if statErr != nil {
		return &ConfigFileError{statErr}
	}
	if fi.Mode().Perm()&0077 != 0 {
		return &ConfigFileError{fmt.Errorf(
			"%s: permissions %#o are too open for a file meant to hold credentials - chmod 600 it (or remove group/world read access)",
			path, fi.Mode().Perm())}
	}

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		a := strings.SplitN(line, "=", 2)
		v := ""
		if len(a) > 1 {
			v = a[1]
		}
		if err := applyOption(mo, strings.TrimSpace(a[0]), v); err != nil {
			return &ConfigFileError{fmt.Errorf("%s line %d: %v", path, lineNo, err)}
		}
	}
	if err := scanner.Err(); err != nil {
		return &ConfigFileError{err}
	}
	return nil
}
