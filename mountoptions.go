
package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// optDuration is a duration option that remembers whether it was given, so
// that an explicit 0 (for example statcache=0, "never cache") can be told
// apart from "not set, use the default".
type optDuration struct {
	D	time.Duration
	Set	bool
}

// optUInt is the same idea for a count: retries=0 turns retrying off.
type optUInt struct {
	N	uint32
	Set	bool
}

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
	TraceFile		string
	Retries			optUInt
	RetryDelay		optDuration
	IdleTimeout		optDuration
	Timeout			optDuration
	StatCache		optDuration
	AttrTimeout		optDuration
	ReadAhead		optUInt
	ReadAheadBlocks		optUInt
	ReadAheadMax		optUInt
	HTTP2			bool
}

// parseSize reads a size in bytes with an optional K, M or G suffix ("512K",
// "4M"). 0 is allowed and means off. max is the largest value accepted.
func parseSize(v string, name string, loc *optUInt, max uint64) (err error) {
	mult := uint64(1)
	switch {
	case strings.HasSuffix(v, "K") || strings.HasSuffix(v, "k"):
		mult, v = 1 << 10, v[:len(v)-1]
	case strings.HasSuffix(v, "M") || strings.HasSuffix(v, "m"):
		mult, v = 1 << 20, v[:len(v)-1]
	case strings.HasSuffix(v, "G") || strings.HasSuffix(v, "g"):
		mult, v = 1 << 30, v[:len(v)-1]
	}
	n, perr := strconv.ParseUint(v, 10, 32)
	if perr != nil || n*mult > max {
		return fmt.Errorf("%s: not a size up to %d bytes (use e.g. 1M, 512K or plain bytes)", name, max)
	}
	*loc = optUInt{N: uint32(n * mult), Set: true}
	return
}

// parseDuration reads a Go duration ("500ms", "30s", "5m"). A bare number
// is taken as seconds, which is what people expect in an fstab line.
func parseDuration(v string, name string, loc *optDuration) (err error) {
	if n, perr := strconv.ParseUint(v, 10, 32); perr == nil {
		*loc = optDuration{D: time.Duration(n) * time.Second, Set: true}
		return
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return errors.New(name + ": not a duration (use e.g. 500ms, 30s, 5m or plain seconds)")
	}
	if d < 0 {
		return errors.New(name + ": must not be negative")
	}
	*loc = optDuration{D: d, Set: true}
	return
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
	case "readahead":
		err = parseSize(v, "readahead", &mo.ReadAhead, 64 << 20)
	case "readaheadblocks":
		err = parseUInt32(v, 10, "readaheadblocks", &mo.ReadAheadBlocks.N)
		if err == nil && mo.ReadAheadBlocks.N > 16 {
			err = errors.New("readaheadblocks: at most 16")
		}
		mo.ReadAheadBlocks.Set = err == nil
	case "readaheadmax":
		// Total memory for read buffers over all open files.
		err = parseSize(v, "readaheadmax", &mo.ReadAheadMax, 4 << 30 - 1)
	case "http2":
		mo.HTTP2 = true
	case "retries":
		err = parseUInt32(v, 10, "retries", &mo.Retries.N)
		if err == nil && mo.Retries.N > maxRetries {
			err = fmt.Errorf("retries: at most %d", maxRetries)
		}
		mo.Retries.Set = err == nil
	case "retrydelay":
		err = parseDuration(v, "retrydelay", &mo.RetryDelay)
		if err == nil && mo.RetryDelay.D > maxRetryDelay {
			err = fmt.Errorf("retrydelay: at most %v", maxRetryDelay)
		}
	case "idletimeout":
		err = parseDuration(v, "idletimeout", &mo.IdleTimeout)
	case "timeout":
		err = parseDuration(v, "timeout", &mo.Timeout)
		if err == nil && mo.Timeout.D < time.Second {
			// 0 would mean no time limit at all: a stalled server
			// would hold a connection and a reader for ever.
			err = errors.New("timeout: at least 1s")
		}
	case "statcache":
		err = parseDuration(v, "statcache", &mo.StatCache)
	case "attrtimeout":
		err = parseDuration(v, "attrtimeout", &mo.AttrTimeout)
	case "sabredav_partialupdate":
		mo.SabreDavPartialUpdate = true
	case "tlsskipverify":
		mo.TLSSkipVerify = true
	case "tracefile":
		// Mount-option equivalent of -F, for the same reason as
		// configfile= below: /etc/fstab and systemd .mount units can
		// only pass mount options, never command-line flags. See
		// traceOpts() in trace.go for what it does without -T.
		if v == "" {
			err = errors.New("tracefile: needs a file name")
		}
		mo.TraceFile = v
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
	case "_netdev", "nofail", "noauto", "auto":
		// Not real webdavfs options - mount(8) and systemd's fstab
		// generator both pass these straight through in the Options=
		// string instead of stripping them (verified: a
		// systemd-fstab-generator run against an fstab line with
		// _netdev,nofail reproduces the literal option in the
		// generated .mount unit's Options=). They only mean something
		// to systemd/mount(8) itself (network dependency ordering,
		// non-blocking boot, auto/no-auto at mount -a time) - recognize
		// and ignore them here so an /etc/fstab entry using them
		// (the normal way to configure a network filesystem mount)
		// doesn't fail with "unknown option".
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
			// -s/sloppy is meant to tolerate unknown option *names*,
			// not to hide a broken configfile=: swallowing that would
			// let the mount proceed with empty/missing credentials
			// instead of failing loudly (fail-open to anonymous auth).
			var cfErr *ConfigFileError
			if sloppy && !errors.As(err, &cfErr) {
				err = nil
				continue
			}
			return
		}
	}
	return
}
