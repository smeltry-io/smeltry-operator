# Used in CI after `nix build` has produced the binary at ./result/bin/smeltry-operator
FROM scratch
COPY smeltry-operator /smeltry-operator
ENTRYPOINT ["/smeltry-operator"]
