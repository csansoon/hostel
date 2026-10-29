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
# Slot users (HOSTEL_SLOT_USERS, space-separated): each has its own home that
# other users cannot read, and accepts the server key.
for u in $HOSTEL_SLOT_USERS; do
	if ! id "$u" > /dev/null 2>&1; then
		useradd --create-home --shell /bin/bash "$u"
		usermod --password '*' "$u"
		chmod 700 "/home/$u"
		install -d -m 700 -o "$u" -g "$u" "/home/$u/.ssh"
		printf '%s\n' "$HOSTEL_AUTHORIZED_KEY" > "/home/$u/.ssh/authorized_keys"
		chown "$u:$u" "/home/$u/.ssh/authorized_keys"
		chmod 600 "/home/$u/.ssh/authorized_keys"
	fi
done
# A fake Namespace devbox (test/fakeprovider, devbox mode): SSH sessions get the
# task directory, as on a real devbox.
if [ -n "$NAMESPACE_DEVBOX_TASKS_DIR" ] && ! grep -q NAMESPACE_DEVBOX_TASKS_DIR /etc/environment; then
	install -d -m 755 -o hostel -g hostel "$NAMESPACE_DEVBOX_TASKS_DIR"
	echo "NAMESPACE_DEVBOX_TASKS_DIR=$NAMESPACE_DEVBOX_TASKS_DIR" >> /etc/environment
fi
exec /usr/sbin/sshd -D -e
