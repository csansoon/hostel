# Test machine: sshd, git, curl, and sudo (as the Namespace instance machine needs),
# and netcat for TCP servers in tests. No helper and no mise: the server uploads
# them, so the tests cover the upload path (design, section 20).
FROM debian:13-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends openssh-server git ca-certificates curl netcat-openbsd sudo \
	&& rm -rf /var/lib/apt/lists/* \
	&& rm -f /etc/ssh/ssh_host_* \
	&& useradd --create-home --shell /bin/bash hostel \
	&& usermod --password '*' hostel \
	&& mkdir -p /run/sshd
COPY sshd.conf /etc/ssh/sshd_config.d/hostel.conf
COPY entrypoint.sh /entrypoint.sh
EXPOSE 22
ENTRYPOINT ["/bin/sh", "/entrypoint.sh"]
