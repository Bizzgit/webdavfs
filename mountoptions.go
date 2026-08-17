
package main

import (
	"errors"
	"strconv"
	"strings"
)

type MountOptions struct {
	AllowRoot		bool
	AllowOther		bool
	DefaultPermissions	bool
	NoDefaultPermissions	bool
	ReadOnly		bool
	ReadWrite		bool
	ReadWriteDirOps		bool
	Uid			uint32
	Gid			uint32
	Mode			uint32
	Cookie			string
	Password		string
	Username		string
	AsyncRead		bool
	NonEmpty		bool
	MaxConns		uint32
	MaxIdleConns		uint32
	SabreDavPartialUpdate	bool
	TLSSkipVerify		bool
}

func parseUInt32(v string, base int, name string, loc *uint32) (err error) {
	n, err := strconv.ParseUint(v , base, 32)
	if err == nil {
		*loc = uint32(n)
	}
	return
}

// applyOption sets a single key (optionally "key=value") on mo. Shared by
// the -o comma-separated option parser and the config-file line parser, so
// the two never drift out of sync with each other.
func applyOption(mo *MountOptions, key, v string) (err error) {
	switch key {
	case "allow_root":
		mo.AllowRoot = true
	case "allow_other":
		mo.AllowOther = true
	case "default_permissions":
		mo.DefaultPermissions = true
	case "no_default_permissions":
		mo.NoDefaultPermissions = true
	case "ro":
		mo.ReadOnly = true
	case "rw":
		mo.ReadWrite = true
	case "rwdirops":
		mo.ReadWriteDirOps = true
	case "uid":
		err = parseUInt32(v, 10, "uid", &mo.Uid)
	case "gid":
		err = parseUInt32(v, 10, "gid", &mo.Gid)
	case "mode":
		err = parseUInt32(v, 8, "mode", &mo.Mode)
	case "cookie":
		mo.Cookie = v
	case "password":
		mo.Password = v
	case "username":
		mo.Username = v
	case "async_read":
		mo.AsyncRead = true
	case "nonempty":
		mo.NonEmpty = true
	case "maxconns":
		err = parseUInt32(v, 10, "maxconns", &mo.MaxConns)
	case "maxidleconns":
		err = parseUInt32(v, 10, "maxidleconns", &mo.MaxIdleConns)
	case "sabredav_partialupdate":
		mo.SabreDavPartialUpdate = true
	case "tlsskipverify":
		mo.TLSSkipVerify = true
	case "configfile":
		// Applied immediately, in place, as if the config file's lines
		// had been spliced into the option list right here. This is
		// the mount-option equivalent of -C, and unlike -C it *is*
		// reachable from /etc/fstab and systemd .mount units, which
		// only ever pass through the "-o" Options= string - never
		// arbitrary command-line flags. Put configfile=... first in
		// the option list if later options should be able to override
		// values loaded from it.
		err = parseConfigFile(mo, v)
	default:
		err = errors.New(key + ": unknown option")
	}
	return
}

// parseMountOptions parses a comma-separated "-o" style option string and
// merges it into mo. mo is not reset first, so this can be called on top
// of options already loaded from a config file - values given here win.
func parseMountOptions(mo *MountOptions, n string, sloppy bool) (err error) {
	if n == "" {
		return
	}

	for _, kv := range strings.Split(n, ",") {
		a := strings.SplitN(kv, "=", 2)
		v := ""
		if len(a) > 1 {
			v = a[1]
		}
		err = applyOption(mo, a[0], v)
		if err != nil {
			if sloppy {
				err = nil
				continue
			}
			return
		}
	}
	return
}
