# The agent image: the static binary and nothing else. It is for
# `stagehand-deploy-agent --check-config`, tests and the contract harness. It is
# NOT a deploy runtime: it has no r10k, git or ssh, so it cannot deploy anything.
#
# The build context is a directory holding one binary per architecture,
# <context>/amd64/stagehand-deploy-agent and <context>/arm64/stagehand-deploy-agent,
# which .github/workflows/release.yml (and hack/package-smoke.sh) unpack from the
# goreleaser archives. The image is built after the release verifies, not by
# goreleaser.
FROM scratch
ARG TARGETARCH
LABEL org.opencontainers.image.source="https://github.com/puppet-stagehand/stagehand-deploy-agent" \
      org.opencontainers.image.description="Stagehand deploy agent binary only, for --check-config, tests and the contract harness. Not a deploy runtime." \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --chmod=0755 ${TARGETARCH}/stagehand-deploy-agent /usr/bin/stagehand-deploy-agent
USER 65532:65532
ENTRYPOINT ["/usr/bin/stagehand-deploy-agent"]
CMD ["--help"]
