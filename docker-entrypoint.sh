#!/bin/sh
set -e

# Started as root, which is what the image does unless told otherwise, /data is
# handed to tm before the program starts. /data is a host bind mount, and a bind
# mount keeps the ownership of the host directory: docker creates it as root
# when it is missing, and an installation that ran before the image moved off
# root left its files owned by root. Only what is not already tm's is changed,
# so a start after the first touches nothing. The permission bits are left as
# they are, since the program already makes keys/ and logs/ 0700 and the files
# it writes 0600.
#
# Started as somebody else, by a user: in docker-compose.yaml, there is no root
# to change ownership with and none to drop, so the command runs as it is.
if [ "$(id -u)" = "0" ]; then
	mkdir -p /data
	find /data \( ! -user 10888 -o ! -group 10888 \) -exec chown -h 10888:10888 {} +
	exec su-exec tm "$@"
fi

exec "$@"
