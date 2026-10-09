# Release public keys

Every `*.pub` file here is an ed25519 public key that viiwork trusts to sign
releases (`SHA256SUMS.sig`). They are compiled in. More than one file means a
rotation is in progress: a release signed by any of them verifies.

The private halves never enter this repository. `viiwork-release keygen` makes a
pair on the publisher's machine and writes the public half here; see
`docs/releases.md`.
