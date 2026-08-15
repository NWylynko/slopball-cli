# Security

Report a vulnerability to **nick@wylynko.com**. Say what you found, how to
reproduce it, and which version (`slopball --version`) — you will get a reply
from a person, and a fix lands on `main` and in the next release rather than in
a private branch: this project fixes forward.

Please don't open a public issue for something exploitable until it is fixed.
Everything else — a hardening idea, a question about what slopball does with
your data — is fine as an issue.

Before reporting, read [`TRUST.md`](./TRUST.md): several things that look like
findings (the relay carries your traffic; a managed box holds your code in
clear; telemetry records credentials verbatim) are documented behaviour with the
reasoning written down, and the report we want is "this claim is false", not
"this claim is alarming".
