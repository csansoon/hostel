#!/bin/sh
# Host keys are made at the first start of each container, so each room has its
# own key and keeps it across "docker stop" and "docker start".
set -e
ssh-keygen -A
if [ -n "$HOSTEL_AUTHORIZED_KEY" ] && [ ! -f /home/hostel/.ssh/authorized_keys ]; then
	install -d -m 700 -o hostel -g hostel /home/hostel/.ssh
	printf '%s\n' "$HOSTEL_AUTHORIZED_KEY" > /home/hostel/.ssh/authorized_keys
	chown hostel:hostel /home/hostel/.ssh/authorized_keys
	chmod 600 /home/hostel/.ssh/authorized_keys
fi
exec /usr/sbin/sshd -D -e
