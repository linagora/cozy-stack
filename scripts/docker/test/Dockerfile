# Runs the Go tests for docker-compose.test.yml, with the tools that
# .github/workflows/go-tests.yml installs: Ghostscript and ImageMagick, without
# its security policy, for the thumbnails and the avatars
FROM golang:1.26-bookworm

RUN apt-get update \
  && apt-get install -y --no-install-recommends busybox ghostscript imagemagick \
  && rm -rf /var/lib/apt/lists/* \
  && rm /etc/ImageMagick-6/policy.xml

# The configuration tests set up a syslog hook, which needs a daemon on /dev/log
ENTRYPOINT ["/bin/sh", "-c", "busybox syslogd -O /dev/null && exec \"$@\"", "sh"]
