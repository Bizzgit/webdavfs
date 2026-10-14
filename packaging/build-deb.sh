#!/bin/sh
# Build a Debian package of mount.webdavfs for the architecture of this machine.
#
#   packaging/build-deb.sh [output-dir]      (default: ./dist)
#
# Needs: go, dpkg-deb, git. The version is VERSION from main.go plus the
# commit date and hash, so every build from a different commit is a different,
# increasing package version; a build from an uncommitted tree gets "-dirty".
# Override with VERSION=... if you need a specific one.
set -eu

cd "$(dirname "$0")/.."
out=${1:-dist}
arch=$(dpkg --print-architecture)

if [ -z "${VERSION:-}" ]; then
	base=$(sed -n 's/^const VERSION = "\(.*\)"/\1/p' main.go)
	[ -n "$base" ] || { echo "cannot find VERSION in main.go" >&2; exit 1; }
	git_="git -c safe.directory=$PWD"
	date=$($git_ show -s --format=%cd --date=format:%Y%m%d HEAD)
	rev=$($git_ rev-parse --short HEAD)
	VERSION="$base+git$date.$rev"
	[ -z "$($git_ status --porcelain)" ] || VERSION="$VERSION-dirty"
fi

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
chmod 0755 "$stage"

install -d "$stage/usr/sbin" "$stage/etc/logrotate.d" "$stage/usr/share/doc/webdavfs" "$stage/DEBIAN"
go build -trimpath -o "$stage/usr/sbin/mount.webdavfs" .
chmod 0755 "$stage/usr/sbin/mount.webdavfs"
install -m 0644 packaging/logrotate "$stage/etc/logrotate.d/webdavfs"
install -m 0644 LICENSE.md "$stage/usr/share/doc/webdavfs/copyright"
install -m 0644 README.md "$stage/usr/share/doc/webdavfs/README.md"
echo /etc/logrotate.d/webdavfs > "$stage/DEBIAN/conffiles"

size=$(du -sk "$stage" | cut -f1)
sed -e "s/@VERSION@/$VERSION/" -e "s/@ARCH@/$arch/" -e "s/@SIZE@/$size/" \
	packaging/control.in > "$stage/DEBIAN/control"

mkdir -p "$out"
deb="$out/webdavfs_${VERSION}_${arch}.deb"
dpkg-deb --root-owner-group --build "$stage" "$deb" >/dev/null
echo "$deb"
