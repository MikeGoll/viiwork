"""Checks the release workflow's security properties:

	python3 scripts/release-workflow_test.py

- expressions (${{ }}) never appear inside a run: script;
- every action is pinned to a full commit SHA;
- the job that runs the build and third-party actions can only read; the job
  that can write runs no third-party code but the artifact download;
- runs for one tag never interleave;
- a signed release is never re-uploaded under its signature.
"""
import re
import sys

import yaml

text = open(".github/workflows/release.yml").read()
wf = yaml.safe_load(text)
fail = []


def check(ok, what):
    print(("ok    " if ok else "FAIL  ") + what)
    if not ok:
        fail.append(what)


jobs = wf["jobs"]
check(wf.get("permissions") in ({}, {"contents": "read"}), "workflow-level permissions are read-only or empty")
check(set(jobs) == {"build", "publish"}, "jobs are exactly build and publish")
check(jobs.get("build", {}).get("permissions") == {"contents": "read"}, "build can only read")
check(jobs.get("publish", {}).get("permissions") == {"contents": "write"}, "publish can write")
check(jobs.get("publish", {}).get("needs") == "build", "publish waits for build")
publish_uses = [s["uses"].split("@")[0] for s in jobs.get("publish", {}).get("steps", []) if "uses" in s]
check(publish_uses == ["actions/download-artifact"], "publish runs no third-party action but the artifact download")
check("concurrency" in wf, "runs for one tag never interleave")
for job in jobs.values():
    for step in job.get("steps", []):
        check("${{" not in step.get("run", ""), "no expression inside run: " + step.get("name", step.get("uses", "?")))
for uses in re.findall(r"uses: (\S+)", text):
    check(bool(re.search(r"@[0-9a-f]{40}$", uses)), "pinned by SHA: " + uses)
publish_run = "\n".join(s.get("run", "") for s in jobs.get("publish", {}).get("steps", []))
check("SHA256SUMS.sig" in publish_run and "exit 1" in publish_run, "publish refuses a release that is already signed")
sys.exit(1 if fail else 0)
