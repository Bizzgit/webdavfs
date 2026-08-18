# webdavfs

## A FUSE filesystem for WEBDAV shares.

Most filesystem drivers for Webdav shares act somewhat like a mirror;
if a file is read it's first downloaded then cached in its entirety
on a local drive, then read from there. Writing files is similar or
even worse- a partial update to a file might involve downloading it first,
modifying it, then uploading it again. In many cases that is not optimal.

This filesystem driver behaves like a network filesystem. It doesn't
cache anything locally, it just sends out partial reads/writes over the
network.

For that to work, you need partial write support- and unfortunately,
there is no standard for that. See
https://blog.sphere.chronosempire.org.uk/2012/11/21/webdav-and-the-http-patch-nightmare

However, there is support in Apache (the webserver, using mod_dav) and
[SabreDav](SABREDAV-partialupdate.md) (a php webserver server library,
used by e.g. NextCloud) for partial writes. So we detect if it's Apache or
SabreDav we're talking to and then use their specific methods to partially
update files.

If no support for partial writes is detected, mount.webdavfs will
print a warning and mount the filesystem read-only. In that case you can
also use the `rwdirops` mount option, this will make metadata writable
(i.e. you can use rm / mv / mkdir / rmdir) but you still won't be able
to write to files.

But if you only need to read files it's still way faster than davfs2 :)

## What is working

Basic filesystem operations.

- files: create/delete/read/write/truncate/seek
- directories: mkdir rmdir readdir
- query filesystem size (df / vfsstat)
- a cancelled operation (client killed, kernel interrupt) stops waiting
  immediately instead of hanging until the underlying HTTP request or
  an internal lock wait times out on its own

## What is not yet working

- locking

## What will not ever work

- change permissions (all files are 644, all dirs are 755)
- change user/group
- devices / fifos / chardev / blockdev etc
- truncate(2) / ftruncate(2) for lengths between 1 .. currentfilesize - 1

This is basically because these are mostly just missing properties
from webdav.

## What platforms does it run on

- Linux
- FreeBSD (untested, but should work)
- It might work on macos if you use [osxfuse 3](https://github.com/osxfuse/osxfuse/releases/tag/osxfuse-3.11.2). Then again it might not. This is completely unsupported. See also [this issue](https://github.com/miquels/webdavfs/issues/11).

## How to install and use.

First you need to install golang, git, fuse, and set up your environment.
For Debian:

```
$ sudo -s
Password:
# apt-get install golang git fuse
# exit
```

Now with go and git installed, get a copy of this github repository:

```
$ git clone https://github.com/miquels/webdavfs
$ cd webdavfs
```

You're now ready to build the binary:

```
$ go get
$ go build
```

And install it:

```
$ sudo -s
Password:
# cp webdavfs /sbin/mount.webdavfs
```

Using it is simple as:
```
# mount -t webdavfs -ousername=you,password=pass https://webdav.where.ever/subdir /mnt
```

## Command line options

| Option | Description |
| --- | --- |
| -f | don't actually mount |
| -D | daemonize | default when called as mount.* |
| -T opts | trace options: fuse,webdav,httpreq,httphdr |
| -F file | trace file. file will be reopened when renamed, tracing will stop when file is removed |
| -o opts | mount options |
| -C file | config file: same option syntax as -o, one option per line, `#` for comments. Options given via -o override the same option from the config file. Mainly meant for credentials, so they don't need to be on the command line or in fstab. Not reachable from /etc/fstab or systemd .mount units - use the `configfile=` mount option below for that. The file's permissions must not be group/world readable (webdavfs refuses to load it otherwise), same posture as an SSH private key. |

## Mount options

| Option | Description |
| --- | --- |
| allow_root		| If mounted as normal user, allow access by root |
| allow_other		| Allow access by others than the mount owner. This |
|			| also sets "default_permisions" |
| default_permissions	| As per fuse documentation |
| no_default_permissions | Don't set "default_permissions" with "allow_other" |
| ro			| Read only |
| rwdirops		| Read-write for directory operations, but no file-writing (no PUT) |
| rw			| Read-write (default) |
| uid			| User ID for filesystem |
| gid			| Group ID for filesystem. |
| mode			| Mode for files/directories on the filesystem (600, 666, etc). |
|			| Files will never have the executable bit on, directories always. |
| cookie		| Authorization Cookie (Useful for O365 Sharepoint/OneDrive for Business) |
| password		| Password of webdav user |
| username		| Username of webdav user |
| async_read		| As per fuse documentation |
| nonempty		| As per fuse documentation |
| maxconns              | Maximum number of parallel connections to the webdav
|                       | server (default 8)
| maxidleconns          | Maximum number of idle connections (default 8)
| sabredav_partialupdate | Use the sabredav partialupdate protocol even when
|                        | the remote server doesn't advertise support (DANGEROUS)
| tlsskipverify          | Don't verify the remote server's TLS certificate. Needed
|                        | for https:// URLs with a self-signed certificate (DANGEROUS:
|                        | disables protection against man-in-the-middle attacks - note this
|                        | also means any credentials sent over that connection, including
|                        | ones loaded via `configfile=`/-C, are exposed to that same MITM;
|                        | using both together protects credentials from local exposure only,
|                        | not from the network)
| configfile=file        | Load more options (same syntax as -C, see above) from file, applied
|                        | in place at this point in the option list. Unlike -C, this **is**
|                        | reachable from /etc/fstab and systemd .mount units, since those only
|                        | ever pass through the mount options, not extra command-line flags -
|                        | this is the one to use to keep credentials out of fstab/the unit file.
|                        | Same permission restriction as -C applies. A file referencing itself
|                        | (directly or via a chain of configfile= options) fails with a clear
|                        | error rather than recursing.

