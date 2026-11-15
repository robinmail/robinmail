#!/bin/sh
# vdirsyncer against robinmail: discover the calendars and address books
# of a user, sync them to directories, then sync both ways.
#
#   vdirsyncer.sh USER PASSWORD init        a new config and directory, discover, sync
#   vdirsyncer.sh USER PASSWORD sync        sync again
#   vdirsyncer.sh USER PASSWORD ls          the files it has, one per line
#
# The directories are /tmp/vds/<user>/{cal,card}/<collection>/.
set -eu
user=$1 password=$2 what=$3
dir=/tmp/vds/$user
conf=$dir/config
case "$what" in
init)
	rm -rf "$dir"
	mkdir -p "$dir/status" "$dir/cal" "$dir/card"
	cat > "$conf" <<CONF
[general]
status_path = "$dir/status/"

[pair cal]
a = "cal_local"
b = "cal_remote"
collections = ["from b"]
conflict_resolution = "b wins"

[storage cal_local]
type = "filesystem"
path = "$dir/cal/"
fileext = ".ics"

[storage cal_remote]
type = "caldav"
url = "https://mx.robin.test/dav/"
username = "$user"
password = "$password"
verify = "/lab/secrets/ca.crt"

[pair card]
a = "card_local"
b = "card_remote"
collections = ["from b"]
conflict_resolution = "b wins"

[storage card_local]
type = "filesystem"
path = "$dir/card/"
fileext = ".vcf"

[storage card_remote]
type = "carddav"
url = "https://mx.robin.test/dav/"
username = "$user"
password = "$password"
verify = "/lab/secrets/ca.crt"
CONF
	yes | vdirsyncer -c "$conf" discover
	vdirsyncer -c "$conf" sync
	;;
sync) vdirsyncer -c "$conf" sync ;;
ls) cd "$dir" && find cal card -type f | sort ;;
*) echo "usage: vdirsyncer.sh USER PASSWORD init|sync|ls" >&2; exit 2 ;;
esac
