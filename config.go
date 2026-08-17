
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// parseConfigFile reads a simple "key=value" (or bare "key" for booleans)
// per-line config file into mo, using the same option set as "-o". Blank
// lines and lines starting with "#" are ignored. This is meant mainly for
// credentials (username/password/cookie) so they don't have to be passed
// on the command line or in fstab, where they'd be visible to anyone who
// can run "ps" or read /etc/fstab.
func parseConfigFile(mo *MountOptions, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

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
			return fmt.Errorf("%s line %d: %v", path, lineNo, err)
		}
	}
	return scanner.Err()
}