If the webdavfs program is called via `mount -t webdavfs` or as `mount.webdav`,
it will fork, re-exec and run in the background. In that case it will remove
the username and password options from the command line, and communicate them
via the environment instead.

The environment options for username and password are WEBDAV_USERNAME and
WEBDAV_PASSWORD, respectively.

Credentials can also be loaded from a config file - see `-C` and
`configfile=` above. Note that `-T httphdr` tracing prints full request
headers, including `Authorization` and `Cookie`, to the trace file - if
you're keeping credentials out of `ps`/fstab via a config file, the
trace file is an equally sensitive place for them to leak from.

## TODO

- `bazil.org/fuse` (the module this project is built on, `bazil.org/fuse/fs`
  specifically) has had no commits since Dec 2023 and is effectively
  unmaintained - no fixes to expect if a future kernel FUSE ABI change or
  security issue ever affects it. Not an active problem today, so there's
  no reason to migrate pre-emptively; treat this as a someday/maybe, not a
  standing task. If it ever does break: [hanwen/go-fuse](https://github.com/hanwen/go-fuse)
  (`v2`'s `fs` package) is the option to reach for - actively maintained,
  the closest abstraction match to the `Node`/`Handle` model this fork
  already uses, and proven in comparable read-heavy remote-storage
  workloads (JuiceFS, containerd/stargz-snapshotter). This would still be
  a genuine rewrite of the FUSE-facing layer in `fuse.go`/`node.go`, not a
  mechanical port - call signatures, error types, and the inode/embedding
  lifecycle all differ. `jacobsa/fuse` and `bazil.org/fuse`'s own raw
  (non-`fs`) interface are both lower-level, op-callback wire protocols;
  moving to either would mean rebuilding request dispatch and inode/lookup
  bookkeeping from scratch (things `fs.Serve` currently handles), for no
  benefit specific to this kind of workload - not worth it just to get off
  an unmaintained dependency.

## Unix filesystem extensions for webdav.

Not ever going to happen, but if you wanted a more unix-like
experience and better performance, here are a few ideas. Genuine,
still-unaddressed protocol gaps:

- Content-Type: for unix pipes / chardevs / etc - no RFC or de facto
  convention exists for representing special file types over WebDAV.
- contentsize property (read-write) - `getcontentlength` (RFC 4918 §15.4)
  is defined as the server-computed length of the GET response body; a
  client-writable size decoupled from actual bytes would break basic HTTP
  semantics, not just WebDAV convention, so this was likely never truly
  implementable as originally imagined.
- DELETE Depth 0 for collections (no delete if non-empty) - RFC 4918
  §9.6.1 mandates DELETE on a collection always acts as Depth: infinity;
  there's no protocol concept of a depth-limited "rmdir if empty" delete.

Partially addressed since this list was written, worth knowing about
even though not worth building against:

- inodenumber property - no RFC standard, but Nextcloud/ownCloud expose a
  vendor-specific `oc:fileid` property that serves a similar purpose
  against those specific servers.
- unix properties like uid/gid/mode - RFC 3744 (WebDAV ACL) defines
  `DAV:owner`/`DAV:group` for Unix-privilege-model repositories, but
  principals are URIs (not numeric uid/gid) and there's no `mode`
  equivalent - RFC 3744 replaces POSIX mode with a heavier per-principal
  ACL model instead, with inconsistent server adoption.

Now solved by a later RFC, no longer a gap:

- return updated PROPSTAT information after operations like PUT / DELETE
  / MKCOL / MOVE - RFC 8144 (2017) standardizes `Prefer: return=minimal` /
  `return=representation` for WebDAV, letting a client request updated
  state back in the same round trip.

