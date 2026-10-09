"""Write an allowlisted test matrix to the GitHub Actions job summary."""

import os
import re
from pathlib import Path

log = Path("live-test.log")
text = log.read_text() if log.exists() else ""
results = {
    (configuration, client): result
    for result, configuration, client in re.findall(
        r"--- (PASS|FAIL|SKIP): TestClientAuthentication/(generated|corrected)/(trusted_after|trusted|none|unrelated)\s",
        text,
    )
}
labels = {"PASS": "Passed", "FAIL": "Failed", "SKIP": "Skipped"}
lines = [
    "## RLS client certificate authentication",
    "",
    "Tests Envoy Gateway **v1.9.1** with RLS **8fe6ea42** in two disposable k3d clusters.",
    "The generated configuration supplies `GRPC_SERVER_TLS_CA_CERT`; the corrected",
    "configuration adds `GRPC_CLIENT_TLS_CACERT=/certs/ca.crt`, which this RLS revision recognizes.",
    "",
    "**A passing baseline reproduces the missing client authentication.** A passing",
    "corrected case confirms that missing/untrusted certificates are rejected while trusted clients still reach RLS.",
    "",
    "| Configuration | Client | Expected behavior | Actual test result |",
    "| --- | --- | --- | --- |",
]
for configuration in ("generated", "corrected"):
    for client, label in (
        ("trusted", "EG-issued certificate"),
        ("none", "No certificate"),
        ("unrelated", "Certificate signed by unrelated CA"),
        ("trusted_after", "EG-issued certificate after rejection probes"),
    ):
        expected = "RPC reaches RLS"
        if configuration == "corrected" and client == "none":
            expected = "TLS handshake rejected"
        elif configuration == "corrected" and client == "unrelated":
            expected = "Untrusted certificate rejected"
        outcome = labels.get(results.get((configuration, client)), "Not run")
        lines.append(f"| {configuration} | {label} | {expected} | {outcome} |")
lines += [
    "",
    "Each probe uses a fresh TLS 1.2 connection and private port-forward. Server CA",
    "and hostname verification remain enabled. An accepted RPC returns the exact",
    "application error `Unknown: no rate limit configuration loaded`; this proves",
    "the call reached RLS. TLS alerts prove rejection. EOF, timeouts, and connection",
    "failures do not count as successful rejection tests.",
    "",
    "This tests **client authentication**, not quota accounting or HTTP traffic through Envoy.",
    "",
    f"- Live test step: **{os.environ.get('PROBE_OUTCOME', 'unknown')}**",
    f"- Cleanup step: **{os.environ.get('CLEANUP_OUTCOME', 'unknown')}**",
    "- The `live-result` artifact contains the verbose test log and cleanup evidence.",
    "- `Not run` means no completed subtest result was recorded, including setup failures.",
    "",
]
rendered = "\n".join(lines)
Path("summary.md").write_text(rendered)
with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
    summary.write(rendered)
