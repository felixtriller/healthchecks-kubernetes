#!/usr/bin/env python3
"""Check chart labels with Flux's OCI digest suffix in both RBAC scopes."""

import json
import re
import shutil
import subprocess
import tempfile
from pathlib import Path


chart = Path(__file__).resolve().parent.parent / "charts/healthchecks-kubernetes"
metadata = (chart / "Chart.yaml").read_text()
version = re.search(r"^version: (.+)$", metadata, re.MULTILINE).group(1)
valid_label = re.compile(r"[A-Za-z0-9](?:[-A-Za-z0-9_.]*[A-Za-z0-9])?")

with tempfile.TemporaryDirectory(prefix="healthchecks-chart-labels-") as directory:
    staged = Path(directory) / chart.name
    shutil.copytree(chart, staged)
    for test_version in (version, version + "+2f864937c6fa", version + "+" + "a" * 64):
        (staged / "Chart.yaml").write_text(
            re.sub(r"^version: .+$", "version: " + test_version, metadata, flags=re.MULTILINE)
        )
        for namespace in ("", "jobs"):
            result = subprocess.run(
                ["helm", "template", "healthchecks", str(staged),
                 "--set", "config.cluster=ci", "--set", "credentials.existingSecret=healthchecks",
                 "--set", "config.namespace=" + namespace],
                check=True, capture_output=True, text=True,
            )
            labels = [json.loads(line.split(":", 1)[1].strip())
                      for line in result.stdout.splitlines() if line.strip().startswith("helm.sh/chart:")]
            assert labels, "No chart labels were rendered"
            for label in labels:
                assert len(label) <= 63 and valid_label.fullmatch(label), (
                    f"Invalid chart label for version {test_version}: {label}"
                )

print("Chart labels are valid with OCI digest metadata in both RBAC scopes.")
