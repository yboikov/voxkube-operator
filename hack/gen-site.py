#!/usr/bin/env python3
"""Build the GitHub Pages site into an output directory.

Copies site/ and README.md, and turns the CRDs in config/crd/bases into data/crds.json for the API explorer,
so the published API reference always matches the CRDs of the commit it was built from.

Usage: hack/gen-site.py [OUTPUT_DIR]   (default: _site)
"""
import json
import pathlib
import shutil
import subprocess
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parent.parent
KIND_ORDER = ["VoxCluster", "VoxNode", "VoxDiscovery"]


def load_crds():
    kinds = []
    for path in sorted((ROOT / "config" / "crd" / "bases").glob("*.yaml")):
        for doc in yaml.safe_load_all(path.read_text()):
            if not doc or doc.get("kind") != "CustomResourceDefinition":
                continue
            spec = doc["spec"]
            for ver in spec["versions"]:
                if not ver.get("served", True):
                    continue
                kinds.append({
                    "kind": spec["names"]["kind"],
                    "group": spec["group"],
                    "version": ver["name"],
                    "plural": spec["names"]["plural"],
                    "shortNames": spec["names"].get("shortNames", []),
                    "scope": spec["scope"],
                    "printerColumns": ver.get("additionalPrinterColumns", []),
                    "schema": ver["schema"]["openAPIV3Schema"],
                })
    kinds.sort(key=lambda k: KIND_ORDER.index(k["kind"]) if k["kind"] in KIND_ORDER else 99)
    return kinds


def chart_version():
    for line in (ROOT / "charts" / "voxkube-operator" / "Chart.yaml").read_text().splitlines():
        if line.startswith("version:"):
            return line.split(":", 1)[1].strip().strip('"')
    return ""


def main():
    out = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "_site").resolve()
    if out.exists():
        shutil.rmtree(out)
    shutil.copytree(ROOT / "site", out)
    shutil.copy(ROOT / "README.md", out / "README.md")
    try:
        commit = subprocess.run(["git", "rev-parse", "--short=7", "HEAD"], cwd=ROOT, capture_output=True,
                                text=True, check=True).stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        commit = ""
    data = {"chartVersion": chart_version(), "commit": commit, "kinds": load_crds()}
    (out / "data").mkdir(exist_ok=True)
    (out / "data" / "crds.json").write_text(json.dumps(data, separators=(",", ":")))
    print(f"site built in {out}: {len(data['kinds'])} kinds, chart {data['chartVersion']}, commit {commit}")


if __name__ == "__main__":
    main()
