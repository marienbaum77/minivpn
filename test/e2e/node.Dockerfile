# A fresh node as the hoster hands it over: Ubuntu with curl, python3 and a
# root password for SSH. The app installs everything else over SSH. Built
# before the test clock starts, so a slow apt mirror does not eat the
# readiness timeout.
# PYTHON= (empty) builds an image without it, to test that setup installs it.
FROM ubuntu:24.04
ARG PYTHON=python3
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends curl ca-certificates openssh-server $PYTHON \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /run/sshd \
 && echo 'root:e2e-root' | chpasswd \
 && printf 'PermitRootLogin yes\nPasswordAuthentication yes\n' >/etc/ssh/sshd_config.d/e2e.conf
CMD ["/usr/sbin/sshd", "-D", "-e"]
