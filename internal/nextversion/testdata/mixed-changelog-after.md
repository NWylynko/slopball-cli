# CHANGELOG

The header this fixture keeps, to prove the newest release lands under it and
above every older one.

## v1.0.0

### Breaking — an already-installed client stops working

- floor v1.4.0 — An old client is refused: the control plane no longer reads overlayAddr from a claim.

### Additive — an already-installed client is unaffected

- An old client keeps working: the control plane accepts a member row that carries no memberRole.

### Patch — nothing an already-installed client can observe changed

- An old client sees no difference: the field it never decoded was renamed.

## v0.1.0

- The first tag.
