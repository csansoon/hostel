# Test room: sshd and git only. The server uploads the helper and mise, so the
# tests cover the upload path (design, section 20).
FROM debian:13-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends openssh-server git ca-certificates \
	&& rm -rf /var/lib/apt/lists/* \
	&& rm -f /etc/ssh/ssh_host_* \
	&& useradd --create-home --shell /bin/bash hostel \
	&& usermod --password '*' hostel \
	&& mkdir -p /run/sshd
COPY sshd.conf /etc/ssh/sshd_config.d/hostel.conf
COPY entrypoint.sh /entrypoint.sh
EXPOSE 22
ENTRYPOINT ["/bin/sh", "/entrypoint.sh"]
